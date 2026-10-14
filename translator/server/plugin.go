package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	mmplugin "github.com/mattermost/mattermost/server/public/plugin"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/automatic"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/gemini"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/limit"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/manual"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/store"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/worker"
)

type commandRegistrar interface {
	RegisterCommand(command *model.Command) error
}

type automaticRuntime interface {
	worker.Processor
	SetActivationAt(activationAt int64)
	ReconcileMappings(context.Context) error
	HandlePosted(post *model.Post) error
	HandleUpdated(post *model.Post) error
	HandleDeleted(post *model.Post) error
}

type manualRuntime interface {
	Execute(context.Context, *model.CommandArgs, []string) (*model.CommandResponse, error)
	Shutdown()
}

type workerRuntime interface {
	Start(context.Context)
	Stop()
	Wake()
}

type translationRuntime struct {
	context    context.Context
	cancel     context.CancelFunc
	repository *store.Repository
	gate       *limit.Gate
	automatic  automaticRuntime
	manual     manualRuntime
	workers    workerRuntime
	stopOnce   sync.Once
}

func (r *translationRuntime) setActivationAt(activationAt int64) {
	if r != nil && r.automatic != nil {
		r.automatic.SetActivationAt(activationAt)
	}
}

func (r *translationRuntime) start() error {
	if r == nil || r.automatic == nil || r.manual == nil || r.workers == nil {
		return errors.New("translator runtime is incomplete")
	}
	if err := r.automatic.ReconcileMappings(r.context); err != nil {
		return fmt.Errorf("automatic mapping reconciliation failed: %w", err)
	}
	r.workers.Start(r.context)
	return nil
}

func (r *translationRuntime) stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		if r.manual != nil {
			r.manual.Shutdown()
		}
		if r.workers != nil {
			r.workers.Stop()
		}
	})
}

type runtimeFactory func(context.Context, *runtimeConfiguration) (*translationRuntime, error)

type translationClient interface {
	automatic.KoreanTranslator
	manual.Translator
}

type runtimeConstructors struct {
	newAutomatic func(
		scope *automatic.Scope,
		repository *store.Repository,
		api publication.PublicationAPI,
		translator automatic.KoreanTranslator,
		waker automatic.Waker,
		config automatic.ServiceConfig,
	) *automatic.Service
	newManual func(
		parent context.Context,
		api manual.API,
		translator manual.Translator,
		gate *limit.Gate,
		config manual.ServiceConfig,
	) *manual.Service
	newWorker func(
		repository worker.Repository,
		processor worker.Processor,
		gate *limit.Gate,
		concurrency int,
		model string,
		logger worker.Logger,
	) *worker.Pool
}

func defaultRuntimeConstructors() runtimeConstructors {
	return runtimeConstructors{
		newAutomatic: automatic.NewService,
		newManual:    manual.NewService,
		newWorker:    worker.New,
	}
}

type runtimeWaker struct {
	mu     sync.RWMutex
	target workerRuntime
}

func (w *runtimeWaker) Wake() {
	w.mu.RLock()
	target := w.target
	w.mu.RUnlock()
	if target != nil {
		target.Wake()
	}
}

func (w *runtimeWaker) set(target workerRuntime) {
	w.mu.Lock()
	w.target = target
	w.mu.Unlock()
}

func assembleRuntime(
	parent context.Context,
	configuration *runtimeConfiguration,
	adapter *mattermostAdapter,
	logger worker.Logger,
	repository *store.Repository,
	gate *limit.Gate,
	client translationClient,
	constructors runtimeConstructors,
) *translationRuntime {
	if parent == nil {
		parent = context.Background()
	}
	runtimeContext, cancel := context.WithCancel(parent)
	waker := &runtimeWaker{}
	scope := automatic.NewScope(
		adapter,
		configuration.automaticTeamID,
		configuration.automaticSourceUserIDs,
		configuration.translatorBotUserID,
		configuration.includeDirectMessages,
	)
	automaticService := constructors.newAutomatic(
		scope,
		repository,
		adapter,
		client,
		waker,
		automatic.ServiceConfig{
			ActivationAt:     configuration.activationAt,
			TranslatorUserID: configuration.translatorBotUserID,
			SiteURL:          configuration.siteURL,
			ModelID:          configuration.primaryModel,
			Glossary:         configuration.glossary,
		},
	)
	workerPool := constructors.newWorker(
		repository,
		automaticService,
		gate,
		configuration.maximumConcurrentGeminiRequests,
		configuration.primaryModel,
		logger,
	)
	waker.set(workerPool)
	manualService := constructors.newManual(
		runtimeContext,
		adapter,
		client,
		gate,
		manual.ServiceConfig{
			TranslatorUserID: configuration.translatorBotUserID,
			Glossary:         configuration.glossary,
		},
	)
	return &translationRuntime{
		context:    runtimeContext,
		cancel:     cancel,
		repository: repository,
		gate:       gate,
		automatic:  automaticService,
		manual:     manualService,
		workers:    workerPool,
	}
}

func newTranslatorRuntime(
	parent context.Context,
	configuration *runtimeConfiguration,
	api mattermostAPI,
	logger *safeLogger,
) (*translationRuntime, error) {
	if configuration == nil || api == nil {
		return nil, errors.New("translator runtime dependencies are unavailable")
	}
	serverConfiguration := api.GetConfig()
	if serverConfiguration == nil || serverConfiguration.ServiceSettings.SiteURL == nil ||
		strings.TrimSpace(*serverConfiguration.ServiceSettings.SiteURL) == "" {
		return nil, errors.New("Mattermost SiteURL is unavailable")
	}
	configured := cloneRuntimeConfiguration(configuration)
	configured.siteURL = strings.TrimRight(strings.TrimSpace(*serverConfiguration.ServiceSettings.SiteURL), "/")

	client, err := gemini.NewClient(parent, configured.geminiAPIKey, configured.primaryModel, configured.fallbackModel)
	if err != nil {
		return nil, errors.New("Gemini runtime client could not be created")
	}
	adapter := newMattermostAdapter(api)
	repository := store.NewRepository(adapter)
	gate := limit.New(configured.maximumConcurrentGeminiRequests)
	return assembleRuntime(
		parent,
		configured,
		adapter,
		logger,
		repository,
		gate,
		client,
		defaultRuntimeConstructors(),
	), nil
}

type Plugin struct {
	mmplugin.MattermostPlugin

	configurationMu         sync.RWMutex
	configuration           *runtimeConfiguration
	configurationGeneration uint64
	configurationLoader     configurationLoader
	configurationAPI        ConfigurationAPI
	modelValidator          ModelValidator

	lifecycleMu      sync.Mutex
	runtimeMu        sync.RWMutex
	runtime          *translationRuntime
	runtimeFactory   runtimeFactory
	commandRegistrar commandRegistrar
	rootContext      context.Context
	rootCancel       context.CancelFunc
	active           bool
	activationAt     int64
	now              func() time.Time
	logger           *safeLogger
}

func (p *Plugin) OnConfigurationChange() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.ensureDependenciesLocked()
	root := p.ensureRootLocked()

	configuration, err := p.loadAndValidateConfiguration(root)
	if err != nil {
		p.disableRuntimeLocked()
		p.logRuntimeFailure(RuntimeFailureMetadata{Operation: LogOperationConfiguration, Category: "invalid"})
		return err
	}
	configuration.activationAt = p.activationAt
	if p.runtimeFactory == nil {
		p.clearConfiguration()
		p.disableRuntimeLocked()
		return errors.New("translator runtime factory is unavailable")
	}
	replacement, err := p.runtimeFactory(root, configuration)
	if err != nil || replacement == nil {
		p.clearConfiguration()
		p.disableRuntimeLocked()
		p.logRuntimeFailure(RuntimeFailureMetadata{Operation: LogOperationConfiguration, Category: "runtime"})
		return errors.New("translator runtime could not be built")
	}

	p.runtimeMu.Lock()
	defer p.runtimeMu.Unlock()
	if p.runtime != nil {
		p.runtime.stop()
	}
	p.runtime = replacement
	if p.active {
		if err := replacement.start(); err != nil {
			replacement.stop()
			p.runtime = nil
			p.clearConfiguration()
			p.logRuntimeFailure(RuntimeFailureMetadata{Operation: LogOperationConfiguration, Category: "reconcile"})
			return errors.New("translator replacement runtime could not start")
		}
	}
	return nil
}

func (p *Plugin) OnActivate() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.ensureDependenciesLocked()
	root := p.ensureRootLocked()
	if !p.active && p.activationAt == 0 {
		p.activationAt = p.lifecycleNow().UnixMilli()
	}

	if p.currentRuntime() == nil {
		configuration, err := p.loadAndValidateConfiguration(root)
		if err != nil {
			p.activationAt = 0
			p.disableRuntimeLocked()
			p.logRuntimeFailure(RuntimeFailureMetadata{Operation: LogOperationActivation, Category: "invalid"})
			return err
		}
		configuration.activationAt = p.activationAt
		if p.runtimeFactory == nil {
			p.activationAt = 0
			p.clearConfiguration()
			return errors.New("translator runtime factory is unavailable")
		}
		replacement, err := p.runtimeFactory(root, configuration)
		if err != nil || replacement == nil {
			p.activationAt = 0
			p.clearConfiguration()
			p.logRuntimeFailure(RuntimeFailureMetadata{Operation: LogOperationActivation, Category: "runtime"})
			return errors.New("translator runtime could not be built")
		}
		p.runtimeMu.Lock()
		p.runtime = replacement
		p.runtimeMu.Unlock()
	}

	p.runtimeMu.Lock()
	defer p.runtimeMu.Unlock()
	if p.runtime == nil {
		p.activationAt = 0
		return errors.New("translator configuration is invalid")
	}
	p.runtime.setActivationAt(p.activationAt)
	if p.commandRegistrar == nil {
		p.runtime.stop()
		p.runtime = nil
		p.activationAt = 0
		p.clearConfiguration()
		return errors.New("translator command registrar is unavailable")
	}
	if err := p.commandRegistrar.RegisterCommand(translatorCommand()); err != nil {
		p.runtime.stop()
		p.runtime = nil
		p.activationAt = 0
		p.clearConfiguration()
		p.logRuntimeFailure(RuntimeFailureMetadata{Operation: LogOperationActivation, Category: "command"})
		return errors.New("translator command registration failed")
	}
	if err := p.runtime.start(); err != nil {
		p.runtime.stop()
		p.runtime = nil
		p.activationAt = 0
		p.clearConfiguration()
		p.logRuntimeFailure(RuntimeFailureMetadata{Operation: LogOperationActivation, Category: "reconcile"})
		return err
	}
	p.active = true
	return nil
}

func (p *Plugin) OnDeactivate() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.active = false
	if p.rootCancel != nil {
		p.rootCancel()
	}
	p.runtimeMu.Lock()
	if p.runtime != nil {
		p.runtime.stop()
		p.runtime = nil
	}
	p.runtimeMu.Unlock()
	p.rootContext = nil
	p.rootCancel = nil
	p.activationAt = 0
	return nil
}

func (p *Plugin) MessageHasBeenPosted(_ *mmplugin.Context, post *model.Post) {
	if translatorGenerated(post) {
		return
	}
	p.runtimeMu.RLock()
	defer p.runtimeMu.RUnlock()
	if p.runtime == nil || p.runtime.automatic == nil {
		return
	}
	if err := p.runtime.automatic.HandlePosted(post); err != nil {
		p.logRuntimeFailure(postFailureMetadata(LogOperationPostedHook, post))
	}
}

func (p *Plugin) MessageHasBeenUpdated(_ *mmplugin.Context, newPost, oldPost *model.Post) {
	if translatorGenerated(newPost) {
		return
	}
	if oldPost != nil {
		newSource, newOwned := publication.StripInlineTranslation(newPost)
		oldSource, _ := publication.StripInlineTranslation(oldPost)
		if newOwned && newSource == oldSource {
			return
		}
	}
	p.runtimeMu.RLock()
	defer p.runtimeMu.RUnlock()
	if p.runtime == nil || p.runtime.automatic == nil {
		return
	}
	if err := p.runtime.automatic.HandleUpdated(newPost); err != nil {
		p.logRuntimeFailure(postFailureMetadata(LogOperationUpdatedHook, newPost))
	}
}

func (p *Plugin) MessageHasBeenDeleted(_ *mmplugin.Context, post *model.Post) {
	p.runtimeMu.RLock()
	defer p.runtimeMu.RUnlock()
	if p.runtime == nil || p.runtime.automatic == nil {
		return
	}
	if err := p.runtime.automatic.HandleDeleted(post); err != nil {
		p.logRuntimeFailure(postFailureMetadata(LogOperationDeletedHook, post))
	}
}

func (p *Plugin) ExecuteCommand(_ *mmplugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	return p.executeManual(args, nil), nil
}

// ServeHTTP serves POST /trans, which the webapp uses instead of the slash
// command when the draft has attachments: CommandArgs cannot carry file IDs.
func (p *Plugin) ServeHTTP(_ *mmplugin.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/trans" {
		http.NotFound(w, r)
		return
	}
	userID := r.Header.Get("Mattermost-User-Id")
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var request struct {
		ChannelID string   `json:"channel_id"`
		RootID    string   `json:"root_id"`
		Command   string   `json:"command"`
		FileIDs   []string `json:"file_ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil || request.ChannelID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// The plugin posts as the caller, so this replaces Mattermost's own
	// slash-command channel check.
	if !p.API.HasPermissionToChannel(userID, request.ChannelID, model.PermissionCreatePost) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	response := p.executeManual(&model.CommandArgs{
		UserId: userID, ChannelId: request.ChannelID, RootId: request.RootID, Command: request.Command,
	}, request.FileIDs)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"text": response.Text})
}

func (p *Plugin) executeManual(args *model.CommandArgs, fileIDs []string) *model.CommandResponse {
	p.runtimeMu.RLock()
	defer p.runtimeMu.RUnlock()
	if p.runtime == nil || p.runtime.manual == nil {
		return unavailableCommandResponse()
	}
	response, err := p.runtime.manual.Execute(p.runtime.context, args, fileIDs)
	if err != nil {
		p.logRuntimeFailure(RuntimeFailureMetadata{Operation: LogOperationCommand, Category: "internal"})
		return unavailableCommandResponse()
	}
	if response == nil {
		response = &model.CommandResponse{}
	}
	response.ResponseType = model.CommandResponseTypeEphemeral
	return response
}

func (p *Plugin) currentRuntime() *translationRuntime {
	p.runtimeMu.RLock()
	defer p.runtimeMu.RUnlock()
	return p.runtime
}

func (p *Plugin) ensureDependenciesLocked() {
	if p.API != nil {
		api := newMattermostAdapter(p.API)
		if p.configurationLoader == nil {
			p.configurationLoader = api
		}
		if p.configurationAPI == nil {
			p.configurationAPI = api
		}
		if p.commandRegistrar == nil {
			p.commandRegistrar = api
		}
		if p.logger == nil {
			p.logger = newSafeLogger(p.API)
		}
		if p.runtimeFactory == nil {
			rawAPI := p.API
			p.runtimeFactory = func(ctx context.Context, configuration *runtimeConfiguration) (*translationRuntime, error) {
				return newTranslatorRuntime(ctx, configuration, rawAPI, p.logger)
			}
		}
	}
	if p.modelValidator == nil {
		p.modelValidator = &gemini.Client{}
	}
}

func (p *Plugin) ensureRootLocked() context.Context {
	if p.rootContext == nil || p.rootContext.Err() != nil {
		p.rootContext, p.rootCancel = context.WithCancel(context.Background())
	}
	return p.rootContext
}

func (p *Plugin) lifecycleNow() time.Time {
	if p.now != nil {
		return p.now().UTC()
	}
	return time.Now().UTC()
}

func (p *Plugin) disableRuntimeLocked() {
	p.runtimeMu.Lock()
	if p.runtime != nil {
		p.runtime.stop()
		p.runtime = nil
	}
	p.runtimeMu.Unlock()
}

func (p *Plugin) clearConfiguration() {
	generation := p.beginConfigurationLoad()
	p.replaceConfigurationIfCurrent(generation, nil)
}

func (p *Plugin) logRuntimeFailure(metadata RuntimeFailureMetadata) {
	if p.logger != nil {
		p.logger.LogRuntimeFailure(metadata)
	}
}

func postFailureMetadata(operation LogOperation, post *model.Post) RuntimeFailureMetadata {
	metadata := RuntimeFailureMetadata{Operation: operation, Category: "internal"}
	if post != nil {
		metadata.SourcePostID = post.Id
		metadata.ChannelID = post.ChannelId
		if operationID, _ := post.GetProp(publication.TranslatorOperationIDProp).(string); operationID != "" {
			metadata.CorrelationID = operationID
		}
	}
	return metadata
}

func translatorGenerated(post *model.Post) bool {
	if post == nil {
		return false
	}
	for _, key := range []string{
		publication.TranslatorKindProp,
		publication.TranslatorOperationIDProp,
		publication.TranslatorSourcePostIDProp,
		publication.TranslatorSourceUpdateAtProp,
		publication.TranslatorModelProp,
	} {
		if post.GetProp(key) != nil {
			return true
		}
	}
	return false
}

func translatorCommand() *model.Command {
	return &model.Command{
		Trigger:          "trans",
		AutoComplete:     true,
		AutoCompleteDesc: "한국어 메시지를 영어로 번역해 원문과 함께 게시합니다.",
		AutoCompleteHint: "<한국어 메시지>",
	}
}

func unavailableCommandResponse() *model.CommandResponse {
	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         "번역 기능을 사용할 수 없습니다. 잠시 후 다시 시도해 주세요.",
	}
}

var _ mattermostAPI = mmplugin.API(nil)
