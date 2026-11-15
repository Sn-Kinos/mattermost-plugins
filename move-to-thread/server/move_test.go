package main

import (
	"bytes"
	"encoding/json"
	"errors"
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
	members     map[string]bool
	permissions map[string]bool
	events      []map[string]any
	plans       []plan
	updated     []string
	forgotten   []string
	failStore   bool
}

func newFake() *fakeAPI {
	return &fakeAPI{
		posts:       map[string]*model.Post{},
		members:     map[string]bool{channel: true, other: true},
		permissions: map[string]bool{"create_post": true, "delete_post": true, "delete_others_posts": true},
	}
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

func (f *fakeAPI) PublishWebSocketEvent(event string, payload map[string]any, broadcast *model.WebsocketBroadcast) {
	f.events = append(f.events, map[string]any{"event": event, "channel": broadcast.ChannelId,
		"root_id": payload["root_id"], "post_ids": payload["post_ids"], "source_roots": payload["source_roots"]})
}

func (f *fakeAPI) UpdatePost(post *model.Post) (*model.Post, *model.AppError) {
	f.updated = append(f.updated, post.Id+":"+post.Message)
	return post, nil
}

func (f *fakeAPI) ForgetEdits(postID string, since int64) error {
	f.forgotten = append(f.forgotten, postID)
	return nil
}

// Relocate stands in for the database: it puts each post under the new root
// at the planned time, as the SQL does.
func (f *fakeAPI) Relocate(p plan) error {
	if f.failStore {
		return errors.New("database unavailable")
	}
	for _, m := range p.Moves {
		f.posts[m.ID].RootId = p.RootID
		f.posts[m.ID].CreateAt = m.CreateAt
	}
	f.plans = append(f.plans, p)
	return nil
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

func post(t *testing.T, api *fakeAPI, user string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/move", bytes.NewReader(encoded))
	if user != "" {
		request.Header.Set("Mattermost-User-Id", user)
	}
	recorder := httptest.NewRecorder()
	(&Plugin{api: api, store: api}).ServeHTTP(nil, recorder, request)
	return recorder
}

func moveRequest(t *testing.T, api *fakeAPI, target string, ids ...string) *httptest.ResponseRecorder {
	t.Helper()
	return post(t, api, me, map[string]any{"target_id": target, "post_ids": ids})
}

func TestMoveKeepsThePostsAndChangesOnlyTheirThread(t *testing.T) {
	api := newFake()
	api.add("target00000000000000000000", alex, channel, "", "topic", 100)
	api.add("aaaa00000000000000000000aa", me, channel, "", "first", 200)
	api.add("bbbb00000000000000000000bb", alex, channel, "", "second", 300)

	response := moveRequest(t, api, "target00000000000000000000", "bbbb00000000000000000000bb", "aaaa00000000000000000000aa")

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, `{"root_id":"target00000000000000000000"}`, response.Body.String())
	assert.Equal(t, []string{"me:first", "al:second"}, api.thread("target00000000000000000000"))
	assert.Equal(t, "target00000000000000000000", api.posts["aaaa00000000000000000000aa"].RootId, "the same post, not a copy")
	assert.Equal(t, []string{"target00000000000000000000:topic"}, api.updated, "an unchanged save makes Mattermost drop its cached channel listing")
	assert.Equal(t, []string{"target00000000000000000000"}, api.forgotten, "and the edit-history row that save leaves is removed")
	assert.Len(t, api.posts, 3)
	assert.Equal(t, []map[string]any{{
		"event": "moved", "channel": channel, "root_id": "target00000000000000000000",
		"post_ids": []string{"aaaa00000000000000000000aa", "bbbb00000000000000000000bb"}, "source_roots": []string{},
	}}, api.events)
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
		"burn-on-read message": {
			setup: func(api *fakeAPI) {
				api.add("burn00000000000000000000aa", me, channel, "", "secret", 250).Type = model.PostTypeBurnOnRead
			},
			ids:    []string{"burn00000000000000000000aa"},
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
			assert.Empty(t, api.plans)
			assert.Empty(t, api.events)
			assert.Empty(t, api.updated)
		})
	}
}

func TestMoveReportsADatabaseFailureAndTellsNoOne(t *testing.T) {
	api := newFake()
	api.add("target00000000000000000000", alex, channel, "", "topic", 100)
	api.add("aaaa00000000000000000000aa", me, channel, "", "first", 200)
	api.failStore = true

	response := moveRequest(t, api, "target00000000000000000000", "aaaa00000000000000000000aa")

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.JSONEq(t, `{"error":"메시지를 옮기지 못했습니다. 아무것도 바뀌지 않았습니다."}`, response.Body.String())
	assert.Empty(t, api.events)
}

func TestOnlyPostMoveIsServed(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/move", nil)
	request.Header.Set("Mattermost-User-Id", me)
	(&Plugin{api: newFake(), store: newFake()}).ServeHTTP(nil, recorder, request)
	assert.Equal(t, http.StatusNotFound, recorder.Code)
}

func TestMoveRootWhoseReplyIsOlderThanIt(t *testing.T) {
	api := newFake()
	api.add("target00000000000000000000", alex, channel, "", "topic", 100)
	api.add("aaaa00000000000000000000aa", me, channel, "", "moved earlier", 300)
	api.add("aaaareply00000000000000000", alex, channel, "aaaa00000000000000000000aa", "older reply", 200)

	response := moveRequest(t, api, "target00000000000000000000", "aaaa00000000000000000000aa")

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, []string{"al:older reply", "me:moved earlier"}, api.thread("target00000000000000000000"))
	assert.Empty(t, api.thread("aaaa00000000000000000000aa"))
}

func TestMoveKeepsOlderPostsAfterTheRoot(t *testing.T) {
	api := newFake()
	api.add("root0000000000000000000000", alex, channel, "", "topic", 500)
	api.add("reply000000000000000000000", alex, channel, "root0000000000000000000000", "existing", 700)
	api.add("aaaa00000000000000000000aa", me, channel, "", "much older", 200)
	api.add("bbbb00000000000000000000bb", alex, channel, "", "older", 300)
	api.add("cccc00000000000000000000cc", me, channel, "", "newer", 600)

	response := moveRequest(t, api, "reply000000000000000000000",
		"cccc00000000000000000000cc", "bbbb00000000000000000000bb", "aaaa00000000000000000000aa")

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var times []string
	for _, p := range api.posts {
		if p.RootId == "root0000000000000000000000" && p.DeleteAt == 0 {
			times = append(times, fmt.Sprintf("%d:%s", p.CreateAt, p.Message))
		}
	}
	sort.Strings(times)
	assert.Equal(t, []string{"501:much older", "502:older", "600:newer", "700:existing"}, times)
}

func TestMovePlanNamesEveryThreadToRecount(t *testing.T) {
	api := newFake()
	api.add("target00000000000000000000", alex, channel, "", "topic", 100)
	api.add("aaaa00000000000000000000aa", me, channel, "", "question", 200)
	api.add("aaaareply00000000000000000", alex, channel, "aaaa00000000000000000000aa", "answer", 300)
	api.add("source0000000000000000000a", alex, channel, "", "other topic", 150)
	api.add("sourcereply000000000000000", me, channel, "source0000000000000000000a", "stray reply", 400)

	response := moveRequest(t, api, "target00000000000000000000", "aaaa00000000000000000000aa", "sourcereply000000000000000")

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Len(t, api.plans, 1)
	p := api.plans[0]
	assert.Equal(t, "target00000000000000000000", p.RootID)
	assert.Equal(t, alex, p.RootUserID)
	assert.Equal(t, channel, p.ChannelID)
	assert.Equal(t, []string{"aaaa00000000000000000000aa"}, p.FormerRoots)
	assert.Equal(t, []string{"source0000000000000000000a"}, p.SourceRoots)
	assert.Equal(t, []string{alex, me}, p.Authors)
	require.Len(t, api.events, 1)
	assert.Equal(t, []string{"source0000000000000000000a"}, api.events[0]["source_roots"])
	assert.Equal(t, []relocation{
		{ID: "aaaa00000000000000000000aa", CreateAt: 200},
		{ID: "aaaareply00000000000000000", CreateAt: 300},
		{ID: "sourcereply000000000000000", CreateAt: 400},
	}, p.Moves)
}
