package publication

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
)

const (
	testBotID       = "translator-bot"
	testChannelID   = "channel-1"
	testSourceID    = "source-1"
	testOperationID = "operation-2"
)

var testStartedAt = time.UnixMilli(1_000)

func TestPublishAdoptsPostWhenCreateSucceededButAcknowledgementWasLost(t *testing.T) {
	api := newPublicationTestAPI()
	api.createStoreThenError = true
	publisher := NewPublisher(api, &deletionIntentRecorder{})

	result, err := publisher.Publish(context.Background(), automaticRequest())

	require.NoError(t, err)
	posts := api.visiblePostsForOperation(testOperationID)
	require.Len(t, posts, 1)
	require.Equal(t, posts[0].Id, result.PostID)
	require.NotEmpty(t, result.PostID)
	require.Equal(t, "translated message", posts[0].Message)
}

func TestPublishReturnsUncertainWithoutBlindRetryWhenCreateFailsAndRescanIsEmpty(t *testing.T) {
	api := newPublicationTestAPI()
	api.createErrorBeforeStoreOnce = true
	publisher := NewPublisher(api, &deletionIntentRecorder{})

	_, err := publisher.Publish(context.Background(), automaticRequest())

	require.ErrorIs(t, err, ErrReconcileUncertain)
	require.Empty(t, api.visiblePostsForOperation(testOperationID))
	require.Empty(t, api.allVisiblePosts())
}

func TestReconcileDistinguishesConfirmedMissingOperationFromScanFailure(t *testing.T) {
	api := newPublicationTestAPI()
	publisher := NewPublisher(api, &deletionIntentRecorder{})
	request := automaticRequest()

	_, err := publisher.Reconcile(context.Background(), Operation{
		ID:        request.OperationID,
		StartedAt: request.OperationStartedAt,
		ChannelID: request.ChannelID,
		UserID:    request.UserID,
	})

	require.ErrorIs(t, err, ErrOperationPostNotFound)
	api.scanErrorPage = 0
	_, err = publisher.Reconcile(context.Background(), Operation{
		ID:        request.OperationID,
		StartedAt: request.OperationStartedAt,
		ChannelID: request.ChannelID,
		UserID:    request.UserID,
	})
	require.ErrorIs(t, err, ErrReconcileUncertain)
	require.NotErrorIs(t, err, ErrOperationPostNotFound)
}

func TestPublishFindsOperationAfterPendingPostCacheWindow(t *testing.T) {
	api := newPublicationTestAPI()
	for index := 0; index < 200; index++ {
		api.seedPost(&model.Post{
			Id:        fmt.Sprintf("newer-%03d", index),
			CreateAt:  3_000 + int64(index),
			UserId:    "ordinary-user",
			ChannelId: testChannelID,
		})
	}
	existing := automaticPost("mattermost-generated", testOperationID, 2, 2_000)
	api.seedPost(existing)
	publisher := NewPublisher(api, &deletionIntentRecorder{})

	result, err := publisher.Publish(context.Background(), automaticRequest())

	require.NoError(t, err)
	require.Equal(t, existing.Id, result.PostID)
	posts := api.visiblePostsForOperation(testOperationID)
	require.Len(t, posts, 1)
	require.Equal(t, existing.Id, posts[0].Id)
}

func TestPublishNeverCreatesWhenChannelScanFails(t *testing.T) {
	api := newPublicationTestAPI()
	api.scanErrorPage = 0
	publisher := NewPublisher(api, &deletionIntentRecorder{})

	_, err := publisher.Publish(context.Background(), automaticRequest())

	require.ErrorIs(t, err, ErrReconcileUncertain)
	require.Empty(t, api.visiblePostsForOperation(testOperationID))
	require.Empty(t, api.allVisiblePosts())
}

func TestPublishDoesNotAdoptEarlyMatchWhenLaterScanPageFails(t *testing.T) {
	api := newPublicationTestAPI()
	api.seedPost(automaticPost("early-match", testOperationID, 2, 4_000))
	for index := 0; index < 199; index++ {
		api.seedPost(&model.Post{
			Id:        fmt.Sprintf("newer-%03d", index),
			CreateAt:  3_000 + int64(index),
			UserId:    "ordinary-user",
			ChannelId: testChannelID,
		})
	}
	api.scanErrorPage = 1
	publisher := NewPublisher(api, &deletionIntentRecorder{})

	_, err := publisher.Publish(context.Background(), automaticRequest())

	require.ErrorIs(t, err, ErrReconcileUncertain)
	posts := api.visiblePostsForOperation(testOperationID)
	require.Len(t, posts, 1)
	require.Equal(t, "early-match", posts[0].Id)
}

func TestPublishKeepsEarliestMatchAndDeletesDuplicatesWithIntent(t *testing.T) {
	api := newPublicationTestAPI()
	api.seedPost(automaticPost("later", testOperationID, 2, 2_100))
	api.seedPost(automaticPost("same-time-b", testOperationID, 2, 2_000))
	api.seedPost(automaticPost("same-time-a", testOperationID, 2, 2_000))
	intents := &deletionIntentRecorder{}
	publisher := NewPublisher(api, intents)

	result, err := publisher.Publish(context.Background(), automaticRequest())

	require.NoError(t, err)
	require.Equal(t, "same-time-a", result.PostID)
	posts := api.visiblePostsForOperation(testOperationID)
	require.Len(t, posts, 1)
	require.Equal(t, "same-time-a", posts[0].Id)
	require.Equal(t, map[string]recordedDeletionIntent{
		"later": {
			intent: domain.DeletionIntent{
				TranslationPostID: "later",
				SourcePostID:      testSourceID,
				OperationID:       testOperationID,
			},
			expireInSeconds: 300,
		},
		"same-time-b": {
			intent: domain.DeletionIntent{
				TranslationPostID: "same-time-b",
				SourcePostID:      testSourceID,
				OperationID:       testOperationID,
			},
			expireInSeconds: 300,
		},
	}, intents.snapshot())
}

func TestPublishUsesOperationIDAsPendingPostIDAndProperty(t *testing.T) {
	api := newPublicationTestAPI()
	publisher := NewPublisher(api, &deletionIntentRecorder{})

	result, err := publisher.Publish(context.Background(), automaticRequest())

	require.NoError(t, err)
	post := api.post(result.PostID)
	require.NotNil(t, post)
	require.Equal(t, testOperationID, post.PendingPostId)
	require.Equal(t, testOperationID, post.GetProp(TranslatorOperationIDProp))
	require.Equal(t, testBotID, post.UserId)
	require.Equal(t, testChannelID, post.ChannelId)
	require.Equal(t, "root-1", post.RootId)
	require.Equal(t, "translated message", post.Message)
}

func TestPublishUpdatesKnownAutomaticPostWithoutChangingMattermostID(t *testing.T) {
	api := newPublicationTestAPI()
	existing := automaticPost("known-post", "operation-1", 1, 500)
	existing.Message = "old translation"
	existing.AddProp(TranslatorModelProp, "old-model")
	api.seedPost(existing)
	request := automaticRequest()
	request.KnownPostID = existing.Id
	request.PublicationGuard = publicationGuardForRequest(request)
	request.PublicationGuardNow = testStartedAt
	publisher := NewPublisher(api, &deletionIntentRecorder{}, &allowingPublicationGuardRepository{})

	result, err := publisher.Publish(context.Background(), request)

	require.NoError(t, err)
	require.Equal(t, existing.Id, result.PostID)
	posts := api.allVisiblePosts()
	require.Len(t, posts, 1)
	updated := posts[0]
	require.Equal(t, existing.Id, updated.Id)
	require.Equal(t, existing.CreateAt, updated.CreateAt)
	require.Equal(t, "translated message", updated.Message)
	require.Equal(t, testOperationID, updated.GetProp(TranslatorOperationIDProp))
	require.EqualValues(t, 2, updated.GetProp(TranslatorSourceUpdateAtProp))
	require.Equal(t, "new-model", updated.GetProp(TranslatorModelProp))
	require.Equal(t, testSourceID, updated.GetProp(TranslatorSourcePostIDProp))
	require.Equal(t, "automatic", updated.GetProp(TranslatorKindProp))
}

func TestPublishRejectsOlderKnownAutomaticVersion(t *testing.T) {
	api := newPublicationTestAPI()
	existing := automaticPost("known-post", "operation-3", 3, 500)
	existing.Message = "newer translation"
	api.seedPost(existing)
	request := automaticRequest()
	request.KnownPostID = existing.Id
	request.PublicationGuard = publicationGuardForRequest(request)
	request.PublicationGuardNow = testStartedAt
	publisher := NewPublisher(api, &deletionIntentRecorder{}, &allowingPublicationGuardRepository{})

	_, err := publisher.Publish(context.Background(), request)

	require.ErrorIs(t, err, ErrStaleKnownPost)
	stored := api.post(existing.Id)
	require.Equal(t, "newer translation", stored.Message)
	require.Equal(t, "operation-3", stored.GetProp(TranslatorOperationIDProp))
	require.EqualValues(t, 3, stored.GetProp(TranslatorSourceUpdateAtProp))
}

func TestPublishFailsClosedForKnownAutomaticUpdateWithoutCompleteGuard(t *testing.T) {
	for _, test := range []struct {
		name      string
		withStore bool
		withData  bool
	}{
		{name: "missing guard store", withData: true},
		{name: "missing guard data", withStore: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newPublicationTestAPI()
			existing := automaticPost("known-post", "operation-1", 1, 500)
			existing.Message = "old translation"
			api.seedPost(existing)
			guards := &allowingPublicationGuardRepository{}
			publisher := NewPublisher(api, &deletionIntentRecorder{})
			if test.withStore {
				publisher = NewPublisher(api, &deletionIntentRecorder{}, guards)
			}
			request := automaticRequest()
			request.KnownPostID = existing.Id
			if test.withData {
				request.PublicationGuard = publicationGuardForRequest(request)
				request.PublicationGuardNow = testStartedAt
			}

			_, err := publisher.Publish(context.Background(), request)

			require.ErrorIs(t, err, ErrReconcileUncertain)
			stored := api.post(existing.Id)
			require.Equal(t, "old translation", stored.Message)
			require.Equal(t, "operation-1", stored.GetProp(TranslatorOperationIDProp))
			require.EqualValues(t, 1, stored.GetProp(TranslatorSourceUpdateAtProp))
		})
	}
}

func TestPublishAdoptsKnownUpdateWhenAcknowledgementWasLost(t *testing.T) {
	api := newPublicationTestAPI()
	existing := automaticPost("known-post", "operation-1", 1, 500)
	existing.Message = "old translation"
	api.seedPost(existing)
	api.updateStoreThenError = true
	request := automaticRequest()
	request.KnownPostID = existing.Id
	request.PublicationGuard = publicationGuardForRequest(request)
	request.PublicationGuardNow = testStartedAt
	publisher := NewPublisher(api, &deletionIntentRecorder{}, &allowingPublicationGuardRepository{})

	result, err := publisher.Publish(context.Background(), request)

	require.NoError(t, err)
	require.Equal(t, existing.Id, result.PostID)
	posts := api.allVisiblePosts()
	require.Len(t, posts, 1)
	require.Equal(t, "translated message", posts[0].Message)
	require.Equal(t, testOperationID, posts[0].GetProp(TranslatorOperationIDProp))
	require.EqualValues(t, 2, posts[0].GetProp(TranslatorSourceUpdateAtProp))
}

func TestPublishRejectsUnrelatedKnownPostWithoutCreating(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*model.Post)
	}{
		{name: "bot", mutate: func(post *model.Post) { post.UserId = "other-bot" }},
		{name: "channel", mutate: func(post *model.Post) { post.ChannelId = "other-channel" }},
		{name: "kind", mutate: func(post *model.Post) { post.AddProp(TranslatorKindProp, "manual") }},
		{name: "source", mutate: func(post *model.Post) { post.AddProp(TranslatorSourcePostIDProp, "other-source") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newPublicationTestAPI()
			existing := automaticPost("known-post", testOperationID, 2, 500)
			test.mutate(existing)
			api.seedPost(existing)
			publisher := NewPublisher(api, &deletionIntentRecorder{})
			request := automaticRequest()
			request.KnownPostID = existing.Id

			_, err := publisher.Publish(context.Background(), request)

			require.ErrorIs(t, err, ErrReconcileUncertain)
			require.Len(t, api.allVisiblePosts(), 1)
			require.Equal(t, existing.Message, api.post(existing.Id).Message)
		})
	}
}

func TestPublishRejectsKnownAutomaticRequestWithoutMappedSourceIdentity(t *testing.T) {
	api := newPublicationTestAPI()
	existing := automaticPost("known-post", testOperationID, 2, 500)
	existing.DelProp(TranslatorSourcePostIDProp)
	existing.DelProp(TranslatorSourceUpdateAtProp)
	api.seedPost(existing)
	publisher := NewPublisher(api, &deletionIntentRecorder{})
	request := automaticRequest()
	delete(request.Props, TranslatorSourcePostIDProp)
	delete(request.Props, TranslatorSourceUpdateAtProp)
	request.KnownPostID = existing.Id

	_, err := publisher.Publish(context.Background(), request)

	require.ErrorIs(t, err, ErrReconcileUncertain)
	posts := api.allVisiblePosts()
	require.Len(t, posts, 1)
	require.Equal(t, existing.Id, posts[0].Id)
}

func TestPublishDoesNotCreateWhenUpdateAcknowledgementCannotBeConfirmed(t *testing.T) {
	api := newPublicationTestAPI()
	existing := automaticPost("known-post", "operation-1", 1, 500)
	existing.Message = "old translation"
	api.seedPost(existing)
	api.updateErrorBeforeStore = true
	publisher := NewPublisher(api, &deletionIntentRecorder{})
	request := automaticRequest()
	request.KnownPostID = existing.Id

	_, err := publisher.Publish(context.Background(), request)

	require.ErrorIs(t, err, ErrReconcileUncertain)
	posts := api.allVisiblePosts()
	require.Len(t, posts, 1)
	require.Equal(t, existing.Id, posts[0].Id)
	require.Equal(t, "old translation", posts[0].Message)
	require.Empty(t, api.visiblePostsForOperation(testOperationID))
}

func TestPublishScansAndCreatesWhenKnownPostWasDeleted(t *testing.T) {
	api := newPublicationTestAPI()
	deleted := automaticPost("deleted-known-post", "operation-1", 1, 500)
	deleted.DeleteAt = 800
	api.seedPost(deleted)
	publisher := NewPublisher(api, &deletionIntentRecorder{})
	request := automaticRequest()
	request.KnownPostID = deleted.Id

	result, err := publisher.Publish(context.Background(), request)

	require.NoError(t, err)
	require.NotEqual(t, deleted.Id, result.PostID)
	posts := api.visiblePostsForOperation(testOperationID)
	require.Len(t, posts, 1)
	require.Equal(t, result.PostID, posts[0].Id)
}

func TestReconcileAdoptsExistingOperationAndReturnsUncertainWhenAbsent(t *testing.T) {
	api := newPublicationTestAPI()
	existing := automaticPost("existing-post", testOperationID, 2, 2_000)
	api.seedPost(existing)
	publisher := NewPublisher(api, &deletionIntentRecorder{})
	operation := Operation{
		ID:        testOperationID,
		StartedAt: testStartedAt,
		ChannelID: testChannelID,
		UserID:    testBotID,
	}

	result, err := publisher.Reconcile(context.Background(), operation)

	require.NoError(t, err)
	require.Equal(t, existing.Id, result.PostID)

	operation.ID = "absent-operation"
	_, err = publisher.Reconcile(context.Background(), operation)
	require.ErrorIs(t, err, ErrReconcileUncertain)
	require.Empty(t, api.visiblePostsForOperation(operation.ID))
}

func TestPublishDoesNotDeleteDuplicateWhenIntentPersistenceFails(t *testing.T) {
	api := newPublicationTestAPI()
	api.seedPost(automaticPost("winner", testOperationID, 2, 2_000))
	api.seedPost(automaticPost("duplicate", testOperationID, 2, 2_100))
	intents := &deletionIntentRecorder{err: errors.New("kv unavailable")}
	publisher := NewPublisher(api, intents)

	_, err := publisher.Publish(context.Background(), automaticRequest())

	require.ErrorIs(t, err, ErrReconcileUncertain)
	posts := api.visiblePostsForOperation(testOperationID)
	require.Len(t, posts, 2)
}

func automaticRequest() Request {
	return Request{
		OperationID:        testOperationID,
		OperationStartedAt: testStartedAt,
		ChannelID:          testChannelID,
		RootID:             "root-1",
		UserID:             testBotID,
		Message:            "translated message",
		Props: model.StringInterface{
			TranslatorKindProp:                "automatic",
			TranslatorSourcePostIDProp:        testSourceID,
			TranslatorSourceUpdateAtProp:      int64(2),
			TranslatorModelProp:               "new-model",
			model.PostPropsSilentNotification: true,
		},
	}
}

func publicationGuardForRequest(request Request) domain.AutomaticPublicationGuard {
	return domain.AutomaticPublicationGuard{
		TranslationPostID: request.KnownPostID,
		SourcePostID:      fmt.Sprint(request.Props[TranslatorSourcePostIDProp]),
		SourceUpdateAt:    2,
		OperationID:       request.OperationID,
		LeaseUntil:        testStartedAt.Add(time.Minute),
	}
}

type allowingPublicationGuardRepository struct{}

func (*allowingPublicationGuardRepository) AcquireAutomaticPublicationGuard(
	desired domain.AutomaticPublicationGuard,
	_ time.Time,
) (domain.AutomaticPublicationGuard, bool, error) {
	return desired, true, nil
}

func (*allowingPublicationGuardRepository) BeginAutomaticPublicationUpdate(
	before domain.AutomaticPublicationGuard,
) (domain.AutomaticPublicationGuard, bool, error) {
	before.Updating = true
	return before, true, nil
}

func (*allowingPublicationGuardRepository) ReleaseAutomaticPublicationGuard(
	domain.AutomaticPublicationGuard,
) (bool, error) {
	return true, nil
}

func automaticPost(id, operationID string, sourceUpdateAt, createAt int64) *model.Post {
	post := &model.Post{
		Id:        id,
		CreateAt:  createAt,
		UserId:    testBotID,
		ChannelId: testChannelID,
		RootId:    "root-1",
		Message:   "existing translation",
	}
	post.SetProps(model.StringInterface{
		TranslatorKindProp:           "automatic",
		TranslatorOperationIDProp:    operationID,
		TranslatorSourcePostIDProp:   testSourceID,
		TranslatorSourceUpdateAtProp: sourceUpdateAt,
		TranslatorModelProp:          "model",
	})
	return post
}

type recordedDeletionIntent struct {
	intent          domain.DeletionIntent
	expireInSeconds int64
}

type deletionIntentRecorder struct {
	mu      sync.Mutex
	records map[string]recordedDeletionIntent
	err     error
}

func (r *deletionIntentRecorder) PutDeletionIntent(intent domain.DeletionIntent, expireInSeconds int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if r.records == nil {
		r.records = make(map[string]recordedDeletionIntent)
	}
	r.records[intent.TranslationPostID] = recordedDeletionIntent{
		intent:          intent,
		expireInSeconds: expireInSeconds,
	}
	return nil
}

func (r *deletionIntentRecorder) snapshot() map[string]recordedDeletionIntent {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := make(map[string]recordedDeletionIntent, len(r.records))
	for id, record := range r.records {
		snapshot[id] = record
	}
	return snapshot
}

type publicationTestAPI struct {
	mu                         sync.Mutex
	posts                      map[string]*model.Post
	nextID                     int
	nextCreateAt               int64
	scanErrorPage              int
	createErrorBeforeStoreOnce bool
	createStoreThenError       bool
	updateStoreThenError       bool
	updateErrorBeforeStore     bool
}

func newPublicationTestAPI() *publicationTestAPI {
	return &publicationTestAPI{
		posts:         make(map[string]*model.Post),
		nextCreateAt:  2_000,
		scanErrorPage: -1,
	}
}

func (a *publicationTestAPI) CreatePost(post *model.Post) (*model.Post, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if post.Id != "" {
		return nil, publicationAppError("caller supplied a post ID", http.StatusBadRequest)
	}
	if a.createErrorBeforeStoreOnce {
		a.createErrorBeforeStoreOnce = false
		return nil, publicationAppError("create failed before storage", http.StatusInternalServerError)
	}
	created := post.Clone()
	a.nextID++
	created.Id = fmt.Sprintf("mattermost-%03d", a.nextID)
	created.CreateAt = a.nextCreateAt
	a.nextCreateAt++
	a.posts[created.Id] = created.Clone()
	if a.createStoreThenError {
		a.createStoreThenError = false
		return nil, publicationAppError("create acknowledgement lost", http.StatusInternalServerError)
	}
	return created.Clone(), nil
}

func (a *publicationTestAPI) UpdatePost(post *model.Post) (*model.Post, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.updateErrorBeforeStore {
		return nil, publicationAppError("update failed", http.StatusInternalServerError)
	}
	current := a.posts[post.Id]
	if current == nil || current.DeleteAt != 0 {
		return nil, publicationAppError("post not found", http.StatusNotFound)
	}
	updated := post.Clone()
	updated.CreateAt = current.CreateAt
	updated.UpdateAt = current.UpdateAt + 1
	a.posts[updated.Id] = updated.Clone()
	if a.updateStoreThenError {
		a.updateStoreThenError = false
		return nil, publicationAppError("update acknowledgement lost", http.StatusInternalServerError)
	}
	return updated.Clone(), nil
}

func (a *publicationTestAPI) GetPost(postID string) (*model.Post, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	post := a.posts[postID]
	if post == nil {
		return nil, publicationAppError("post not found", http.StatusNotFound)
	}
	return post.Clone(), nil
}

func (a *publicationTestAPI) GetPostsForChannel(channelID string, page, perPage int) (*model.PostList, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if page == a.scanErrorPage {
		return nil, publicationAppError("channel scan failed", http.StatusInternalServerError)
	}
	posts := make([]*model.Post, 0, len(a.posts))
	for _, post := range a.posts {
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
		list.AddOrder(post.Id)
	}
	return list, nil
}

func (a *publicationTestAPI) DeletePost(postID string) *model.AppError {
	a.mu.Lock()
	defer a.mu.Unlock()
	post := a.posts[postID]
	if post == nil || post.DeleteAt != 0 {
		return publicationAppError("post not found", http.StatusNotFound)
	}
	post.DeleteAt = post.CreateAt + 10_000
	return nil
}

func (a *publicationTestAPI) seedPost(post *model.Post) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.posts[post.Id] = post.Clone()
}

func (a *publicationTestAPI) post(postID string) *model.Post {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.posts[postID] == nil {
		return nil
	}
	return a.posts[postID].Clone()
}

func (a *publicationTestAPI) visiblePostsForOperation(operationID string) []*model.Post {
	posts := a.allVisiblePosts()
	matches := make([]*model.Post, 0, len(posts))
	for _, post := range posts {
		if post.GetProp(TranslatorOperationIDProp) == operationID {
			matches = append(matches, post)
		}
	}
	return matches
}

func (a *publicationTestAPI) allVisiblePosts() []*model.Post {
	a.mu.Lock()
	defer a.mu.Unlock()
	posts := make([]*model.Post, 0, len(a.posts))
	for _, post := range a.posts {
		if post.DeleteAt == 0 {
			posts = append(posts, post.Clone())
		}
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].Id < posts[j].Id })
	return posts
}

func publicationAppError(message string, status int) *model.AppError {
	return model.NewAppError("publicationTestAPI", "publication.test", nil, message, status)
}
