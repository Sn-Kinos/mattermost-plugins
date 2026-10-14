package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	mmplugin "github.com/mattermost/mattermost/server/public/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/gemini"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/limit"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/store"
)

const (
	integrationAlexID       = "alex0000000000000000000000"
	integrationCallerID     = "caller00000000000000000000"
	integrationTargetID     = "target00000000000000000000"
	integrationOutsiderID   = "outsider000000000000000000"
	integrationTranslatorID = "translator0000000000000000"
	integrationTeamID       = "team0000000000000000000000"
	integrationPublicID     = "public00000000000000000000"
	integrationDMID         = "direct00000000000000000000"
	integrationOutsiderDMID = "outside0000000000000000000"
)

type integrationBackendCall struct {
	model           string
	prompt          string
	maxOutputTokens int32
}

type integrationBackendResult struct {
	response gemini.BackendResponse
	err      error
}

type integrationGeminiBackend struct {
	mu sync.Mutex

	calls            []integrationBackendCall
	koreanResults    []integrationBackendResult
	candidateResults []integrationBackendResult
	candidateBlock   <-chan struct{}
	candidateEntered chan<- struct{}
}

func (b *integrationGeminiBackend) Generate(
	ctx context.Context,
	modelID string,
	_ string,
	prompt string,
	_ gemini.Schema,
	maxOutputTokens int32,
) (gemini.BackendResponse, error) {
	b.mu.Lock()
	b.calls = append(b.calls, integrationBackendCall{model: modelID, prompt: prompt, maxOutputTokens: maxOutputTokens})
	var queued integrationBackendResult
	var hasQueued bool
	if maxOutputTokens == 1024 && len(b.koreanResults) > 0 {
		queued, b.koreanResults = b.koreanResults[0], b.koreanResults[1:]
		hasQueued = true
	}
	if maxOutputTokens == 2048 && len(b.candidateResults) > 0 {
		queued, b.candidateResults = b.candidateResults[0], b.candidateResults[1:]
		hasQueued = true
	}
	block, entered := b.candidateBlock, b.candidateEntered
	b.mu.Unlock()

	if maxOutputTokens == 2048 && block != nil {
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		select {
		case <-block:
		case <-ctx.Done():
			return gemini.BackendResponse{}, ctx.Err()
		}
	}
	if hasQueued {
		return queued.response, queued.err
	}
	if maxOutputTokens == 1024 {
		translated := "안녕하세요"
		switch {
		case strings.Contains(prompt, "After suppression"):
			translated = "억제 후"
		case strings.Contains(prompt, "Restarted"):
			translated = "재시작됨"
		case strings.Contains(prompt, "Edited"):
			translated = "수정됨"
		case strings.Contains(prompt, "Reply"):
			translated = "답글"
		case strings.Contains(prompt, "Atlas"):
			translated = "아틀라스 출시"
		}
		return integrationKoreanResponse(translated), nil
	}
	return integrationCandidateResponse(), nil
}

func (b *integrationGeminiBackend) snapshotCalls() []integrationBackendCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]integrationBackendCall(nil), b.calls...)
}

func (b *integrationGeminiBackend) callCount(maxOutputTokens int32) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	for _, call := range b.calls {
		if call.maxOutputTokens == maxOutputTokens {
			count++
		}
	}
	return count
}

func integrationKoreanResponse(text string) gemini.BackendResponse {
	encoded, _ := json.Marshal(map[string]string{"korean": text})
	return gemini.BackendResponse{Candidates: []gemini.BackendCandidate{{
		FinishReason: "STOP",
		Parts:        []gemini.BackendPart{{Text: string(encoded)}},
	}}}
}

func integrationCandidateResponse() gemini.BackendResponse {
	return gemini.BackendResponse{Candidates: []gemini.BackendCandidate{{
		FinishReason: "STOP",
		Parts:        []gemini.BackendPart{{Text: `{"english":"Could you review this?"}`}},
	}}}
}

type integrationStatusError struct{ status int }

func (e integrationStatusError) Error() string       { return "provider request failed" }
func (e integrationStatusError) HTTPStatusCode() int { return e.status }

type integrationKVValue struct {
	value     []byte
	expiresAt time.Time
}

type integrationMattermost struct {
	mu sync.Mutex

	baseTime      int64
	nextID        int
	posts         map[string]*model.Post
	kv            map[string]integrationKVValue
	users         map[string]*model.User
	usernames     map[string]string
	teams         map[string]*model.Team
	teamNames     map[string]string
	channels      map[string]*model.Channel
	participants  map[string][]string
	memberships   map[string]*model.TeamMember
	ephemerals    map[string]map[string]*model.Post
	commands      []*model.Command
	logs          []string
	configuration systemConsoleConfiguration
}

func newIntegrationMattermost(now time.Time) *integrationMattermost {
	api := &integrationMattermost{
		baseTime:     now.UnixMilli(),
		posts:        make(map[string]*model.Post),
		kv:           make(map[string]integrationKVValue),
		users:        make(map[string]*model.User),
		usernames:    make(map[string]string),
		teams:        make(map[string]*model.Team),
		teamNames:    make(map[string]string),
		channels:     make(map[string]*model.Channel),
		participants: make(map[string][]string),
		memberships:  make(map[string]*model.TeamMember),
		ephemerals:   make(map[string]map[string]*model.Post),
	}
	for _, user := range []*model.User{
		{Id: integrationAlexID, Username: "alex"},
		{Id: integrationCallerID, Username: "caller"},
		{Id: integrationTargetID, Username: "target"},
		{Id: integrationOutsiderID, Username: "outsider"},
		{Id: integrationTranslatorID, Username: "translator", IsBot: true, Roles: "system_user"},
	} {
		api.users[user.Id] = cloneIntegrationUser(user)
		api.usernames[user.Username] = user.Id
	}
	api.teams[integrationTeamID] = &model.Team{Id: integrationTeamID, Name: "example"}
	api.teamNames["example"] = integrationTeamID
	api.channels[integrationPublicID] = &model.Channel{Id: integrationPublicID, TeamId: integrationTeamID, Type: model.ChannelTypeOpen}
	api.channels[integrationDMID] = &model.Channel{Id: integrationDMID, Type: model.ChannelTypeDirect}
	api.channels[integrationOutsiderDMID] = &model.Channel{Id: integrationOutsiderDMID, Type: model.ChannelTypeDirect}
	api.participants[integrationDMID] = []string{integrationAlexID, integrationCallerID}
	api.participants[integrationOutsiderDMID] = []string{integrationAlexID, integrationOutsiderID}
	for _, userID := range []string{integrationAlexID, integrationCallerID, integrationTargetID, integrationTranslatorID} {
		api.memberships[integrationTeamID+"/"+userID] = &model.TeamMember{TeamId: integrationTeamID, UserId: userID}
	}
	return api
}

func (a *integrationMattermost) LoadPluginConfiguration(destination any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	encoded, err := json.Marshal(a.configuration)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, destination)
}

func (a *integrationMattermost) RegisterCommand(command *model.Command) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commands = append(a.commands, command)
	return nil
}

func (a *integrationMattermost) GetConfig() *model.Config {
	siteURL := "https://chat.example.com"
	return &model.Config{ServiceSettings: model.ServiceSettings{SiteURL: &siteURL}}
}

func (a *integrationMattermost) LogError(message string, keyValuePairs ...any) {
	a.mu.Lock()
	a.logs = append(a.logs, fmt.Sprint(append([]any{message}, keyValuePairs...)...))
	a.mu.Unlock()
}

func (a *integrationMattermost) GetTeamByName(name string) (*model.Team, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	team := a.teams[a.teamNames[name]]
	if team == nil {
		return nil, integrationNotFound("team")
	}
	cloned := *team
	return &cloned, nil
}

func (a *integrationMattermost) GetUserByUsername(username string) (*model.User, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	user := a.users[a.usernames[username]]
	if user == nil {
		return nil, integrationNotFound("user")
	}
	return cloneIntegrationUser(user), nil
}

func (a *integrationMattermost) GetUser(userID string) (*model.User, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	user := a.users[userID]
	if user == nil {
		return nil, integrationNotFound("user")
	}
	return cloneIntegrationUser(user), nil
}

func (a *integrationMattermost) GetChannel(channelID string) (*model.Channel, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	channel := a.channels[channelID]
	if channel == nil {
		return nil, integrationNotFound("channel")
	}
	cloned := *channel
	return &cloned, nil
}

func (a *integrationMattermost) GetUsersInChannel(channelID, _ string, page, perPage int) ([]*model.User, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := a.participants[channelID]
	start := page * perPage
	if start >= len(ids) {
		return []*model.User{}, nil
	}
	end := min(start+perPage, len(ids))
	users := make([]*model.User, 0, end-start)
	for _, id := range ids[start:end] {
		users = append(users, cloneIntegrationUser(a.users[id]))
	}
	return users, nil
}

func (a *integrationMattermost) GetTeamMember(teamID, userID string) (*model.TeamMember, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	membership := a.memberships[teamID+"/"+userID]
	if membership == nil {
		return nil, integrationNotFound("team member")
	}
	copy := *membership
	return &copy, nil
}

func (a *integrationMattermost) CreatePost(post *model.Post) (*model.Post, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	created := post.Clone()
	a.nextID++
	created.Id = fmt.Sprintf("%026d", a.nextID)
	if created.CreateAt == 0 {
		a.baseTime++
		created.CreateAt = a.baseTime
	}
	a.posts[created.Id] = created.Clone()
	return created, nil
}

func (a *integrationMattermost) UpdatePost(post *model.Post) (*model.Post, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.posts[post.Id]
	if current == nil || current.DeleteAt != 0 {
		return nil, integrationNotFound("post")
	}
	updated := post.Clone()
	a.baseTime++
	updated.UpdateAt = a.baseTime
	a.posts[updated.Id] = updated.Clone()
	return updated, nil
}

func (a *integrationMattermost) GetPost(postID string) (*model.Post, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	post := a.posts[postID]
	if post == nil || post.DeleteAt != 0 {
		return nil, integrationNotFound("post")
	}
	return post.Clone(), nil
}

func (a *integrationMattermost) GetPostsForChannel(channelID string, page, perPage int) (*model.PostList, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	posts := make([]*model.Post, 0)
	for _, post := range a.posts {
		if post.ChannelId == channelID {
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
	if start >= len(posts) {
		return &model.PostList{Order: []string{}, Posts: map[string]*model.Post{}}, nil
	}
	end := min(start+perPage, len(posts))
	list := &model.PostList{Order: make([]string, 0, end-start), Posts: make(map[string]*model.Post, end-start)}
	for _, post := range posts[start:end] {
		list.Order = append(list.Order, post.Id)
		list.Posts[post.Id] = post
	}
	return list, nil
}

func (a *integrationMattermost) DeletePost(postID string) *model.AppError {
	a.mu.Lock()
	defer a.mu.Unlock()
	post := a.posts[postID]
	if post == nil || post.DeleteAt != 0 {
		return integrationNotFound("post")
	}
	a.baseTime++
	post = post.Clone()
	post.DeleteAt = a.baseTime
	a.posts[postID] = post
	return nil
}

func (a *integrationMattermost) SendEphemeralPost(userID string, post *model.Post) *model.Post {
	a.mu.Lock()
	defer a.mu.Unlock()
	sent := post.Clone()
	if sent.Id == "" {
		a.nextID++
		sent.Id = fmt.Sprintf("%026d", a.nextID)
	}
	if a.ephemerals[userID] == nil {
		a.ephemerals[userID] = make(map[string]*model.Post)
	}
	a.ephemerals[userID][sent.Id] = sent.Clone()
	return sent
}

func (a *integrationMattermost) UpdateEphemeralPost(userID string, post *model.Post) *model.Post {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ephemerals[userID] == nil || a.ephemerals[userID][post.Id] == nil {
		return nil
	}
	updated := post.Clone()
	a.ephemerals[userID][post.Id] = updated.Clone()
	return updated
}

func (a *integrationMattermost) DeleteEphemeralPost(userID, postID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.ephemerals[userID], postID)
}

func (a *integrationMattermost) KVGet(key string) ([]byte, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	value, ok := a.liveKVLocked(key)
	if !ok {
		return nil, nil
	}
	return append([]byte(nil), value.value...), nil
}

func (a *integrationMattermost) KVSet(key string, value []byte) *model.AppError {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.kv[key] = integrationKVValue{value: append([]byte(nil), value...)}
	return nil
}

func (a *integrationMattermost) KVSetWithExpiry(key string, value []byte, expireInSeconds int64) *model.AppError {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.kv[key] = integrationKVValue{value: append([]byte(nil), value...), expiresAt: time.Now().Add(time.Duration(expireInSeconds) * time.Second)}
	return nil
}

func (a *integrationMattermost) KVSetWithOptions(key string, value []byte, options model.PluginKVSetOptions) (bool, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	current, found := a.liveKVLocked(key)
	if options.Atomic {
		if found != (options.OldValue != nil) || found && !bytes.Equal(current.value, options.OldValue) {
			return false, nil
		}
	}
	stored := integrationKVValue{value: append([]byte(nil), value...)}
	if options.ExpireInSeconds > 0 {
		stored.expiresAt = time.Now().Add(time.Duration(options.ExpireInSeconds) * time.Second)
	}
	a.kv[key] = stored
	return true, nil
}

func (a *integrationMattermost) KVCompareAndDelete(key string, oldValue []byte) (bool, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	current, found := a.liveKVLocked(key)
	if !found || !bytes.Equal(current.value, oldValue) {
		return false, nil
	}
	delete(a.kv, key)
	return true, nil
}

func (a *integrationMattermost) KVDelete(key string) *model.AppError {
	a.mu.Lock()
	delete(a.kv, key)
	a.mu.Unlock()
	return nil
}

func (a *integrationMattermost) KVList(page, perPage int) ([]string, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	keys := make([]string, 0, len(a.kv))
	for key := range a.kv {
		if _, ok := a.liveKVLocked(key); ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	start := page * perPage
	if start >= len(keys) {
		return []string{}, nil
	}
	return append([]string(nil), keys[start:min(start+perPage, len(keys))]...), nil
}

func (a *integrationMattermost) liveKVLocked(key string) (integrationKVValue, bool) {
	value, found := a.kv[key]
	if found && !value.expiresAt.IsZero() && !time.Now().Before(value.expiresAt) {
		delete(a.kv, key)
		return integrationKVValue{}, false
	}
	return value, found
}

func (a *integrationMattermost) putSource(post *model.Post) {
	a.mu.Lock()
	a.posts[post.Id] = post.Clone()
	a.mu.Unlock()
}

func (a *integrationMattermost) activeBotPosts(channelID, kind string) []*model.Post {
	a.mu.Lock()
	defer a.mu.Unlock()
	posts := make([]*model.Post, 0)
	for _, post := range a.posts {
		if post.ChannelId == channelID && post.UserId == integrationTranslatorID && post.DeleteAt == 0 &&
			fmt.Sprint(post.GetProp(publication.TranslatorKindProp)) == kind {
			posts = append(posts, post.Clone())
		}
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].Id < posts[j].Id })
	return posts
}

func (a *integrationMattermost) ephemeral(userID string) *model.Post {
	posts := a.ephemeralPosts(userID)
	if len(posts) == 0 {
		return nil
	}
	return posts[len(posts)-1]
}

func (a *integrationMattermost) ephemeralPosts(userID string) []*model.Post {
	a.mu.Lock()
	defer a.mu.Unlock()
	posts := make([]*model.Post, 0, len(a.ephemerals[userID]))
	for _, post := range a.ephemerals[userID] {
		posts = append(posts, post.Clone())
	}
	sort.Slice(posts, func(i, j int) bool {
		if posts[i].CreateAt == posts[j].CreateAt {
			return posts[i].Id < posts[j].Id
		}
		return posts[i].CreateAt < posts[j].CreateAt
	})
	return posts
}

func integrationNotFound(resource string) *model.AppError {
	return model.NewAppError("integrationMattermost", "integration.not_found", map[string]any{"resource": resource}, "", http.StatusNotFound)
}

func cloneIntegrationUser(user *model.User) *model.User {
	if user == nil {
		return nil
	}
	cloned := *user
	return &cloned
}

type integrationHarness struct {
	t       *testing.T
	ctx     context.Context
	api     *integrationMattermost
	adapter *mattermostAdapter
	repo    *store.Repository
	backend *integrationGeminiBackend
	client  *gemini.Client
	config  *runtimeConfiguration
	runtime *translationRuntime
	plugin  *Plugin
}

func newIntegrationHarness(t *testing.T, backend *integrationGeminiBackend, glossary map[string]string) *integrationHarness {
	t.Helper()
	if backend == nil {
		backend = &integrationGeminiBackend{}
	}
	now := time.Now().UTC()
	api := newIntegrationMattermost(now)
	adapter := newMattermostAdapter(api)
	repository := store.NewRepository(adapter)
	client := gemini.NewClientWithBackend("gemini-3.7-flash", "gemini-3.6-flash", backend)
	configuration := &runtimeConfiguration{
		primaryModel:                    "gemini-3.7-flash",
		fallbackModel:                   "gemini-3.6-flash",
		automaticTeamID:                 integrationTeamID,
		automaticSourceUserIDs:          []string{integrationAlexID},
		translatorBotUserID:             integrationTranslatorID,
		includeDirectMessages:           true,
		glossary:                        glossary,
		maximumConcurrentGeminiRequests: 2,
		totalRequestTimeout:             10 * time.Second,
		siteURL:                         "https://chat.example.com",
		activationAt:                    now.Add(-time.Second).UnixMilli(),
	}
	h := &integrationHarness{
		t: t, ctx: t.Context(), api: api, adapter: adapter, repo: repository,
		backend: backend, client: client, config: configuration,
	}
	h.startRuntime()
	t.Cleanup(func() {
		if h.runtime != nil {
			h.runtime.stop()
		}
	})
	return h
}

func (h *integrationHarness) startRuntime() {
	h.t.Helper()
	runtime := assembleRuntime(
		h.ctx,
		h.config,
		h.adapter,
		nil,
		h.repo,
		limit.New(h.config.maximumConcurrentGeminiRequests),
		h.client,
		defaultRuntimeConstructors(),
	)
	runtime.setActivationAt(h.config.activationAt)
	require.NoError(h.t, runtime.start())
	h.runtime = runtime
	h.plugin = &Plugin{runtime: runtime, active: true}
}

func (h *integrationHarness) restart() {
	h.t.Helper()
	h.runtime.stop()
	h.startRuntime()
}

func (h *integrationHarness) source(id, channelID, rootID, message string, versionOffset int64) *model.Post {
	return &model.Post{
		Id: id, UserId: integrationAlexID, ChannelId: channelID, RootId: rootID,
		Message: message, CreateAt: h.config.activationAt + 100, UpdateAt: h.config.activationAt + versionOffset,
	}
}

func waitFor(t *testing.T, description string, condition func() bool) {
	t.Helper()
	require.Eventually(t, condition, 3*time.Second, 5*time.Millisecond, description)
}

func TestEndToEndAlexRootPostAppendsKoreanTranslation(t *testing.T) {
	h := newIntegrationHarness(t, nil, nil)
	source := h.source("source-root-00000000000000", integrationPublicID, "", "Hello team", 100)
	h.api.putSource(source)
	h.plugin.MessageHasBeenPosted(&mmplugin.Context{}, source)

	waitFor(t, "inline root translation", func() bool {
		updated, appErr := h.api.GetPost(source.Id)
		return appErr == nil && updated.Message == "안녕하세요\n\n> Hello team"
	})
	updated, appErr := h.api.GetPost(source.Id)
	require.Nil(t, appErr)
	assert.Equal(t, integrationAlexID, updated.UserId)
	assert.Empty(t, updated.RootId)
	assert.Empty(t, h.api.activeBotPosts(integrationPublicID, "automatic"))
	waitFor(t, "automatic job completion", func() bool {
		job, found, err := h.repo.GetJob(source.Id, source.UpdateAt)
		return err == nil && found && job.State == domain.JobSucceeded
	})
	mappingBefore, mappingFound, err := h.repo.GetMapping(source.Id)
	require.NoError(t, err)
	require.True(t, mappingFound)
	jobBefore, jobFound, err := h.repo.GetJob(source.Id, source.UpdateAt)
	require.NoError(t, err)
	require.True(t, jobFound)
	callsBefore := h.backend.callCount(1024)

	h.plugin.MessageHasBeenPosted(&mmplugin.Context{}, source.Clone())
	require.Never(t, func() bool {
		mapping, found, mappingErr := h.repo.GetMapping(source.Id)
		job, foundJob, jobErr := h.repo.GetJob(source.Id, source.UpdateAt)
		current, currentErr := h.api.GetPost(source.Id)
		return mappingErr != nil || jobErr != nil || !found || !foundJob ||
			mapping != mappingBefore || job != jobBefore ||
			h.backend.callCount(1024) != callsBefore ||
			currentErr != nil || current.Message != updated.Message
	}, 250*time.Millisecond, 5*time.Millisecond, "duplicate hook must not mutate durable state, call Gemini, or update the post")
}

func TestEndToEndAlexThreadReplyUsesSameRoot(t *testing.T) {
	h := newIntegrationHarness(t, nil, nil)
	root := &model.Post{Id: "thread-root-0000000000000", UserId: integrationCallerID, ChannelId: integrationPublicID, Message: "Release discussion", CreateAt: h.config.activationAt + 50}
	reply := h.source("thread-reply-000000000000", integrationPublicID, root.Id, "Reply status", 100)
	h.api.putSource(root)
	h.api.putSource(reply)
	h.plugin.MessageHasBeenPosted(&mmplugin.Context{}, reply)

	waitFor(t, "thread translation", func() bool {
		updated, appErr := h.api.GetPost(reply.Id)
		return appErr == nil && updated.Message == "답글\n\n> Reply status"
	})
	translation, appErr := h.api.GetPost(reply.Id)
	require.Nil(t, appErr)
	assert.Equal(t, root.Id, translation.RootId)
	calls := h.backend.snapshotCalls()
	require.NotEmpty(t, calls)
	assert.Contains(t, calls[0].prompt, `"message":"Reply status"`)
	assert.Contains(t, calls[0].prompt, `"parent_context":"Release discussion"`)
}

func TestEndToEndQualifyingDMAndOutsiderDM(t *testing.T) {
	h := newIntegrationHarness(t, nil, nil)
	qualified := h.source("qualified-dm-0000000000000", integrationDMID, "", "Hello in DM", 100)
	outsider := h.source("outsider-dm-00000000000000", integrationOutsiderDMID, "", "Hello outsider", 101)
	h.api.putSource(qualified)
	h.api.putSource(outsider)
	outsiderCallsBefore := h.backend.callCount(1024)
	h.plugin.MessageHasBeenPosted(&mmplugin.Context{}, outsider)
	_, outsiderMapped, err := h.repo.GetMapping(outsider.Id)
	require.NoError(t, err)
	require.False(t, outsiderMapped, "ineligible outsider DM must not create a mapping in its synchronous hook")
	_, outsiderJob, err := h.repo.GetJob(outsider.Id, outsider.UpdateAt)
	require.NoError(t, err)
	require.False(t, outsiderJob, "ineligible outsider DM must not create a job in its synchronous hook")
	require.Never(t, func() bool {
		return h.backend.callCount(1024) != outsiderCallsBefore ||
			len(h.api.activeBotPosts(integrationOutsiderDMID, "automatic")) != 0
	}, 200*time.Millisecond, 5*time.Millisecond, "ineligible outsider DM must remain side-effect free")

	h.plugin.MessageHasBeenPosted(&mmplugin.Context{}, qualified)
	waitFor(t, "qualified DM translation", func() bool {
		updated, appErr := h.api.GetPost(qualified.Id)
		return appErr == nil && updated.Message == "안녕하세요\n\n> Hello in DM"
	})
	assert.Empty(t, h.api.activeBotPosts(integrationOutsiderDMID, "automatic"))
	unchanged, appErr := h.api.GetPost(outsider.Id)
	require.Nil(t, appErr)
	assert.Equal(t, outsider.Message, unchanged.Message)
	assert.Equal(t, outsiderCallsBefore+1, h.backend.callCount(1024))
}

func TestEndToEndEditRestartAndDelete(t *testing.T) {
	h := newIntegrationHarness(t, nil, nil)
	source := h.source("lifecycle-source-000000000", integrationPublicID, "", "Hello", 100)
	h.api.putSource(source)
	h.plugin.MessageHasBeenPosted(&mmplugin.Context{}, source)
	waitFor(t, "initial lifecycle translation", func() bool {
		updated, appErr := h.api.GetPost(source.Id)
		return appErr == nil && updated.Message == "안녕하세요\n\n> Hello"
	})
	previous, appErr := h.api.GetPost(source.Id)
	require.Nil(t, appErr)

	edited := source.Clone()
	edited.Message = "Edited"
	edited.UpdateAt = previous.UpdateAt + 100
	h.api.putSource(edited)
	h.plugin.MessageHasBeenUpdated(&mmplugin.Context{}, edited, previous)
	waitFor(t, "edited translation", func() bool {
		updated, currentErr := h.api.GetPost(source.Id)
		return currentErr == nil && updated.Message == "수정됨\n\n> Edited"
	})
	afterEdit, appErr := h.api.GetPost(source.Id)
	require.Nil(t, appErr)

	restarted := afterEdit.Clone()
	restarted.Message = "Restarted"
	restarted.UpdateAt = afterEdit.UpdateAt + 100
	h.api.putSource(restarted)
	h.restart()
	waitFor(t, "missed edit reconciled after restart", func() bool {
		updated, currentErr := h.api.GetPost(source.Id)
		return currentErr == nil && updated.Message == "재시작됨\n\n> Restarted"
	})

	deleted, appErr := h.api.GetPost(source.Id)
	require.Nil(t, appErr)
	require.Nil(t, h.api.DeletePost(source.Id))
	deleted.DeleteAt = time.Now().UnixMilli()
	h.plugin.MessageHasBeenDeleted(&mmplugin.Context{}, deleted)
	waitFor(t, "source deletion clears mapping", func() bool {
		mapping, found, mappingErr := h.repo.GetMapping(source.Id)
		return mappingErr == nil && found && mapping.InternallyDeleting && mapping.TranslationPostID == ""
	})
}

func TestEndToEndTransPostsPlaceholderThenTranslation(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	backend := &integrationGeminiBackend{candidateBlock: release, candidateEntered: entered}
	h := newIntegrationHarness(t, backend, nil)
	args := &model.CommandArgs{Command: "/trans 검토해 주세요", UserId: integrationCallerID, ChannelId: integrationPublicID}
	response, appErr := h.plugin.ExecuteCommand(&mmplugin.Context{}, args)
	require.Nil(t, appErr)
	require.NotNil(t, response)
	require.Equal(t, model.CommandResponseTypeEphemeral, response.ResponseType)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Gemini translation did not start within one second")
	}
	postList, listErr := h.api.GetPostsForChannel(integrationPublicID, 0, 200)
	require.Nil(t, listErr)
	require.Len(t, postList.Order, 1, "the post appears before the translation is ready")
	placeholder := postList.Posts[postList.Order[0]]
	assert.Equal(t, "Translating…\n\n> 검토해 주세요", placeholder.Message)
	require.Empty(t, h.api.ephemeralPosts(integrationCallerID))
	close(release)

	waitFor(t, "translated post", func() bool {
		updated, appErr := h.api.GetPost(placeholder.Id)
		return appErr == nil && updated.Message != placeholder.Message
	})
	published, appErr := h.api.GetPost(placeholder.Id)
	require.Nil(t, appErr)
	assert.Equal(t, integrationCallerID, published.UserId)
	assert.Equal(t, "Could you review this?\n\n> 검토해 주세요", published.Message)
	assert.Empty(t, published.RootId)
	assert.Empty(t, h.api.ephemeralPosts(integrationCallerID))
	assert.Empty(t, h.api.activeBotPosts(integrationPublicID, "manual"))
}

func TestEndToEndGlossaryAppliesInBothTranslationDirections(t *testing.T) {
	h := newIntegrationHarness(t, nil, map[string]string{"아틀라스": "Atlas", "출시": "launch"})
	source := h.source("glossary-source-0000000000", integrationPublicID, "", "Atlas launch", 100)
	h.api.putSource(source)
	h.plugin.MessageHasBeenPosted(&mmplugin.Context{}, source)
	waitFor(t, "glossary automatic translation", func() bool {
		updated, appErr := h.api.GetPost(source.Id)
		return appErr == nil && strings.Contains(updated.Message, "아틀라스 출시")
	})
	_, appErr := h.plugin.ExecuteCommand(&mmplugin.Context{}, &model.CommandArgs{
		Command: "/trans 아틀라스 출시", UserId: integrationCallerID, ChannelId: integrationPublicID,
	})
	require.Nil(t, appErr)
	waitFor(t, "glossary manual translation", func() bool {
		for _, call := range h.backend.snapshotCalls() {
			if call.maxOutputTokens == 2048 {
				return true
			}
		}
		return false
	})

	var koreanPrompt, englishPrompt string
	for _, call := range h.backend.snapshotCalls() {
		switch call.maxOutputTokens {
		case 1024:
			koreanPrompt = call.prompt
		case 2048:
			englishPrompt = call.prompt
		}
	}
	assert.Contains(t, koreanPrompt, `"english_source":"Atlas"`)
	assert.Contains(t, koreanPrompt, `"korean_required":"아틀라스"`)
	assert.Contains(t, englishPrompt, `"korean_source":"아틀라스"`)
	assert.Contains(t, englishPrompt, `"english_required":"Atlas"`)
}

func TestEndToEndProviderRetryFallbackAndPrivateFailure(t *testing.T) {
	t.Run("primary retry then fallback publishes", func(t *testing.T) {
		backend := &integrationGeminiBackend{koreanResults: []integrationBackendResult{
			{err: integrationStatusError{status: http.StatusServiceUnavailable}},
			{err: integrationStatusError{status: http.StatusTooManyRequests}},
			{response: integrationKoreanResponse("폴백 성공")},
		}}
		h := newIntegrationHarness(t, backend, nil)
		source := h.source("fallback-source-00000000000", integrationPublicID, "", "Fallback please", 100)
		h.api.putSource(source)
		h.plugin.MessageHasBeenPosted(&mmplugin.Context{}, source)
		waitFor(t, "fallback publication", func() bool {
			updated, appErr := h.api.GetPost(source.Id)
			return appErr == nil && strings.Contains(updated.Message, "폴백 성공")
		})
		calls := backend.snapshotCalls()
		require.Len(t, calls, 3)
		assert.Equal(t, []string{"gemini-3.7-flash", "gemini-3.7-flash", "gemini-3.6-flash"}, []string{calls[0].model, calls[1].model, calls[2].model})
	})

	t.Run("manual provider failure stays private", func(t *testing.T) {
		backend := &integrationGeminiBackend{candidateResults: []integrationBackendResult{{
			err: integrationStatusError{status: http.StatusUnauthorized},
		}}}
		h := newIntegrationHarness(t, backend, nil)
		response, appErr := h.plugin.ExecuteCommand(&mmplugin.Context{}, &model.CommandArgs{
			Command: "/trans 비공개 실패", UserId: integrationCallerID, ChannelId: integrationPublicID,
		})
		require.Nil(t, appErr)
		require.NotNil(t, response)
		require.Equal(t, model.CommandResponseTypeEphemeral, response.ResponseType)
		waitFor(t, "private provider failure", func() bool {
			post := h.api.ephemeral(integrationCallerID)
			return post != nil && strings.Contains(post.Message, "원문만 게시했습니다")
		})
		postList, listErr := h.api.GetPostsForChannel(integrationPublicID, 0, 200)
		require.Nil(t, listErr)
		require.Len(t, postList.Order, 1)
		assert.Equal(t, "비공개 실패", postList.Posts[postList.Order[0]].Message)
		calls := backend.snapshotCalls()
		require.Len(t, calls, 1)
		assert.Equal(t, "gemini-3.7-flash", calls[0].model)
	})
}

var _ mattermostAPI = (*integrationMattermost)(nil)
var _ gemini.Backend = (*integrationGeminiBackend)(nil)
