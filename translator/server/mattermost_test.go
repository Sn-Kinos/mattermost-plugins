package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestMattermostAdapterListFiltersCompleteUnderlyingKeyspaceBeforePaging(t *testing.T) {
	api := &plugintest.API{}
	underlying := make([]string, 0, 450)
	wanted := make([]string, 0, 225)
	for index := range 225 {
		wantedKey := fmt.Sprintf("mapping/%03d", 224-index)
		underlying = append(underlying, fmt.Sprintf("unrelated/%03d", index), wantedKey)
		wanted = append(wanted, wantedKey)
	}
	api.On("KVList", 0, 200).Return(underlying[:200], nil).Once()
	api.On("KVList", 1, 200).Return(underlying[200:400], nil).Once()
	api.On("KVList", 2, 200).Return(underlying[400:], nil).Once()

	adapter := newMattermostAdapter(api)
	first, err := adapter.List("mapping/", 0, 200)
	require.NoError(t, err)
	require.Len(t, first, 200)
	sort.Strings(wanted)
	require.Equal(t, wanted[:200], first)

	api.On("KVList", 0, 200).Return(underlying[:200], nil).Once()
	api.On("KVList", 1, 200).Return(underlying[200:400], nil).Once()
	api.On("KVList", 2, 200).Return(underlying[400:], nil).Once()
	second, err := adapter.List("mapping/", 1, 200)
	require.NoError(t, err)
	require.Equal(t, wanted[200:], second)
	api.AssertExpectations(t)
}

func TestMattermostAdapterUsesExactAtomicExpiryAndDeleteOperations(t *testing.T) {
	api := &plugintest.API{}
	oldValue := []byte(`{"state":"pending"}`)
	newValue := []byte(`{"state":"publishing"}`)
	api.On("KVSetWithOptions", "candidate/nonce", newValue, model.PluginKVSetOptions{
		Atomic:          true,
		OldValue:        oldValue,
		ExpireInSeconds: 317,
	}).Return(true, nil).Once()
	api.On("KVCompareAndDelete", "candidate/nonce", newValue).Return(true, nil).Once()
	api.On("KVSetWithExpiry", "deletion-intent/post", newValue, int64(300)).Return(nil).Once()
	api.On("KVSetWithOptions", "mapping/source", newValue, model.PluginKVSetOptions{
		Atomic:          true,
		OldValue:        oldValue,
		ExpireInSeconds: 0,
	}).Return(true, nil).Once()
	api.On("KVSet", "job/source", newValue).Return(nil).Once()
	api.On("KVDelete", "preference/user").Return(nil).Once()

	adapter := newMattermostAdapter(api)
	stored, err := adapter.CompareAndSetWithExpiry("candidate/nonce", oldValue, newValue, 317)
	require.NoError(t, err)
	require.True(t, stored)
	deleted, err := adapter.CompareAndDelete("candidate/nonce", newValue)
	require.NoError(t, err)
	require.True(t, deleted)
	require.NoError(t, adapter.SetWithExpiry("deletion-intent/post", newValue, 300))
	stored, err = adapter.CompareAndSet("mapping/source", oldValue, newValue)
	require.NoError(t, err)
	require.True(t, stored)
	require.NoError(t, adapter.Set("job/source", newValue))
	require.NoError(t, adapter.Delete("preference/user"))
	api.AssertExpectations(t)
}

func TestMattermostAdapterClearsCallerSuppliedPublicPostID(t *testing.T) {
	api := &plugintest.API{}
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		return post != nil && post.Id == "" && post.ChannelId == "channel-safe"
	})).Return(&model.Post{Id: "mattermost-generated", ChannelId: "channel-safe"}, nil).Once()

	adapter := newMattermostAdapter(api)
	original := &model.Post{Id: "caller-supplied", ChannelId: "channel-safe"}
	created, appErr := adapter.CreatePost(original)
	require.Nil(t, appErr)
	require.Equal(t, "mattermost-generated", created.Id)
	require.Equal(t, "caller-supplied", original.Id, "the adapter must clear a clone, not mutate its caller")
	api.AssertExpectations(t)
}

func TestMattermostAdapterRedactsAppErrorBodiesAndKeys(t *testing.T) {
	api := &plugintest.API{}
	secret := "AIza-secret source sentence translated text {raw-json}"
	api.On("KVGet", "candidate/private-key").Return([]byte(nil), model.NewAppError(
		"remote", "remote.failure", nil, secret, 503,
	)).Once()
	api.On("GetPost", "post-safe").Return((*model.Post)(nil), model.NewAppError(
		"remote", "remote.failure", nil, secret, 502,
	)).Once()

	adapter := newMattermostAdapter(api)
	_, err := adapter.Get("candidate/private-key")
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret)
	require.NotContains(t, err.Error(), "candidate/private-key")
	_, appErr := adapter.GetPost("post-safe")
	require.NotNil(t, appErr)
	require.Equal(t, 502, appErr.StatusCode)
	require.False(t, strings.Contains(appErr.Error(), secret))
	api.AssertExpectations(t)
}
