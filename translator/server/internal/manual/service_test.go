package manual

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/gemini"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/limit"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
)

const (
	manualCallerID  = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	manualOtherID   = "cccccccccccccccccccccccccc"
	manualBotID     = "dddddddddddddddddddddddddd"
	manualChannelID = "eeeeeeeeeeeeeeeeeeeeeeeeee"
	manualRootID    = "gggggggggggggggggggggggggg"
)

func TestExecutePostsPlaceholderThenEditsInTranslation(t *testing.T) {
	api := newManualTestAPI()
	api.posts[manualRootID] = &model.Post{Id: manualRootID, ChannelId: manualChannelID, UserId: manualOtherID, Message: "Earlier context"}
	translator := &manualTestTranslator{block: make(chan struct{}), started: make(chan struct{})}
	glossary := map[string]string{"배포": "deployment"}
	service := NewService(context.Background(), api, translator, limit.New(1), ServiceConfig{TranslatorUserID: manualBotID, Glossary: glossary})
	defer service.Shutdown()

	response, err := service.Execute(context.Background(), &model.CommandArgs{
		UserId: manualCallerID, ChannelId: manualChannelID, RootId: manualRootID,
		Command: "/trans @alex 배포 검토 부탁드립니다.",
	}, nil)
	require.NoError(t, err)
	require.Nil(t, response)
	<-translator.started
	glossary["배포"] = "mutated"

	created := api.createdPosts()
	require.Len(t, created, 1, "the post appears before the translation is ready")
	require.Equal(t, manualCallerID, created[0].UserId)
	require.Equal(t, manualChannelID, created[0].ChannelId)
	require.Equal(t, manualRootID, created[0].RootId)
	require.Equal(t, "Translating…\n\n> @alex 배포 검토 부탁드립니다.", created[0].Message)

	close(translator.block)
	require.Eventually(t, func() bool {
		return api.post(created[0].Id).Message == "Could you review the deployment?\n\n> @alex 배포 검토 부탁드립니다."
	}, time.Second, time.Millisecond)
	updated := api.post(created[0].Id)
	require.True(t, publication.HasInlineTranslation(updated, "Could you review the deployment?"))
	require.Len(t, api.createdPosts(), 1)
	require.Empty(t, api.ephemeralPosts())

	calls := translator.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, gemini.EnglishInput{Message: "@alex 배포 검토 부탁드립니다.", ParentContext: "Earlier context"}, calls[0].input)
	require.Equal(t, map[string]string{"배포": "deployment"}, calls[0].glossary)
}

func TestExecuteAttachesFilesToPostedMessage(t *testing.T) {
	api := newManualTestAPI()
	service := NewService(context.Background(), api, &manualTestTranslator{}, limit.New(1), ServiceConfig{TranslatorUserID: manualBotID})
	defer service.Shutdown()

	_, err := service.Execute(context.Background(), &model.CommandArgs{
		UserId: manualCallerID, ChannelId: manualChannelID, Command: "/trans 검토해 주세요",
	}, []string{"file1", "file2"})
	require.NoError(t, err)
	service.Shutdown()

	created := api.createdPosts()
	require.Len(t, created, 1)
	require.Equal(t, model.StringArray{"file1", "file2"}, created[0].FileIds)
	require.Equal(t, model.StringArray{"file1", "file2"}, api.post(created[0].Id).FileIds)
}

func TestExecuteFailureRestoresPlainSourceAndReleasesCapacity(t *testing.T) {
	api := newManualTestAPI()
	translator := &manualTestTranslator{err: errors.New("provider failed")}
	gate := limit.New(1)
	service := NewService(context.Background(), api, translator, gate, ServiceConfig{TranslatorUserID: manualBotID})
	defer service.Shutdown()

	_, err := service.Execute(context.Background(), &model.CommandArgs{
		UserId: manualCallerID, ChannelId: manualChannelID, Command: "/trans 검토해 주세요",
	}, nil)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(api.ephemeralPosts()) == 1 }, time.Second, time.Millisecond)

	created := api.createdPosts()
	require.Len(t, created, 1)
	restored := api.post(created[0].Id)
	require.Equal(t, "검토해 주세요", restored.Message)
	require.Nil(t, restored.GetProp(publication.InlineBlockHashProp))
	require.Nil(t, restored.GetProp(publication.InlineLayoutProp))
	failure := api.ephemeralPosts()[0]
	require.Equal(t, manualBotID, failure.UserId)
	require.Contains(t, failure.Message, "원문만 게시했습니다")
	require.Eventually(t, func() bool {
		release, acquired := gate.TryAcquire()
		if acquired {
			release()
		}
		return acquired
	}, time.Second, time.Millisecond)
}

func TestExecuteKeepsCallerEditMadeDuringTranslation(t *testing.T) {
	api := newManualTestAPI()
	translator := &manualTestTranslator{block: make(chan struct{}), started: make(chan struct{})}
	service := NewService(context.Background(), api, translator, limit.New(1), ServiceConfig{TranslatorUserID: manualBotID})
	defer service.Shutdown()

	_, err := service.Execute(context.Background(), &model.CommandArgs{
		UserId: manualCallerID, ChannelId: manualChannelID, Command: "/trans 검토해 주세요",
	}, nil)
	require.NoError(t, err)
	<-translator.started
	edited := api.createdPosts()[0]
	edited.Message = "직접 고쳤습니다"
	_, appErr := api.UpdatePost(edited)
	require.Nil(t, appErr)
	close(translator.block)

	service.Shutdown()
	require.Equal(t, "직접 고쳤습니다", api.post(edited.Id).Message)
	require.Empty(t, api.ephemeralPosts())
}

func TestExecuteReportsUnpostedMessagePrivately(t *testing.T) {
	api := newManualTestAPI()
	translator := &manualTestTranslator{}
	service := NewService(context.Background(), api, translator, limit.New(1), ServiceConfig{TranslatorUserID: manualBotID})
	defer service.Shutdown()

	_, err := service.Execute(context.Background(), &model.CommandArgs{
		UserId: manualCallerID, ChannelId: manualChannelID, RootId: manualRootID, Command: "/trans 검토해 주세요",
	}, nil)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(api.ephemeralPosts()) == 1 }, time.Second, time.Millisecond)

	require.Empty(t, api.createdPosts())
	require.Empty(t, translator.snapshot())
	require.Contains(t, api.ephemeralPosts()[0].Message, "게시하지 못했습니다")
	require.Contains(t, api.ephemeralPosts()[0].Message, "검토해 주세요")
}

func TestExecuteReturnsPrivateCapacityErrorWithoutTranslating(t *testing.T) {
	api := newManualTestAPI()
	translator := &manualTestTranslator{}
	gate := limit.New(1)
	release, acquired := gate.TryAcquire()
	require.True(t, acquired)
	defer release()
	service := NewService(context.Background(), api, translator, gate, ServiceConfig{TranslatorUserID: manualBotID})
	defer service.Shutdown()

	response, err := service.Execute(context.Background(), &model.CommandArgs{
		UserId: manualCallerID, ChannelId: manualChannelID, Command: "/trans 검토해 주세요",
	}, nil)

	require.NoError(t, err)
	require.Equal(t, model.CommandResponseTypeEphemeral, response.ResponseType)
	require.Contains(t, response.Text, "번역 요청이 많습니다")
	require.Empty(t, translator.snapshot())
	require.Empty(t, api.createdPosts())
}

func TestShutdownCancelsInFlightTranslation(t *testing.T) {
	api := newManualTestAPI()
	translator := &manualTestTranslator{started: make(chan struct{}), waitForCancellation: true}
	service := NewService(context.Background(), api, translator, limit.New(1), ServiceConfig{TranslatorUserID: manualBotID})

	_, err := service.Execute(context.Background(), &model.CommandArgs{
		UserId: manualCallerID, ChannelId: manualChannelID, Command: "/trans 검토해 주세요",
	}, nil)
	require.NoError(t, err)
	<-translator.started

	done := make(chan struct{})
	go func() {
		service.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the in-flight translation")
	}
	created := api.createdPosts()
	require.Len(t, created, 1)
	require.Equal(t, "검토해 주세요", api.post(created[0].Id).Message)

	response, err := service.Execute(context.Background(), &model.CommandArgs{
		UserId: manualCallerID, ChannelId: manualChannelID, Command: "/trans 검토해 주세요",
	}, nil)
	require.NoError(t, err)
	require.Equal(t, model.CommandResponseTypeEphemeral, response.ResponseType)
}

type translatorCall struct {
	input    gemini.EnglishInput
	glossary map[string]string
}

type manualTestTranslator struct {
	mu                  sync.Mutex
	calls               []translatorCall
	started             chan struct{}
	block               chan struct{}
	err                 error
	waitForCancellation bool
}

func (translator *manualTestTranslator) TranslateToEnglish(ctx context.Context, input gemini.EnglishInput, glossary map[string]string) (string, error) {
	translator.mu.Lock()
	copied := make(map[string]string, len(glossary))
	for korean, english := range glossary {
		copied[korean] = english
	}
	translator.calls = append(translator.calls, translatorCall{input: input, glossary: copied})
	translator.mu.Unlock()
	if translator.started != nil {
		close(translator.started)
	}
	if translator.waitForCancellation {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if translator.block != nil {
		<-translator.block
	}
	if translator.err != nil {
		return "", translator.err
	}
	return "Could you review the deployment?", nil
}

func (translator *manualTestTranslator) snapshot() []translatorCall {
	translator.mu.Lock()
	defer translator.mu.Unlock()
	return append([]translatorCall(nil), translator.calls...)
}

type manualTestAPI struct {
	mu        sync.Mutex
	posts     map[string]*model.Post
	created   []*model.Post
	ephemeral []*model.Post
}

func newManualTestAPI() *manualTestAPI {
	return &manualTestAPI{posts: make(map[string]*model.Post)}
}

func (api *manualTestAPI) GetPost(postID string) (*model.Post, *model.AppError) {
	api.mu.Lock()
	defer api.mu.Unlock()
	post, found := api.posts[postID]
	if !found {
		return nil, model.NewAppError("manualTestAPI", "not_found", nil, "", 404)
	}
	return post.Clone(), nil
}

func (api *manualTestAPI) CreatePost(post *model.Post) (*model.Post, *model.AppError) {
	api.mu.Lock()
	defer api.mu.Unlock()
	created := post.Clone()
	created.Id = model.NewId()
	api.posts[created.Id] = created
	api.created = append(api.created, created.Clone())
	return created.Clone(), nil
}

func (api *manualTestAPI) UpdatePost(post *model.Post) (*model.Post, *model.AppError) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if _, found := api.posts[post.Id]; !found {
		return nil, model.NewAppError("manualTestAPI", "not_found", nil, "", 404)
	}
	api.posts[post.Id] = post.Clone()
	return post.Clone(), nil
}

func (api *manualTestAPI) post(postID string) *model.Post {
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.posts[postID].Clone()
}

func (api *manualTestAPI) SendEphemeralPost(_ string, post *model.Post) *model.Post {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.ephemeral = append(api.ephemeral, post.Clone())
	return post.Clone()
}

func (api *manualTestAPI) createdPosts() []*model.Post {
	api.mu.Lock()
	defer api.mu.Unlock()
	return append([]*model.Post(nil), api.created...)
}

func (api *manualTestAPI) ephemeralPosts() []*model.Post {
	api.mu.Lock()
	defer api.mu.Unlock()
	return append([]*model.Post(nil), api.ephemeral...)
}
