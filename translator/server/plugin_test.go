package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	mmplugin "github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/require"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/automatic"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/limit"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/manual"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/store"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/worker"
)

type lifecycleEvents struct {
	mu     sync.Mutex
	values []string
}

func (e *lifecycleEvents) add(value string) {
	e.mu.Lock()
	e.values = append(e.values, value)
	e.mu.Unlock()
}

func (e *lifecycleEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.values...)
}

type fakeAutomaticRuntime struct {
	events       *lifecycleEvents
	reconcileErr error
	posted       chan *model.Post
	updated      chan *model.Post
	deleted      chan *model.Post
}

func (r *fakeAutomaticRuntime) ReconcileMappings(context.Context) error {
	r.events.add("reconcile-mappings")
	return r.reconcileErr
}
func (r *fakeAutomaticRuntime) HandlePosted(post *model.Post) error {
	r.posted <- post
	return nil
}
func (r *fakeAutomaticRuntime) HandleUpdated(post *model.Post) error {
	r.updated <- post
	return nil
}
func (r *fakeAutomaticRuntime) HandleDeleted(post *model.Post) error {
	r.deleted <- post
	return nil
}
func (r *fakeAutomaticRuntime) ProcessJob(context.Context, domain.AutomaticJob) error { return nil }
func (r *fakeAutomaticRuntime) SetActivationAt(int64)                                 {}

type cutoffAutomaticRuntime struct {
	*fakeAutomaticRuntime
	activationAt int64
	accepted     chan *model.Post
}

func (r *cutoffAutomaticRuntime) SetActivationAt(activationAt int64) {
	r.activationAt = activationAt
}

func (r *cutoffAutomaticRuntime) HandlePosted(post *model.Post) error {
	if post != nil && post.CreateAt > r.activationAt {
		r.accepted <- post
	}
	return nil
}

type fakeManualRuntime struct {
	events      *lifecycleEvents
	shutdown    chan struct{}
	stopEntered chan struct{}
	stopRelease <-chan struct{}
	args        *model.CommandArgs
	fileIDs     []string
}

func (r *fakeManualRuntime) Execute(_ context.Context, args *model.CommandArgs, fileIDs []string) (*model.CommandResponse, error) {
	r.args, r.fileIDs = args, fileIDs
	return &model.CommandResponse{ResponseType: model.CommandResponseTypeEphemeral, Text: "private"}, nil
}
func (r *fakeManualRuntime) Shutdown() {
	r.events.add("manual-stop")
	if r.stopEntered != nil {
		close(r.stopEntered)
	}
	if r.stopRelease != nil {
		<-r.stopRelease
	}
	if r.shutdown != nil {
		close(r.shutdown)
	}
}

type fakeWorkerRuntime struct{ events *lifecycleEvents }

func (r *fakeWorkerRuntime) Start(context.Context) { r.events.add("workers-start") }
func (r *fakeWorkerRuntime) Stop()                 { r.events.add("workers-stop") }
func (r *fakeWorkerRuntime) Wake()                 {}

type fakeCommandRegistrar struct {
	events  *lifecycleEvents
	command *model.Command
	err     error
}

func (r *fakeCommandRegistrar) RegisterCommand(command *model.Command) error {
	r.events.add("register-command")
	r.command = command
	return r.err
}

func newLifecycleRuntime(ctx context.Context, events *lifecycleEvents) *translationRuntime {
	runtimeContext, cancel := context.WithCancel(ctx)
	return &translationRuntime{
		context: runtimeContext,
		cancel:  cancel,
		automatic: &fakeAutomaticRuntime{
			events: events, posted: make(chan *model.Post, 4), updated: make(chan *model.Post, 4), deleted: make(chan *model.Post, 4),
		},
		manual:  &fakeManualRuntime{events: events},
		workers: &fakeWorkerRuntime{events: events},
	}
}

func newLifecyclePlugin(events *lifecycleEvents, factory runtimeFactory) (*Plugin, *fakeCommandRegistrar) {
	registrar := &fakeCommandRegistrar{events: events}
	return &Plugin{
		configurationLoader: configurationLoaderStub{configuration: validSystemConsoleConfiguration()},
		configurationAPI:    validConfigurationAPI(),
		modelValidator:      &modelValidatorStub{},
		runtimeFactory:      factory,
		commandRegistrar:    registrar,
	}, registrar
}

func TestActivationRegistersMobileCompatibleCommandThenReconcilesBeforeWorkers(t *testing.T) {
	events := &lifecycleEvents{}
	plugin, registrar := newLifecyclePlugin(events, func(ctx context.Context, _ *runtimeConfiguration) (*translationRuntime, error) {
		return newLifecycleRuntime(ctx, events), nil
	})

	require.NoError(t, plugin.OnConfigurationChange())
	require.Empty(t, events.snapshot())
	require.NoError(t, plugin.OnActivate())
	require.Equal(t, []string{
		"register-command", "reconcile-mappings", "workers-start",
	}, events.snapshot())
	require.NotNil(t, registrar.command)
	require.Equal(t, "trans", registrar.command.Trigger)
	require.True(t, registrar.command.AutoComplete)
	require.NotContains(t, registrar.command.AutoCompleteHint, "@username")
	require.Contains(t, registrar.command.AutoCompleteHint, "<한국어 메시지>")
}

func TestActivationLoadsValidatedConfigurationAndBuildsRuntimeWhenCallbackWasNotPrecalled(t *testing.T) {
	events := &lifecycleEvents{}
	built := 0
	plugin, _ := newLifecyclePlugin(events, func(ctx context.Context, _ *runtimeConfiguration) (*translationRuntime, error) {
		built++
		events.add("build-runtime")
		return newLifecycleRuntime(ctx, events), nil
	})

	require.NoError(t, plugin.OnActivate())
	require.Equal(t, 1, built)
	require.Equal(t, []string{
		"build-runtime", "register-command", "reconcile-mappings", "workers-start",
	}, events.snapshot())
	_, configured := plugin.getConfiguration()
	require.True(t, configured)
}

func TestReactivationRebuildsFromSavedSettingsWithoutConfigurationCallback(t *testing.T) {
	events := &lifecycleEvents{}
	built := 0
	plugin, _ := newLifecyclePlugin(events, func(ctx context.Context, _ *runtimeConfiguration) (*translationRuntime, error) {
		built++
		return newLifecycleRuntime(ctx, events), nil
	})

	require.NoError(t, plugin.OnActivate())
	require.NoError(t, plugin.OnDeactivate())
	require.NoError(t, plugin.OnActivate())
	require.Equal(t, 2, built)
	require.Equal(t, []string{
		"register-command", "reconcile-mappings", "workers-start",
		"manual-stop", "workers-stop",
		"register-command", "reconcile-mappings", "workers-start",
	}, events.snapshot())
}

func TestActivationFailsClosedWhenTranslatorBotIsUnsafe(t *testing.T) {
	events := &lifecycleEvents{}
	plugin, _ := newLifecyclePlugin(events, func(ctx context.Context, _ *runtimeConfiguration) (*translationRuntime, error) {
		return newLifecycleRuntime(ctx, events), nil
	})
	plugin.configurationAPI.(*configurationAPIStub).usersByID["user-translator"].Roles = "system_user system_admin"

	require.Error(t, plugin.OnConfigurationChange())
	require.Error(t, plugin.OnActivate())
	require.Nil(t, plugin.currentRuntime())
	_, configured := plugin.getConfiguration()
	require.False(t, configured)
}

func TestActivationReconciliationFailureStopsRuntimeAndClearsConfiguration(t *testing.T) {
	events := &lifecycleEvents{}
	plugin, _ := newLifecyclePlugin(events, func(ctx context.Context, _ *runtimeConfiguration) (*translationRuntime, error) {
		runtime := newLifecycleRuntime(ctx, events)
		runtime.automatic.(*fakeAutomaticRuntime).reconcileErr = errors.New("mapping recovery failed")
		return runtime, nil
	})

	require.EqualError(t, plugin.OnActivate(), "automatic mapping reconciliation failed: mapping recovery failed")
	require.Nil(t, plugin.currentRuntime())
	_, configured := plugin.getConfiguration()
	require.False(t, configured)
	require.Equal(t, []string{
		"register-command", "reconcile-mappings", "manual-stop", "workers-stop",
	}, events.snapshot())
}

func TestConfigurationReloadDrainsOldRuntimeBeforeStartingReplacement(t *testing.T) {
	events := &lifecycleEvents{}
	release := make(chan struct{})
	entered := make(chan struct{})
	build := 0
	plugin, _ := newLifecyclePlugin(events, func(ctx context.Context, _ *runtimeConfiguration) (*translationRuntime, error) {
		build++
		runtime := newLifecycleRuntime(ctx, events)
		if build == 1 {
			runtime.manual = &fakeManualRuntime{events: events, stopEntered: entered, stopRelease: release}
		}
		return runtime, nil
	})
	require.NoError(t, plugin.OnConfigurationChange())
	require.NoError(t, plugin.OnActivate())

	done := make(chan error, 1)
	go func() { done <- plugin.OnConfigurationChange() }()
	<-entered
	require.NotContains(t, events.snapshot(), "workers-stop", "runtime Stop must wait for admitted manual work")
	close(release)
	require.NoError(t, <-done)
	require.Equal(t, []string{
		"register-command", "reconcile-mappings", "workers-start",
		"manual-stop", "workers-stop", "reconcile-mappings", "workers-start",
	}, events.snapshot())
}

func TestConfigurationReloadPreservesActivationCutoffForDelayedPostHook(t *testing.T) {
	events := &lifecycleEvents{}
	activationTime := time.UnixMilli(1_000)
	cutoffs := make([]int64, 0, 3)
	accepted := make(chan *model.Post, 1)
	plugin, _ := newLifecyclePlugin(events, func(ctx context.Context, configuration *runtimeConfiguration) (*translationRuntime, error) {
		cutoffs = append(cutoffs, configuration.activationAt)
		runtime := newLifecycleRuntime(ctx, events)
		runtime.automatic = &cutoffAutomaticRuntime{
			fakeAutomaticRuntime: runtime.automatic.(*fakeAutomaticRuntime),
			activationAt:         configuration.activationAt,
			accepted:             accepted,
		}
		return runtime, nil
	})
	plugin.now = func() time.Time { return activationTime }

	require.NoError(t, plugin.OnConfigurationChange(), "an inactive configuration build has no activation cutoff yet")
	require.Equal(t, []int64{0}, cutoffs)
	require.NoError(t, plugin.OnActivate())

	activationTime = time.UnixMilli(2_000)
	require.NoError(t, plugin.OnConfigurationChange())
	require.Equal(t, []int64{0, 1_000}, cutoffs, "active reload must reuse the original cutoff")

	delayed := &model.Post{Id: "delayed", UserId: "alex", ChannelId: "channel", CreateAt: 1_500}
	plugin.MessageHasBeenPosted(&mmplugin.Context{}, delayed)
	require.Same(t, delayed, <-accepted, "a hook delayed until after reload must retain activation eligibility")

	require.NoError(t, plugin.OnDeactivate())
	activationTime = time.UnixMilli(3_000)
	require.NoError(t, plugin.OnActivate())
	require.Equal(t, int64(3_000), cutoffs[len(cutoffs)-1], "genuine reactivation must establish a fresh cutoff")
}

func TestConfigurationFailureStopsAndClearsActiveRuntime(t *testing.T) {
	events := &lifecycleEvents{}
	plugin, _ := newLifecyclePlugin(events, func(ctx context.Context, _ *runtimeConfiguration) (*translationRuntime, error) {
		return newLifecycleRuntime(ctx, events), nil
	})
	require.NoError(t, plugin.OnConfigurationChange())
	require.NoError(t, plugin.OnActivate())
	plugin.configurationLoader = configurationLoaderStub{err: errors.New("provider raw body")}

	require.Error(t, plugin.OnConfigurationChange())
	require.Nil(t, plugin.currentRuntime())
	require.Equal(t, []string{"manual-stop", "workers-stop"}, events.snapshot()[3:])
	_, configured := plugin.getConfiguration()
	require.False(t, configured)
}

func TestDeactivationCancelsAndDrainsManualAndWorkerWork(t *testing.T) {
	events := &lifecycleEvents{}
	plugin, _ := newLifecyclePlugin(events, func(ctx context.Context, _ *runtimeConfiguration) (*translationRuntime, error) {
		return newLifecycleRuntime(ctx, events), nil
	})
	require.NoError(t, plugin.OnConfigurationChange())
	require.NoError(t, plugin.OnActivate())
	runtime := plugin.currentRuntime()

	require.NoError(t, plugin.OnDeactivate())
	require.ErrorIs(t, runtime.context.Err(), context.Canceled)
	require.Equal(t, []string{"manual-stop", "workers-stop"}, events.snapshot()[3:])
	require.Nil(t, plugin.currentRuntime())
}

func TestMessageHooksIgnoreGeneratedPostsAndDurablyRouteSourceLifecycle(t *testing.T) {
	events := &lifecycleEvents{}
	runtime := newLifecycleRuntime(t.Context(), events)
	plugin := &Plugin{runtime: runtime}
	automaticRuntime := runtime.automatic.(*fakeAutomaticRuntime)

	generated := &model.Post{Id: "generated", UserId: "translator"}
	generated.AddProp(publication.TranslatorKindProp, "automatic")
	plugin.MessageHasBeenPosted(&mmplugin.Context{}, generated)
	plugin.MessageHasBeenUpdated(&mmplugin.Context{}, generated, generated)
	select {
	case <-automaticRuntime.posted:
		t.Fatal("generated post looped through posted hook")
	default:
	}
	select {
	case <-automaticRuntime.updated:
		t.Fatal("generated post looped through updated hook")
	default:
	}

	source := &model.Post{Id: "source", UserId: "alex", ChannelId: "channel"}
	plugin.MessageHasBeenPosted(&mmplugin.Context{}, source)
	plugin.MessageHasBeenUpdated(&mmplugin.Context{}, source, source.Clone())
	plugin.MessageHasBeenDeleted(&mmplugin.Context{}, source)
	require.Same(t, source, <-automaticRuntime.posted)
	require.Same(t, source, <-automaticRuntime.updated)
	require.Same(t, source, <-automaticRuntime.deleted)

	plugin.MessageHasBeenDeleted(&mmplugin.Context{}, generated)
	require.Same(t, generated, <-automaticRuntime.deleted, "translator deletions must reach suppression handling")
}

func TestMessageUpdatedHookIgnoresOwnedInlineTranslationOnly(t *testing.T) {
	events := &lifecycleEvents{}
	runtime := newLifecycleRuntime(t.Context(), events)
	plugin := &Plugin{runtime: runtime}
	automaticRuntime := runtime.automatic.(*fakeAutomaticRuntime)
	before := &model.Post{Id: "source", UserId: "alex", ChannelId: "channel", Message: "Hello", UpdateAt: 100}
	after, ok := publication.ApplyInlineTranslation(before, "안녕하세요")
	require.True(t, ok)
	after.UpdateAt = 200

	plugin.MessageHasBeenUpdated(&mmplugin.Context{}, after, before)

	select {
	case <-automaticRuntime.updated:
		t.Fatal("owned inline translation looped through updated hook")
	default:
	}

	edited := after.Clone()
	edited.Message = "안녕하세요 again\n\n> Hello"
	edited.UpdateAt = 300
	plugin.MessageHasBeenUpdated(&mmplugin.Context{}, edited, after)
	select {
	case <-automaticRuntime.updated:
		t.Fatal("owned inline translation looped through updated hook")
	default:
	}

	resourced := after.Clone()
	resourced.Message = "안녕하세요\n\n> Hello again"
	resourced.UpdateAt = 400
	plugin.MessageHasBeenUpdated(&mmplugin.Context{}, resourced, after)
	require.Same(t, resourced, <-automaticRuntime.updated)
}

func TestExecuteCommandReturnsOnlyPrivateManualResponse(t *testing.T) {
	runtime := newLifecycleRuntime(t.Context(), &lifecycleEvents{})
	plugin := &Plugin{runtime: runtime}
	response, appErr := plugin.ExecuteCommand(&mmplugin.Context{}, &model.CommandArgs{
		Command:   "/trans @alex 안녕하세요",
		UserId:    "caller",
		ChannelId: "channel",
	})
	require.Nil(t, appErr)
	require.Equal(t, model.CommandResponseTypeEphemeral, response.ResponseType)
	require.Equal(t, "private", response.Text)
}

func TestServeTransForwardsDraftFilesForPermittedUser(t *testing.T) {
	runtime := newLifecycleRuntime(t.Context(), &lifecycleEvents{})
	api := &plugintest.API{}
	api.On("HasPermissionToChannel", "caller", "channel", model.PermissionCreatePost).Return(true)
	plugin := &Plugin{runtime: runtime}
	plugin.SetAPI(api)

	request := httptest.NewRequest(http.MethodPost, "/trans",
		strings.NewReader(`{"channel_id":"channel","root_id":"root","command":"/trans 안녕하세요","file_ids":["file1"]}`))
	request.Header.Set("Mattermost-User-Id", "caller")
	recorder := httptest.NewRecorder()
	plugin.ServeHTTP(&mmplugin.Context{}, recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"text":"private"}`, recorder.Body.String())
	manual := runtime.manual.(*fakeManualRuntime)
	require.Equal(t, &model.CommandArgs{UserId: "caller", ChannelId: "channel", RootId: "root", Command: "/trans 안녕하세요"}, manual.args)
	require.Equal(t, []string{"file1"}, manual.fileIDs)
}

func TestServeTransRejectsUnauthenticatedOrUnpermittedUser(t *testing.T) {
	runtime := newLifecycleRuntime(t.Context(), &lifecycleEvents{})
	api := &plugintest.API{}
	api.On("HasPermissionToChannel", "caller", "channel", model.PermissionCreatePost).Return(false)
	plugin := &Plugin{runtime: runtime}
	plugin.SetAPI(api)
	body := `{"channel_id":"channel","command":"/trans 안녕하세요","file_ids":["file1"]}`

	anonymous := httptest.NewRecorder()
	plugin.ServeHTTP(&mmplugin.Context{}, anonymous, httptest.NewRequest(http.MethodPost, "/trans", strings.NewReader(body)))
	require.Equal(t, http.StatusUnauthorized, anonymous.Code)

	request := httptest.NewRequest(http.MethodPost, "/trans", strings.NewReader(body))
	request.Header.Set("Mattermost-User-Id", "caller")
	forbidden := httptest.NewRecorder()
	plugin.ServeHTTP(&mmplugin.Context{}, forbidden, request)
	require.Equal(t, http.StatusForbidden, forbidden.Code)
	require.Nil(t, runtime.manual.(*fakeManualRuntime).args)
}

type captureComponentPointers struct {
	automaticRepository *store.Repository
	workerRepository    worker.Repository
	manualGate          *limit.Gate
	workerGate          *limit.Gate
	workerLogger        worker.Logger
}

func TestRuntimeAssemblySharesExactRepositoryAndGatePointers(t *testing.T) {
	repository := store.NewRepository(nil)
	gate := limit.New(7)
	captured := &captureComponentPointers{}
	logger := newSafeLogger(&capturedSafeLog{})
	var automaticActivationAt int64
	var workerModel string
	constructors := runtimeConstructors{
		newAutomatic: func(_ *automatic.Scope, repo *store.Repository, _ publication.PublicationAPI, _ automatic.KoreanTranslator, _ automatic.Waker, config automatic.ServiceConfig) *automatic.Service {
			captured.automaticRepository = repo
			automaticActivationAt = config.ActivationAt
			return nil
		},
		newManual: func(_ context.Context, _ manual.API, _ manual.Translator, sharedGate *limit.Gate, _ manual.ServiceConfig) *manual.Service {
			captured.manualGate = sharedGate
			return nil
		},
		newWorker: func(repo worker.Repository, _ worker.Processor, sharedGate *limit.Gate, _ int, model string, workerLogger worker.Logger) *worker.Pool {
			captured.workerRepository = repo
			captured.workerGate = sharedGate
			captured.workerLogger = workerLogger
			workerModel = model
			return nil
		},
	}

	runtime := assembleRuntime(t.Context(), validRuntimeConfiguration(), nil, logger, repository, gate, nil, constructors)
	require.Same(t, repository, runtime.repository)
	require.Same(t, repository, captured.automaticRepository)
	require.Same(t, repository, captured.workerRepository)
	require.Same(t, gate, runtime.gate)
	require.Same(t, gate, captured.manualGate)
	require.Same(t, gate, captured.workerGate)
	require.Same(t, logger, captured.workerLogger)
	require.Equal(t, int64(4_242), automaticActivationAt)
	require.Equal(t, "gemini-3.7-flash", workerModel)
}

func validRuntimeConfiguration() *runtimeConfiguration {
	return &runtimeConfiguration{
		primaryModel: "gemini-3.7-flash", fallbackModel: "gemini-3.6-flash",
		automaticTeamID: "team", automaticSourceUserIDs: []string{"alex"}, translatorBotUserID: "translator",
		includeDirectMessages: true, glossary: map[string]string{}, maximumConcurrentGeminiRequests: 7,
		activationAt: 4_242,
	}
}
