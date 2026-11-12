package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	me      = "me000000000000000000000000"
	alex    = "alex0000000000000000000000"
	channel = "channel0000000000000000000"
	other   = "otherchannel00000000000000"
)

type fakeAPI struct {
	posts       map[string]*model.Post
	files       map[string]*model.FileInfo
	data        map[string][]byte
	reactions   map[string][]*model.Reaction
	members     map[string]bool
	permissions map[string]bool
	failCreate  int
	created     int
	nextID      int
}

func newFake() *fakeAPI {
	return &fakeAPI{
		posts:       map[string]*model.Post{},
		files:       map[string]*model.FileInfo{},
		data:        map[string][]byte{},
		reactions:   map[string][]*model.Reaction{},
		members:     map[string]bool{channel: true, other: true},
		permissions: map[string]bool{"create_post": true, "delete_post": true, "delete_others_posts": true},
		failCreate:  -1,
	}
}

func (f *fakeAPI) id() string {
	f.nextID++
	return fmt.Sprintf("new%023d", f.nextID)
}

func (f *fakeAPI) add(id, user, ch, root, message string, createAt int64) *model.Post {
	post := &model.Post{Id: id, UserId: user, ChannelId: ch, RootId: root, Message: message, CreateAt: createAt}
	f.posts[id] = post
	return post
}

func (f *fakeAPI) GetPost(id string) (*model.Post, *model.AppError) {
	post, ok := f.posts[id]
	if !ok || post.DeleteAt != 0 {
		return nil, model.NewAppError("GetPost", "not_found", nil, "", http.StatusNotFound)
	}
	return post.Clone(), nil
}

func (f *fakeAPI) GetPostThread(id string) (*model.PostList, *model.AppError) {
	list := model.NewPostList()
	for _, post := range f.posts {
		if post.DeleteAt == 0 && (post.Id == id || post.RootId == id) {
			list.AddPost(post.Clone())
			list.AddOrder(post.Id)
		}
	}
	return list, nil
}

func (f *fakeAPI) GetChannelMember(channelID, userID string) (*model.ChannelMember, *model.AppError) {
	if userID != me || !f.members[channelID] {
		return nil, model.NewAppError("GetChannelMember", "not_found", nil, "", http.StatusNotFound)
	}
	return &model.ChannelMember{ChannelId: channelID, UserId: userID}, nil
}

func (f *fakeAPI) HasPermissionToChannel(userID, _ string, permission *model.Permission) bool {
	return userID == me && f.permissions[permission.Id]
}

func (f *fakeAPI) CreatePost(post *model.Post) (*model.Post, *model.AppError) {
	if f.created == f.failCreate {
		return nil, model.NewAppError("CreatePost", "failed", nil, "", http.StatusInternalServerError)
	}
	f.created++
	saved := post.Clone()
	saved.Id = f.id()
	f.posts[saved.Id] = saved
	return saved.Clone(), nil
}

func (f *fakeAPI) DeletePost(id string) *model.AppError {
	post, ok := f.posts[id]
	if !ok || post.DeleteAt != 0 {
		return model.NewAppError("DeletePost", "not_found", nil, "", http.StatusNotFound)
	}
	post.DeleteAt = 1
	for _, reply := range f.posts {
		if reply.RootId == id {
			reply.DeleteAt = 1
		}
	}
	return nil
}

func (f *fakeAPI) GetReactions(postID string) ([]*model.Reaction, *model.AppError) {
	return f.reactions[postID], nil
}

func (f *fakeAPI) AddReaction(reaction *model.Reaction) (*model.Reaction, *model.AppError) {
	f.reactions[reaction.PostId] = append(f.reactions[reaction.PostId], reaction)
	return reaction, nil
}

func (f *fakeAPI) GetFileInfo(id string) (*model.FileInfo, *model.AppError) {
	return f.files[id], nil
}

func (f *fakeAPI) GetFile(id string) ([]byte, *model.AppError) {
	return f.data[id], nil
}

func (f *fakeAPI) UploadFile(data []byte, channelID, name string) (*model.FileInfo, *model.AppError) {
	info := &model.FileInfo{Id: f.id(), ChannelId: channelID, Name: name}
	f.files[info.Id] = info
	f.data[info.Id] = data
	return info, nil
}

// thread returns the live replies under root as "author:message" in thread order.
func (f *fakeAPI) thread(root string) []string {
	var replies []*model.Post
	for _, post := range f.posts {
		if post.RootId == root && post.DeleteAt == 0 {
			replies = append(replies, post)
		}
	}
	sort.Slice(replies, func(i, j int) bool { return replies[i].CreateAt < replies[j].CreateAt })
	out := []string{}
	for _, post := range replies {
		out = append(out, post.UserId[:2]+":"+post.Message)
	}
	return out
}

func (f *fakeAPI) live(id string) bool {
	return f.posts[id] != nil && f.posts[id].DeleteAt == 0
}

func post(t *testing.T, api *fakeAPI, user string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/move", bytes.NewReader(encoded))
	if user != "" {
		request.Header.Set("Mattermost-User-Id", user)
	}
	recorder := httptest.NewRecorder()
	(&Plugin{api: api}).ServeHTTP(nil, recorder, request)
	return recorder
}

func moveRequest(t *testing.T, api *fakeAPI, target string, ids ...string) *httptest.ResponseRecorder {
	t.Helper()
	return post(t, api, me, map[string]any{"target_id": target, "post_ids": ids})
}

func TestMoveCopiesPostsIntoTargetThreadAndDeletesOriginals(t *testing.T) {
	api := newFake()
	api.add("target00000000000000000000", alex, channel, "", "topic", 100)
	a := api.add("aaaa00000000000000000000aa", me, channel, "", "first", 200)
	a.IsPinned = true
	a.EditAt = 250
	a.AddProp("attachments", "kept")
	a.FileIds = []string{"oldfile0000000000000000000"}
	api.files["oldfile0000000000000000000"] = &model.FileInfo{Id: "oldfile0000000000000000000", Name: "plan.pdf"}
	api.data["oldfile0000000000000000000"] = []byte("pdf")
	api.reactions[a.Id] = []*model.Reaction{{UserId: alex, PostId: a.Id, EmojiName: "+1"}}
	api.add("bbbb00000000000000000000bb", alex, channel, "", "second", 300)

	response := moveRequest(t, api, "target00000000000000000000", "bbbb00000000000000000000bb", "aaaa00000000000000000000aa")

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, `{"root_id":"target00000000000000000000"}`, response.Body.String())
	assert.Equal(t, []string{"me:first", "al:second"}, api.thread("target00000000000000000000"))
	assert.False(t, api.live("aaaa00000000000000000000aa"))
	assert.False(t, api.live("bbbb00000000000000000000bb"))

	var copied *model.Post
	for _, p := range api.posts {
		if p.Message == "first" && p.DeleteAt == 0 {
			copied = p
		}
	}
	require.NotNil(t, copied)
	assert.Equal(t, int64(200), copied.CreateAt)
	assert.Equal(t, int64(250), copied.EditAt)
	assert.True(t, copied.IsPinned)
	assert.Equal(t, "kept", copied.GetProp("attachments"))
	require.Len(t, copied.FileIds, 1)
	assert.NotEqual(t, "oldfile0000000000000000000", copied.FileIds[0])
	assert.Equal(t, "plan.pdf", api.files[copied.FileIds[0]].Name)
	assert.Equal(t, []byte("pdf"), api.data[copied.FileIds[0]])
	require.Len(t, api.reactions[copied.Id], 1)
	assert.Equal(t, alex, api.reactions[copied.Id][0].UserId)
	assert.Equal(t, "+1", api.reactions[copied.Id][0].EmojiName)
}

func TestMoveIntoReplyTargetUsesItsRoot(t *testing.T) {
	api := newFake()
	api.add("root0000000000000000000000", alex, channel, "", "topic", 100)
	api.add("reply000000000000000000000", alex, channel, "root0000000000000000000000", "old reply", 150)
	api.add("aaaa00000000000000000000aa", me, channel, "", "late", 200)

	response := moveRequest(t, api, "reply000000000000000000000", "aaaa00000000000000000000aa")

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, `{"root_id":"root0000000000000000000000"}`, response.Body.String())
	assert.Equal(t, []string{"al:old reply", "me:late"}, api.thread("root0000000000000000000000"))
}

func TestMoveBringsRepliesOfSelectedRoot(t *testing.T) {
	api := newFake()
	api.add("target00000000000000000000", alex, channel, "", "topic", 100)
	api.add("aaaa00000000000000000000aa", me, channel, "", "question", 200)
	api.add("aaaareply00000000000000000", alex, channel, "aaaa00000000000000000000aa", "answer", 400)
	api.add("bbbb00000000000000000000bb", me, channel, "", "aside", 300)

	response := moveRequest(t, api, "target00000000000000000000", "aaaa00000000000000000000aa", "bbbb00000000000000000000bb")

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, []string{"me:question", "me:aside", "al:answer"}, api.thread("target00000000000000000000"))
	assert.Empty(t, api.thread("aaaa00000000000000000000aa"))
}

func TestMoveRejectsWithoutChangingAnything(t *testing.T) {
	cases := map[string]struct {
		setup  func(api *fakeAPI)
		user   string
		ids    []string
		status int
	}{
		"others' post without delete_others_posts": {
			setup:  func(api *fakeAPI) { api.permissions["delete_others_posts"] = false },
			ids:    []string{"aaaa00000000000000000000aa", "bbbb00000000000000000000bb"},
			status: http.StatusForbidden,
		},
		"own post without delete_post": {
			setup:  func(api *fakeAPI) { api.permissions["delete_post"] = false },
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusForbidden,
		},
		"not a channel member": {
			setup:  func(api *fakeAPI) { api.members[channel] = false },
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusForbidden,
		},
		"post in another channel": {
			setup: func(api *fakeAPI) {
				api.add("elsewhere00000000000000000", me, other, "", "far", 250)
			},
			ids:    []string{"aaaa00000000000000000000aa", "elsewhere00000000000000000"},
			status: http.StatusBadRequest,
		},
		"system message": {
			setup: func(api *fakeAPI) {
				api.add("system0000000000000000000a", me, channel, "", "joined", 250).Type = model.PostTypeJoinChannel
			},
			ids:    []string{"system0000000000000000000a"},
			status: http.StatusBadRequest,
		},
		"the target itself": {
			ids:    []string{"aaaa00000000000000000000aa", "target00000000000000000000"},
			status: http.StatusBadRequest,
		},
		"a reply already in the target thread": {
			setup: func(api *fakeAPI) {
				api.add("inside0000000000000000000a", me, channel, "target00000000000000000000", "in", 250)
			},
			ids:    []string{"inside0000000000000000000a"},
			status: http.StatusBadRequest,
		},
		"deleted post": {
			setup:  func(api *fakeAPI) { api.posts["aaaa00000000000000000000aa"].DeleteAt = 5 },
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusBadRequest,
		},
		"nothing selected": {
			ids:    []string{},
			status: http.StatusBadRequest,
		},
		"no user": {
			user:   "-",
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusUnauthorized,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFake()
			api.add("target00000000000000000000", alex, channel, "", "topic", 100)
			api.add("aaaa00000000000000000000aa", me, channel, "", "mine", 200)
			api.add("bbbb00000000000000000000bb", alex, channel, "", "theirs", 300)
			if tc.setup != nil {
				tc.setup(api)
			}
			before := len(api.posts)
			threadBefore := api.thread("target00000000000000000000")
			user := me
			if tc.user == "-" {
				user = ""
			}

			response := post(t, api, user, map[string]any{"target_id": "target00000000000000000000", "post_ids": tc.ids})

			assert.Equal(t, tc.status, response.Code, response.Body.String())
			assert.Len(t, api.posts, before)
			assert.Equal(t, threadBefore, api.thread("target00000000000000000000"))
			assert.Equal(t, 0, api.created)
		})
	}
}

func TestMoveRollsBackCopiesWhenOneFails(t *testing.T) {
	api := newFake()
	api.add("target00000000000000000000", alex, channel, "", "topic", 100)
	api.add("aaaa00000000000000000000aa", me, channel, "", "first", 200)
	api.add("bbbb00000000000000000000bb", me, channel, "", "second", 300)
	api.failCreate = 1

	response := moveRequest(t, api, "target00000000000000000000", "aaaa00000000000000000000aa", "bbbb00000000000000000000bb")

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.Empty(t, api.thread("target00000000000000000000"))
	assert.True(t, api.live("aaaa00000000000000000000aa"))
	assert.True(t, api.live("bbbb00000000000000000000bb"))
}

func TestOnlyPostMoveIsServed(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/move", nil)
	request.Header.Set("Mattermost-User-Id", me)
	(&Plugin{api: newFake()}).ServeHTTP(nil, recorder, request)
	assert.Equal(t, http.StatusNotFound, recorder.Code)
}
