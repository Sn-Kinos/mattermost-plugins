package automatic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/gemini"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/store"
)

const (
	automaticTestTeamID    = "team-example"
	automaticTestChannelID = "channel-town-square"
	automaticTestSourceID  = "source-user"
	automaticTestBotID     = "translator-bot"
	automaticTestSiteURL   = "https://chat.example.com"
	automaticTestModelID   = "gemini-test"
)

var automaticTestNow = time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)

func TestHandlePostedPersistsVersionedJobAndMappingBeforeWake(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)

	require.NoError(t, environment.service.HandlePosted(source.Clone()))

	job, found, err := environment.repository.GetJob(source.Id, source.UpdateAt)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, domain.JobPending, job.State)
	require.Equal(t, source.ChannelId, job.ChannelID)
	require.Equal(t, source.RootId, job.RootID)
	require.NotEmpty(t, job.OperationID)
	require.Equal(t, automaticTestNow, job.OperationStartedAt)
	mapping, found, err := environment.repository.GetMapping(source.Id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, source.UpdateAt, mapping.SourceUpdateAt)
	require.Equal(t, job.OperationID, mapping.OperationID)
	require.Equal(t, 1, environment.waker.wakes)
	require.True(t, environment.waker.persistedBeforeEveryWake)
	require.Empty(t, environment.translator.snapshot())
	require.Empty(t, environment.api.visibleBotPosts())
}

func TestHandlePostedDuplicateEventIsIdempotent(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))
	firstJob, found, err := environment.repository.GetJob(source.Id, source.UpdateAt)
	require.NoError(t, err)
	require.True(t, found)
	firstMapping, found, err := environment.repository.GetMapping(source.Id)
	require.NoError(t, err)
	require.True(t, found)

	environment.now = environment.now.Add(time.Hour)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))

	secondJob, found, err := environment.repository.GetJob(source.Id, source.UpdateAt)
	require.NoError(t, err)
	require.True(t, found)
	secondMapping, found, err := environment.repository.GetMapping(source.Id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, firstJob, secondJob)
	require.Equal(t, firstMapping, secondMapping)
	require.Equal(t, 1, environment.waker.wakes)
}

func TestHandleUpdatedCreatesNewVersionJob(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))
	first, found, err := environment.repository.GetJob(source.Id, source.UpdateAt)
	require.NoError(t, err)
	require.True(t, found)

	updated := source.Clone()
	updated.UpdateAt = 202
	updated.Message = "Edited"
	environment.api.seedPost(updated)
	environment.now = environment.now.Add(time.Minute)
	require.NoError(t, environment.service.HandleUpdated(updated.Clone()))

	second, found, err := environment.repository.GetJob(updated.Id, updated.UpdateAt)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEqual(t, first.OperationID, second.OperationID)
	require.Equal(t, environment.now, second.OperationStartedAt)
	mapping, found, err := environment.repository.GetMapping(source.Id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, updated.UpdateAt, mapping.SourceUpdateAt)
	require.Equal(t, second.OperationID, mapping.OperationID)
	require.Equal(t, 2, environment.waker.wakes)
	require.Empty(t, environment.translator.snapshot())
}

func TestHandleUpdatedProcessesMappedSourceCreatedBeforeCurrentActivation(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	updated := automaticSourcePost("source-1", 202, "Edited after restart")
	updated.CreateAt = 50
	prior := domain.SourceMapping{
		SourcePostID:       updated.Id,
		SourceUpdateAt:     40,
		ChannelID:          updated.ChannelId,
		OperationID:        "operation-before-restart",
		OperationStartedAt: automaticTestNow.Add(-time.Hour),
		TranslationPostID:  "translation-1",
	}
	require.NoError(t, environment.repository.PutMapping(prior))
	environment.api.seedPost(updated)

	require.NoError(t, environment.service.HandleUpdated(updated.Clone()))

	job := mustJob(t, environment.repository, updated.Id, updated.UpdateAt)
	require.NotEqual(t, prior.OperationID, job.OperationID)
	require.Equal(t, prior.TranslationPostID, mustMapping(t, environment.repository, updated.Id).TranslationPostID)
	require.Equal(t, 1, environment.waker.wakes)
}

func TestHandlePostedSkipsTranslatorLoops(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	translatorPost := automaticSourcePost("translator-post", 101, "loop")
	translatorPost.UserId = automaticTestBotID
	generatedPost := automaticSourcePost("generated-post", 102, "loop")
	generatedPost.AddProp(publication.TranslatorKindProp, "automatic")

	require.NoError(t, environment.service.HandlePosted(translatorPost))
	require.NoError(t, environment.service.HandlePosted(generatedPost))

	due, err := environment.repository.ListDueJobs(time.Unix(1<<31, 0))
	require.NoError(t, err)
	require.Empty(t, due)
	mappings, err := environment.repository.ListMappings()
	require.NoError(t, err)
	require.Empty(t, mappings)
	require.Zero(t, environment.waker.wakes)
	require.Empty(t, environment.translator.snapshot())
}

type automaticTestEnvironment struct {
	api                *automaticTestAPI
	repository         *store.Repository
	translator         *automaticTestTranslator
	waker              *automaticTestWaker
	service            *Service
	configuredGlossary map[string]string
	now                time.Time
	intentIDMu         sync.Mutex
	nextIntentID       int
}

func newAutomaticTestEnvironment(t *testing.T) *automaticTestEnvironment {
	t.Helper()
	api := newAutomaticTestAPI()
	repository := store.NewRepository(newAutomaticMemoryKV())
	translator := &automaticTestTranslator{
		translate: func(input gemini.KoreanInput) string {
			return strings.Replace(input.Message, "Hello", "안녕하세요", 1)
		},
	}
	waker := &automaticTestWaker{repository: repository, persistedBeforeEveryWake: true}
	configuredGlossary := map[string]string{"배포": "deployment"}
	environment := &automaticTestEnvironment{
		api:                api,
		repository:         repository,
		translator:         translator,
		waker:              waker,
		configuredGlossary: configuredGlossary,
		now:                automaticTestNow,
	}
	scope := NewScope(api, automaticTestTeamID, []string{automaticTestSourceID}, automaticTestBotID, false)
	environment.service = newService(
		scope,
		repository,
		api,
		translator,
		publication.NewPublisher(api, repository, repository),
		waker,
		ServiceConfig{
			ActivationAt:     100,
			TranslatorUserID: automaticTestBotID,
			SiteURL:          automaticTestSiteURL,
			ModelID:          automaticTestModelID,
			Glossary:         configuredGlossary,
			Now:              func() time.Time { return environment.now },
		},
		environment.newPublicationIntentID,
	)
	return environment
}

func (environment *automaticTestEnvironment) newService(repository Repository, publisher PostPublisher) *Service {
	scope := NewScope(environment.api, automaticTestTeamID, []string{automaticTestSourceID}, automaticTestBotID, false)
	return newService(
		scope,
		repository,
		environment.api,
		environment.translator,
		publisher,
		environment.waker,
		ServiceConfig{
			ActivationAt:     100,
			TranslatorUserID: automaticTestBotID,
			SiteURL:          automaticTestSiteURL,
			ModelID:          automaticTestModelID,
			Glossary:         environment.configuredGlossary,
			Now:              func() time.Time { return environment.now },
		},
		environment.newPublicationIntentID,
	)
}

func (environment *automaticTestEnvironment) newPublicationIntentID() string {
	environment.intentIDMu.Lock()
	defer environment.intentIDMu.Unlock()
	environment.nextIntentID++
	return fmt.Sprintf("publication-intent-%d", environment.nextIntentID)
}

func automaticSourcePost(id string, updateAt int64, message string) *model.Post {
	return &model.Post{
		Id:        id,
		CreateAt:  101,
		UpdateAt:  updateAt,
		UserId:    automaticTestSourceID,
		ChannelId: automaticTestChannelID,
		Message:   message,
	}
}

func automaticMappedPost(id string, mapping domain.SourceMapping, createAt int64) *model.Post {
	post := &model.Post{
		Id:        id,
		CreateAt:  createAt,
		UpdateAt:  createAt,
		UserId:    automaticTestBotID,
		ChannelId: mapping.ChannelID,
		Message:   "🇰🇷 translated",
	}
	post.SetProps(model.StringInterface{
		publication.TranslatorKindProp:           "automatic",
		publication.TranslatorOperationIDProp:    mapping.OperationID,
		publication.TranslatorSourcePostIDProp:   mapping.SourcePostID,
		publication.TranslatorSourceUpdateAtProp: mapping.SourceUpdateAt,
		publication.TranslatorModelProp:          automaticTestModelID,
		model.PostPropsSilentNotification:        true,
	})
	return post
}

func mustJob(t *testing.T, repository *store.Repository, sourceID string, updateAt int64) domain.AutomaticJob {
	t.Helper()
	job, found, err := repository.GetJob(sourceID, updateAt)
	require.NoError(t, err)
	require.True(t, found)
	return job
}

func mustMapping(t *testing.T, repository *store.Repository, sourceID string) domain.SourceMapping {
	t.Helper()
	mapping, found, err := repository.GetMapping(sourceID)
	require.NoError(t, err)
	require.True(t, found)
	return mapping
}

type recordedAutomaticTranslation struct {
	input    gemini.KoreanInput
	glossary map[string]string
}

type automaticTestTranslator struct {
	mu        sync.Mutex
	translate func(gemini.KoreanInput) string
	calls     []recordedAutomaticTranslation
	err       error
}

func (translator *automaticTestTranslator) TranslateToKorean(_ context.Context, input gemini.KoreanInput, glossary map[string]string) (domain.KoreanTranslation, error) {
	translator.mu.Lock()
	defer translator.mu.Unlock()
	copiedGlossary := make(map[string]string, len(glossary))
	for korean, english := range glossary {
		copiedGlossary[korean] = english
	}
	translator.calls = append(translator.calls, recordedAutomaticTranslation{input: input, glossary: copiedGlossary})
	if translator.err != nil {
		return domain.KoreanTranslation{}, translator.err
	}
	translated := input.Message
	if translator.translate != nil {
		translated = translator.translate(input)
	}
	return domain.KoreanTranslation{Text: translated}, nil
}

func (translator *automaticTestTranslator) snapshot() []recordedAutomaticTranslation {
	translator.mu.Lock()
	defer translator.mu.Unlock()
	return append([]recordedAutomaticTranslation(nil), translator.calls...)
}

type automaticTestWaker struct {
	repository               *store.Repository
	wakes                    int
	persistedBeforeEveryWake bool
}

type publicationOutcome struct {
	result publication.Result
	err    error
}

type barrierPostPublisher struct {
	delegate  PostPublisher
	published chan publicationOutcome
	resume    chan struct{}
}

type beforePublishBarrier struct {
	delegate   PostPublisher
	blocked    chan struct{}
	resume     chan struct{}
	blockOnce  sync.Once
	resumeOnce sync.Once
}

func newBarrierPostPublisher(delegate PostPublisher) *barrierPostPublisher {
	return &barrierPostPublisher{
		delegate:  delegate,
		published: make(chan publicationOutcome, 1),
		resume:    make(chan struct{}),
	}
}

func newBeforePublishBarrier(delegate PostPublisher) *beforePublishBarrier {
	return &beforePublishBarrier{
		delegate: delegate,
		blocked:  make(chan struct{}),
		resume:   make(chan struct{}),
	}
}

type postReloadBarrier struct {
	publication.PublicationAPI
	targetPostID string
	blockOnCall  int
	mu           sync.Mutex
	calls        int
	blocked      chan struct{}
	resume       chan struct{}
}

type deletePostBarrier struct {
	LifecycleAPI
	targetPostID string
	blocked      chan struct{}
	resume       chan struct{}
	blockOnce    sync.Once
	resumeOnce   sync.Once
}

func newDeletePostBarrier(api LifecycleAPI, targetPostID string) *deletePostBarrier {
	return &deletePostBarrier{
		LifecycleAPI: api,
		targetPostID: targetPostID,
		blocked:      make(chan struct{}),
		resume:       make(chan struct{}),
	}
}

func (api *deletePostBarrier) DeletePost(postID string) *model.AppError {
	if postID == api.targetPostID {
		api.blockOnce.Do(func() {
			close(api.blocked)
			<-api.resume
		})
	}
	return api.LifecycleAPI.DeletePost(postID)
}

func (api *deletePostBarrier) release() {
	api.resumeOnce.Do(func() { close(api.resume) })
}

func newPostReloadBarrier(api publication.PublicationAPI, targetPostID string, blockOnCall int) *postReloadBarrier {
	return &postReloadBarrier{
		PublicationAPI: api,
		targetPostID:   targetPostID,
		blockOnCall:    blockOnCall,
		blocked:        make(chan struct{}),
		resume:         make(chan struct{}),
	}
}

func (api *postReloadBarrier) GetPost(postID string) (*model.Post, *model.AppError) {
	post, appErr := api.PublicationAPI.GetPost(postID)
	if postID != api.targetPostID {
		return post, appErr
	}
	api.mu.Lock()
	api.calls++
	shouldBlock := api.calls == api.blockOnCall
	api.mu.Unlock()
	if shouldBlock {
		close(api.blocked)
		<-api.resume
	}
	return post, appErr
}

func (publisher *barrierPostPublisher) Publish(ctx context.Context, request publication.Request) (publication.Result, error) {
	result, err := publisher.delegate.Publish(ctx, request)
	publisher.published <- publicationOutcome{result: result, err: err}
	<-publisher.resume
	return result, err
}

func (publisher *barrierPostPublisher) Reconcile(ctx context.Context, operation publication.Operation) (publication.Result, error) {
	return publisher.delegate.Reconcile(ctx, operation)
}

func (publisher *beforePublishBarrier) Publish(ctx context.Context, request publication.Request) (publication.Result, error) {
	publisher.blockOnce.Do(func() {
		close(publisher.blocked)
		<-publisher.resume
	})
	return publisher.delegate.Publish(ctx, request)
}

func (publisher *beforePublishBarrier) Reconcile(ctx context.Context, operation publication.Operation) (publication.Result, error) {
	return publisher.delegate.Reconcile(ctx, operation)
}

func (publisher *beforePublishBarrier) release() {
	publisher.resumeOnce.Do(func() { close(publisher.resume) })
}

type barrierMappingRepository struct {
	Repository
	shouldBlock func(domain.SourceMapping) bool
	blocked     chan struct{}
	resume      chan struct{}
	once        sync.Once
}

type barrierSaveErrorRepository struct {
	Repository
	shouldBlock func(domain.SourceMapping) bool
	blocked     chan struct{}
	resume      chan struct{}
	once        sync.Once
}

type barrierOutcomeResultRepository struct {
	Repository
	blocked    chan struct{}
	resume     chan struct{}
	blockOnce  sync.Once
	resumeOnce sync.Once
}

func newBarrierOutcomeResultRepository(repository Repository) *barrierOutcomeResultRepository {
	return &barrierOutcomeResultRepository{
		Repository: repository,
		blocked:    make(chan struct{}),
		resume:     make(chan struct{}),
	}
}

func (repository *barrierOutcomeResultRepository) SaveAutomaticPublicationOutcomeCAS(
	before, after domain.AutomaticPublicationOutcome,
) (bool, error) {
	if before.State == domain.AutomaticPublicationPublishing &&
		after.State == domain.AutomaticPublicationPublished && after.TranslationPostID != "" {
		repository.blockOnce.Do(func() {
			close(repository.blocked)
			<-repository.resume
		})
	}
	return repository.Repository.SaveAutomaticPublicationOutcomeCAS(before, after)
}

func (repository *barrierOutcomeResultRepository) release() {
	repository.resumeOnce.Do(func() { close(repository.resume) })
}

func newBarrierSaveErrorRepository(
	repository Repository,
	shouldBlock func(domain.SourceMapping) bool,
) *barrierSaveErrorRepository {
	return &barrierSaveErrorRepository{
		Repository:  repository,
		shouldBlock: shouldBlock,
		blocked:     make(chan struct{}),
		resume:      make(chan struct{}),
	}
}

func (repository *barrierSaveErrorRepository) SaveMappingCAS(beforeToken string, after domain.SourceMapping) (bool, error) {
	blocked := false
	if repository.shouldBlock != nil && repository.shouldBlock(after) {
		repository.once.Do(func() {
			blocked = true
			close(repository.blocked)
			<-repository.resume
		})
	}
	if blocked {
		return false, errors.New("injected mapping CAS failure after publication")
	}
	return repository.Repository.SaveMappingCAS(beforeToken, after)
}

func newBarrierMappingRepository(repository Repository, shouldBlock func(domain.SourceMapping) bool) *barrierMappingRepository {
	return &barrierMappingRepository{
		Repository:  repository,
		shouldBlock: shouldBlock,
		blocked:     make(chan struct{}),
		resume:      make(chan struct{}),
	}
}

func (repository *barrierMappingRepository) SaveMappingCAS(beforeToken string, after domain.SourceMapping) (bool, error) {
	repository.block(after)
	return repository.Repository.SaveMappingCAS(beforeToken, after)
}

func (repository *barrierMappingRepository) CreateMapping(mapping domain.SourceMapping) (bool, error) {
	repository.block(mapping)
	return repository.Repository.CreateMapping(mapping)
}

func (repository *barrierMappingRepository) block(mapping domain.SourceMapping) {
	if repository.shouldBlock == nil || !repository.shouldBlock(mapping) {
		return
	}
	repository.once.Do(func() {
		close(repository.blocked)
		<-repository.resume
	})
}

type failingMappingRepository struct {
	Repository
	mu             sync.Mutex
	failGetMapping int
	failSave       int
}

type failingOutcomeResultRepository struct {
	Repository
	mu       sync.Mutex
	failures int
}

type failingOutcomeIntentRepository struct {
	Repository
	mu       sync.Mutex
	failures int
}

func (repository *failingOutcomeIntentRepository) CreateAutomaticPublicationOutcome(
	outcome domain.AutomaticPublicationOutcome,
) (bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.failures > 0 {
		repository.failures--
		return false, errors.New("injected publication outcome intent failure")
	}
	return repository.Repository.CreateAutomaticPublicationOutcome(outcome)
}

func (repository *failingOutcomeResultRepository) SaveAutomaticPublicationOutcomeCAS(
	before, after domain.AutomaticPublicationOutcome,
) (bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.failures > 0 {
		repository.failures--
		return false, errors.New("injected publication outcome result failure")
	}
	return repository.Repository.SaveAutomaticPublicationOutcomeCAS(before, after)
}

type failingGuardReleaseRepository struct {
	publication.AutomaticPublicationGuardRepository
	mu       sync.Mutex
	failures int
}

func (repository *failingGuardReleaseRepository) ReleaseAutomaticPublicationGuard(
	before domain.AutomaticPublicationGuard,
) (bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.failures > 0 {
		repository.failures--
		return false, errors.New("injected publication guard release failure")
	}
	return repository.AutomaticPublicationGuardRepository.ReleaseAutomaticPublicationGuard(before)
}

type finalCASExhaustionRepository struct {
	Repository
	remaining int
}

func (repository *finalCASExhaustionRepository) SaveMappingCAS(beforeToken string, after domain.SourceMapping) (bool, error) {
	if after.TranslationPostID == "" || repository.remaining == 0 {
		return repository.Repository.SaveMappingCAS(beforeToken, after)
	}
	repository.remaining--
	if repository.remaining != 0 {
		return false, nil
	}
	current, token, found, err := repository.Repository.GetMappingForUpdate(after.SourcePostID)
	if err != nil || !found {
		return false, err
	}
	current.SuppressedUpdateAt = current.SourceUpdateAt
	current.TranslationPostID = ""
	stored, err := repository.Repository.SaveMappingCAS(token, current)
	if err != nil {
		return false, err
	}
	if !stored {
		return false, errors.New("failed to install suppression at final CAS exhaustion")
	}
	return false, nil
}

func (repository *failingMappingRepository) GetMapping(sourcePostID string) (domain.SourceMapping, bool, error) {
	return repository.Repository.GetMapping(sourcePostID)
}

func (repository *failingMappingRepository) GetMappingForUpdate(sourcePostID string) (domain.SourceMapping, string, bool, error) {
	repository.mu.Lock()
	if repository.failGetMapping > 0 {
		repository.failGetMapping--
		repository.mu.Unlock()
		return domain.SourceMapping{}, "", false, errors.New("injected mapping lookup failure")
	}
	repository.mu.Unlock()
	return repository.Repository.GetMappingForUpdate(sourcePostID)
}

func (repository *failingMappingRepository) SaveMappingCAS(beforeToken string, after domain.SourceMapping) (bool, error) {
	if repository.takeSaveFailure() {
		return false, errors.New("injected mapping save failure")
	}
	return repository.Repository.SaveMappingCAS(beforeToken, after)
}

func (repository *failingMappingRepository) CreateMapping(mapping domain.SourceMapping) (bool, error) {
	if repository.takeSaveFailure() {
		return false, errors.New("injected mapping save failure")
	}
	return repository.Repository.CreateMapping(mapping)
}

func (repository *failingMappingRepository) takeSaveFailure() bool {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.failSave == 0 {
		return false
	}
	repository.failSave--
	return true
}

func (waker *automaticTestWaker) Wake() {
	waker.wakes++
	due, err := waker.repository.ListDueJobs(time.Unix(1<<31, 0))
	if err != nil || len(due) == 0 {
		waker.persistedBeforeEveryWake = false
		return
	}
	foundDurableMapping := false
	for _, job := range due {
		mapping, found, getErr := waker.repository.GetMapping(job.SourcePostID)
		if getErr == nil && found && mapping.SourceUpdateAt == job.SourceUpdateAt && mapping.OperationID == job.OperationID {
			foundDurableMapping = true
			break
		}
	}
	if !foundDurableMapping {
		waker.persistedBeforeEveryWake = false
	}
}

type automaticTestAPI struct {
	mu                       sync.Mutex
	posts                    map[string]*model.Post
	channels                 map[string]*model.Channel
	nextPost                 int
	now                      int64
	updateStoreThenError     bool
	createStoreThenError     bool
	scanError                bool
	createAttempted          bool
	updateAttempted          bool
	getPostFailures          map[string]int
	teamMemberships          map[string]*model.TeamMember
	conversationParticipants map[string][]*model.User
}

func newAutomaticTestAPI() *automaticTestAPI {
	return &automaticTestAPI{
		posts: map[string]*model.Post{},
		channels: map[string]*model.Channel{
			automaticTestChannelID: {
				Id:     automaticTestChannelID,
				TeamId: automaticTestTeamID,
				Type:   model.ChannelTypeOpen,
			},
		},
		now:                      10_000,
		getPostFailures:          map[string]int{},
		teamMemberships:          map[string]*model.TeamMember{},
		conversationParticipants: map[string][]*model.User{},
	}
}

func (api *automaticTestAPI) seedPost(post *model.Post) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.posts[post.Id] = post.Clone()
}

func (api *automaticTestAPI) post(postID string) *model.Post {
	api.mu.Lock()
	defer api.mu.Unlock()
	if post := api.posts[postID]; post != nil {
		return post.Clone()
	}
	return nil
}

func (api *automaticTestAPI) visibleBotPosts() []*model.Post {
	api.mu.Lock()
	defer api.mu.Unlock()
	posts := make([]*model.Post, 0)
	for _, post := range api.posts {
		if post.UserId == automaticTestBotID && post.DeleteAt == 0 {
			posts = append(posts, post.Clone())
		}
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].Id < posts[j].Id })
	return posts
}

func (api *automaticTestAPI) GetChannel(channelID string) (*model.Channel, *model.AppError) {
	api.mu.Lock()
	defer api.mu.Unlock()
	channel := api.channels[channelID]
	if channel == nil {
		return nil, automaticNotFound("channel")
	}
	return channel.DeepCopy(), nil
}

func (api *automaticTestAPI) GetUsersInChannel(channelID, _ string, page, perPage int) ([]*model.User, *model.AppError) {
	api.mu.Lock()
	defer api.mu.Unlock()
	participants := api.conversationParticipants[channelID]
	start := page * perPage
	if start >= len(participants) {
		return []*model.User{}, nil
	}
	end := start + perPage
	if end > len(participants) {
		end = len(participants)
	}
	result := make([]*model.User, 0, end-start)
	for _, participant := range participants[start:end] {
		result = append(result, participant.DeepCopy())
	}
	return result, nil
}

func (api *automaticTestAPI) GetTeamMember(teamID, userID string) (*model.TeamMember, *model.AppError) {
	api.mu.Lock()
	defer api.mu.Unlock()
	membership := api.teamMemberships[teamID+"/"+userID]
	if membership == nil {
		return nil, automaticNotFound("team member")
	}
	copied := *membership
	return &copied, nil
}

func (api *automaticTestAPI) GetPost(postID string) (*model.Post, *model.AppError) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.getPostFailures[postID] > 0 {
		api.getPostFailures[postID]--
		return nil, model.NewAppError("automaticTestAPI.GetPost", "injected post lookup failure", nil, "", http.StatusInternalServerError)
	}
	post := api.posts[postID]
	if post == nil {
		return nil, automaticNotFound("post")
	}
	return post.Clone(), nil
}

func (api *automaticTestAPI) failNextGetPost(postID string) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.getPostFailures[postID]++
}

func (api *automaticTestAPI) CreatePost(post *model.Post) (*model.Post, *model.AppError) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.createAttempted = true
	api.nextPost++
	created := post.Clone()
	created.Id = "created-" + time.Unix(int64(api.nextPost), 0).UTC().Format("150405")
	created.CreateAt = api.now + int64(api.nextPost)
	created.UpdateAt = created.CreateAt
	api.posts[created.Id] = created.Clone()
	if api.createStoreThenError {
		return nil, model.NewAppError("automaticTestAPI.CreatePost", "create acknowledgement lost", nil, "", http.StatusInternalServerError)
	}
	return created.Clone(), nil
}

func (api *automaticTestAPI) UpdatePost(post *model.Post) (*model.Post, *model.AppError) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.updateAttempted = true
	current := api.posts[post.Id]
	if current == nil || current.DeleteAt != 0 {
		return nil, automaticNotFound("post")
	}
	updated := post.Clone()
	api.now++
	updated.UpdateAt = api.now
	api.posts[updated.Id] = updated.Clone()
	if api.updateStoreThenError {
		api.updateStoreThenError = false
		return nil, model.NewAppError("automaticTestAPI.UpdatePost", "update acknowledgement lost", nil, "", http.StatusInternalServerError)
	}
	return updated.Clone(), nil
}

func (api *automaticTestAPI) GetPostsForChannel(channelID string, page, perPage int) (*model.PostList, *model.AppError) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.scanError {
		return nil, model.NewAppError("automaticTestAPI.GetPostsForChannel", "scan unavailable", nil, "", http.StatusInternalServerError)
	}
	posts := make([]*model.Post, 0)
	for _, post := range api.posts {
		if post.ChannelId == channelID && post.DeleteAt == 0 {
			posts = append(posts, post.Clone())
		}
	}
	sort.Slice(posts, func(i, j int) bool {
		if posts[i].CreateAt == posts[j].CreateAt {
			return posts[i].Id > posts[j].Id
		}
		return posts[i].CreateAt > posts[j].CreateAt
	})
	start := page * perPage
	if start > len(posts) {
		start = len(posts)
	}
	end := start + perPage
	if end > len(posts) {
		end = len(posts)
	}
	list := model.NewPostList()
	for _, post := range posts[start:end] {
		list.AddPost(post)
	}
	return list, nil
}

func (api *automaticTestAPI) DeletePost(postID string) *model.AppError {
	api.mu.Lock()
	defer api.mu.Unlock()
	post := api.posts[postID]
	if post == nil || post.DeleteAt != 0 {
		return automaticNotFound("post")
	}
	api.now++
	post = post.Clone()
	post.DeleteAt = api.now
	api.posts[postID] = post
	return nil
}

func automaticNotFound(resource string) *model.AppError {
	return model.NewAppError("automaticTestAPI", resource+" not found", nil, "", http.StatusNotFound)
}

type automaticMemoryKV struct {
	mu     sync.Mutex
	values map[string][]byte
}

func newAutomaticMemoryKV() *automaticMemoryKV {
	return &automaticMemoryKV{values: make(map[string][]byte)}
}

func (kv *automaticMemoryKV) Get(key string) ([]byte, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return append([]byte(nil), kv.values[key]...), nil
}

func (kv *automaticMemoryKV) Set(key string, value []byte) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.values[key] = append([]byte(nil), value...)
	return nil
}

func (kv *automaticMemoryKV) SetWithExpiry(key string, value []byte, _ int64) error {
	return kv.Set(key, value)
}

func (kv *automaticMemoryKV) CompareAndSet(key string, oldValue, newValue []byte) (bool, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	current, exists := kv.values[key]
	if exists != (oldValue != nil) || !bytes.Equal(current, oldValue) {
		return false, nil
	}
	kv.values[key] = append([]byte(nil), newValue...)
	return true, nil
}

func (kv *automaticMemoryKV) CompareAndSetWithExpiry(key string, oldValue, newValue []byte, _ int64) (bool, error) {
	return kv.CompareAndSet(key, oldValue, newValue)
}

func (kv *automaticMemoryKV) CompareAndDelete(key string, oldValue []byte) (bool, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	current, exists := kv.values[key]
	if !exists || !bytes.Equal(current, oldValue) {
		return false, nil
	}
	delete(kv.values, key)
	return true, nil
}

func (kv *automaticMemoryKV) Delete(key string) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	delete(kv.values, key)
	return nil
}

func (kv *automaticMemoryKV) List(prefix string, page, perPage int) ([]string, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	keys := make([]string, 0)
	for key := range kv.values {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	start := page * perPage
	if start > len(keys) {
		start = len(keys)
	}
	end := start + perPage
	if end > len(keys) {
		end = len(keys)
	}
	return append([]string(nil), keys[start:end]...), nil
}
