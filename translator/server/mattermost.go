package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/automatic"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/manual"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/store"
)

const mattermostKVPageSize = 200

var errMattermostOperation = errors.New("Mattermost operation failed")

// mattermostAPI is the exact subset of the public v0.4.4 plugin API used by
// this plugin. Keeping it here prevents domain packages from depending on the
// full Mattermost API surface.
type mattermostAPI interface {
	LoadPluginConfiguration(destination any) error
	RegisterCommand(command *model.Command) error
	GetConfig() *model.Config
	LogError(message string, keyValuePairs ...any)

	GetTeamByName(name string) (*model.Team, *model.AppError)
	GetUserByUsername(username string) (*model.User, *model.AppError)
	GetUser(userID string) (*model.User, *model.AppError)
	GetChannel(channelID string) (*model.Channel, *model.AppError)
	GetUsersInChannel(channelID, sortBy string, page, perPage int) ([]*model.User, *model.AppError)
	GetTeamMember(teamID, userID string) (*model.TeamMember, *model.AppError)

	CreatePost(post *model.Post) (*model.Post, *model.AppError)
	UpdatePost(post *model.Post) (*model.Post, *model.AppError)
	GetPost(postID string) (*model.Post, *model.AppError)
	GetPostsForChannel(channelID string, page, perPage int) (*model.PostList, *model.AppError)
	DeletePost(postID string) *model.AppError
	SendEphemeralPost(userID string, post *model.Post) *model.Post
	UpdateEphemeralPost(userID string, post *model.Post) *model.Post
	DeleteEphemeralPost(userID, postID string)

	KVGet(key string) ([]byte, *model.AppError)
	KVSet(key string, value []byte) *model.AppError
	KVSetWithExpiry(key string, value []byte, expireInSeconds int64) *model.AppError
	KVSetWithOptions(key string, value []byte, options model.PluginKVSetOptions) (bool, *model.AppError)
	KVCompareAndDelete(key string, oldValue []byte) (bool, *model.AppError)
	KVDelete(key string) *model.AppError
	KVList(page, perPage int) ([]string, *model.AppError)
}

type mattermostAdapter struct {
	api mattermostAPI
}

func newMattermostAdapter(api mattermostAPI) *mattermostAdapter {
	return &mattermostAdapter{api: api}
}

func (a *mattermostAdapter) RegisterCommand(command *model.Command) error {
	if a == nil || a.api == nil {
		return operationError("register_command", 0)
	}
	if err := a.api.RegisterCommand(command); err != nil {
		return operationError("register_command", 0)
	}
	return nil
}

func (a *mattermostAdapter) LoadPluginConfiguration(destination any) error {
	if a == nil || a.api == nil {
		return operationError("load_plugin_configuration", 0)
	}
	if err := a.api.LoadPluginConfiguration(destination); err != nil {
		return operationError("load_plugin_configuration", 0)
	}
	return nil
}

func (a *mattermostAdapter) GetTeamByName(name string) (*model.Team, *model.AppError) {
	team, appErr := a.api.GetTeamByName(name)
	return team, safeAppError(appErr)
}

func (a *mattermostAdapter) GetUserByUsername(username string) (*model.User, *model.AppError) {
	user, appErr := a.api.GetUserByUsername(username)
	return user, safeAppError(appErr)
}

func (a *mattermostAdapter) GetUser(userID string) (*model.User, *model.AppError) {
	user, appErr := a.api.GetUser(userID)
	return user, safeAppError(appErr)
}

func (a *mattermostAdapter) GetChannel(channelID string) (*model.Channel, *model.AppError) {
	channel, appErr := a.api.GetChannel(channelID)
	return channel, safeAppError(appErr)
}

func (a *mattermostAdapter) GetUsersInChannel(channelID, sortBy string, page, perPage int) ([]*model.User, *model.AppError) {
	users, appErr := a.api.GetUsersInChannel(channelID, sortBy, page, perPage)
	return users, safeAppError(appErr)
}

func (a *mattermostAdapter) GetTeamMember(teamID, userID string) (*model.TeamMember, *model.AppError) {
	membership, appErr := a.api.GetTeamMember(teamID, userID)
	return membership, safeAppError(appErr)
}

func (a *mattermostAdapter) CreatePost(post *model.Post) (*model.Post, *model.AppError) {
	if a == nil || a.api == nil || post == nil {
		return nil, safeAppError(model.NewAppError("mattermostAdapter.CreatePost", "translator.mattermost_api", nil, "", 500))
	}
	publicPost := post.Clone()
	publicPost.Id = ""
	created, appErr := a.api.CreatePost(publicPost)
	return created, safeAppError(appErr)
}

func (a *mattermostAdapter) UpdatePost(post *model.Post) (*model.Post, *model.AppError) {
	updated, appErr := a.api.UpdatePost(post)
	return updated, safeAppError(appErr)
}

func (a *mattermostAdapter) GetPost(postID string) (*model.Post, *model.AppError) {
	post, appErr := a.api.GetPost(postID)
	return post, safeAppError(appErr)
}

func (a *mattermostAdapter) GetPostsForChannel(channelID string, page, perPage int) (*model.PostList, *model.AppError) {
	posts, appErr := a.api.GetPostsForChannel(channelID, page, perPage)
	return posts, safeAppError(appErr)
}

func (a *mattermostAdapter) DeletePost(postID string) *model.AppError {
	return safeAppError(a.api.DeletePost(postID))
}

func (a *mattermostAdapter) SendEphemeralPost(userID string, post *model.Post) *model.Post {
	if a == nil || a.api == nil || post == nil {
		return nil
	}
	return a.api.SendEphemeralPost(userID, a.mentionEphemeralRecipient(userID, post))
}

func (a *mattermostAdapter) UpdateEphemeralPost(userID string, post *model.Post) *model.Post {
	if a == nil || a.api == nil || post == nil {
		return nil
	}
	return a.api.UpdateEphemeralPost(userID, a.mentionEphemeralRecipient(userID, post))
}

func (a *mattermostAdapter) mentionEphemeralRecipient(userID string, post *model.Post) *model.Post {
	user, appErr := a.api.GetUser(userID)
	if appErr != nil || user == nil || user.Username == "" {
		return post
	}
	updated := post.Clone()
	mention := "@" + user.Username
	if updated.Message != mention && !strings.HasPrefix(updated.Message, mention+"\n") {
		updated.Message = strings.TrimSpace(mention + "\n" + updated.Message)
	}
	return updated
}

func (a *mattermostAdapter) DeleteEphemeralPost(userID, postID string) {
	a.api.DeleteEphemeralPost(userID, postID)
}

func (a *mattermostAdapter) Get(key string) ([]byte, error) {
	value, appErr := a.api.KVGet(key)
	if appErr != nil {
		return nil, operationError("kv_get", appErr.StatusCode)
	}
	return append([]byte(nil), value...), nil
}

func (a *mattermostAdapter) Set(key string, value []byte) error {
	if appErr := a.api.KVSet(key, value); appErr != nil {
		return operationError("kv_set", appErr.StatusCode)
	}
	return nil
}

func (a *mattermostAdapter) SetWithExpiry(key string, value []byte, expireInSeconds int64) error {
	if appErr := a.api.KVSetWithExpiry(key, value, expireInSeconds); appErr != nil {
		return operationError("kv_set_with_expiry", appErr.StatusCode)
	}
	return nil
}

func (a *mattermostAdapter) CompareAndSet(key string, oldValue, newValue []byte) (bool, error) {
	stored, appErr := a.api.KVSetWithOptions(key, newValue, model.PluginKVSetOptions{
		Atomic:          true,
		OldValue:        oldValue,
		ExpireInSeconds: 0,
	})
	if appErr != nil {
		return false, operationError("kv_compare_and_set", appErr.StatusCode)
	}
	return stored, nil
}

func (a *mattermostAdapter) CompareAndSetWithExpiry(
	key string,
	oldValue, newValue []byte,
	expireInSeconds int64,
) (bool, error) {
	stored, appErr := a.api.KVSetWithOptions(key, newValue, model.PluginKVSetOptions{
		Atomic:          true,
		OldValue:        oldValue,
		ExpireInSeconds: expireInSeconds,
	})
	if appErr != nil {
		return false, operationError("kv_compare_and_set_with_expiry", appErr.StatusCode)
	}
	return stored, nil
}

func (a *mattermostAdapter) CompareAndDelete(key string, oldValue []byte) (bool, error) {
	deleted, appErr := a.api.KVCompareAndDelete(key, oldValue)
	if appErr != nil {
		return false, operationError("kv_compare_and_delete", appErr.StatusCode)
	}
	return deleted, nil
}

func (a *mattermostAdapter) Delete(key string) error {
	if appErr := a.api.KVDelete(key); appErr != nil {
		return operationError("kv_delete", appErr.StatusCode)
	}
	return nil
}

// List must not page the filtered result by paging the unfiltered Mattermost
// result. Mattermost KVList has no prefix argument, so every underlying page is
// collected first, then filtered, sorted, and finally sliced.
func (a *mattermostAdapter) List(prefix string, page, perPage int) ([]string, error) {
	if page < 0 || perPage < 1 {
		return nil, operationError("kv_list", 0)
	}
	all := make([]string, 0, mattermostKVPageSize)
	for underlyingPage := 0; ; underlyingPage++ {
		keys, appErr := a.api.KVList(underlyingPage, mattermostKVPageSize)
		if appErr != nil {
			return nil, operationError("kv_list", appErr.StatusCode)
		}
		all = append(all, keys...)
		if len(keys) < mattermostKVPageSize {
			break
		}
	}

	seen := make(map[string]struct{}, len(all))
	filtered := make([]string, 0, len(all))
	for _, key := range all {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		filtered = append(filtered, key)
	}
	sort.Strings(filtered)
	start := page * perPage
	if start >= len(filtered) {
		return []string{}, nil
	}
	end := start + perPage
	if end > len(filtered) {
		end = len(filtered)
	}
	return append([]string(nil), filtered[start:end]...), nil
}

func operationError(operation string, status int) error {
	if status > 0 {
		return fmt.Errorf("%w: operation=%s status=%d", errMattermostOperation, operation, status)
	}
	return fmt.Errorf("%w: operation=%s", errMattermostOperation, operation)
}

func safeAppError(appErr *model.AppError) *model.AppError {
	if appErr == nil {
		return nil
	}
	return model.NewAppError(
		"mattermostAdapter",
		"translator.mattermost_api_error",
		nil,
		"",
		appErr.StatusCode,
	)
}

var _ manual.API = (*mattermostAdapter)(nil)
var _ automatic.ScopeAPI = (*mattermostAdapter)(nil)
var _ automatic.LifecycleAPI = (*mattermostAdapter)(nil)
var _ publication.PublicationAPI = (*mattermostAdapter)(nil)
var _ store.KV = (*mattermostAdapter)(nil)
var _ ConfigurationAPI = (*mattermostAdapter)(nil)
var _ configurationLoader = (*mattermostAdapter)(nil)
var _ commandRegistrar = (*mattermostAdapter)(nil)
