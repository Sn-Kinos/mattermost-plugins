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
	direct  = "direct00000000000000000000"
	faraway = "faraway0000000000000000000"
	team    = "team000000000000000000000a"
	farTeam = "team000000000000000000000b"
)

type fakeAPI struct {
	posts     map[string]*model.Post
	channels  map[string]*model.Channel
	members   map[string]bool
	revoked   map[string]bool
	events    []map[string]any
	plans     []plan
	updated   []string
	forgotten []string
	failStore bool
}

func newFake() *fakeAPI {
	return &fakeAPI{
		posts: map[string]*model.Post{},
		channels: map[string]*model.Channel{
			channel: {Id: channel, TeamId: team, Type: model.ChannelTypeOpen},
			other:   {Id: other, TeamId: team, Type: model.ChannelTypePrivate},
			direct:  {Id: direct, Type: model.ChannelTypeDirect},
			faraway: {Id: faraway, TeamId: farTeam, Type: model.ChannelTypeOpen},
		},
		members: map[string]bool{channel: true, other: true, direct: true, faraway: true},
		revoked: map[string]bool{},
	}
}

func (f *fakeAPI) add(id, user, ch, root, message string, createAt int64) *model.Post {
	post := &model.Post{Id: id, UserId: user, ChannelId: ch, RootId: root, Message: message, CreateAt: createAt}
	f.posts[id] = post
	return post
}

// revoke takes permission away from me in one channel only.
func (f *fakeAPI) revoke(channelID, permission string) { f.revoked[channelID+" "+permission] = true }

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

func (f *fakeAPI) GetChannel(id string) (*model.Channel, *model.AppError) {
	ch, ok := f.channels[id]
	if !ok {
		return nil, model.NewAppError("GetChannel", "not_found", nil, "", http.StatusNotFound)
	}
	return ch.DeepCopy(), nil
}

func (f *fakeAPI) GetChannelMember(channelID, userID string) (*model.ChannelMember, *model.AppError) {
	if userID != me || !f.members[channelID] {
		return nil, model.NewAppError("GetChannelMember", "not_found", nil, "", http.StatusNotFound)
	}
	return &model.ChannelMember{ChannelId: channelID, UserId: userID}, nil
}

func (f *fakeAPI) HasPermissionToChannel(userID, channelID string, permission *model.Permission) bool {
	return userID == me && !f.revoked[channelID+" "+permission.Id]
}

func (f *fakeAPI) PublishWebSocketEvent(event string, payload map[string]any, broadcast *model.WebsocketBroadcast) {
	f.events = append(f.events, map[string]any{"event": event, "broadcast": broadcast.ChannelId, "data": payload})
}

func (f *fakeAPI) UpdatePost(post *model.Post) (*model.Post, *model.AppError) {
	f.updated = append(f.updated, post.Id+":"+post.Message)
	return post, nil
}

func (f *fakeAPI) ForgetEdits(postID string, since int64) error {
	f.forgotten = append(f.forgotten, postID)
	return nil
}

func (f *fakeAPI) NewestRoot(channelID string) (string, error) {
	var newest *model.Post
	for _, post := range f.posts {
		if post.ChannelId == channelID && post.RootId == "" && post.DeleteAt == 0 && !post.IsSystemMessage() &&
			(newest == nil || post.CreateAt > newest.CreateAt) {
			newest = post
		}
	}
	if newest == nil {
		return "", nil
	}
	return newest.Id, nil
}

// Relocate stands in for the database: it gives each post its planned root,
// channel and time, as the SQL does.
func (f *fakeAPI) Relocate(p plan) error {
	if f.failStore {
		return errors.New("database unavailable")
	}
	for _, m := range p.Moves {
		f.posts[m.ID].RootId = m.RootID
		f.posts[m.ID].ChannelId = p.To
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

// places returns every post as "channel root createAt".
func places(api *fakeAPI) map[string]string {
	out := map[string]string{}
	for id, p := range api.posts {
		out[id] = fmt.Sprintf("%s %s %d", p.ChannelId, p.RootId, p.CreateAt)
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

func channelRequest(t *testing.T, api *fakeAPI, channelID string, ids ...string) *httptest.ResponseRecorder {
	t.Helper()
	return post(t, api, me, map[string]any{"channel_id": channelID, "post_ids": ids})
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
	assert.Equal(t, []map[string]any{{"event": "moved", "broadcast": channel, "data": map[string]any{
		"root_id": "target00000000000000000000", "channel_id": channel,
		"post_ids": []string{"aaaa00000000000000000000aa", "bbbb00000000000000000000bb"}, "source_roots": []string{},
	}}}, api.events)
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
		setup func(api *fakeAPI)
		user  string
		ids   []string
		// to moves into this channel instead of the target thread.
		to     string
		body   map[string]any
		status int
	}{
		"others' post without delete_others_posts": {
			setup:  func(api *fakeAPI) { api.revoke(channel, "delete_others_posts") },
			ids:    []string{"aaaa00000000000000000000aa", "bbbb00000000000000000000bb"},
			status: http.StatusForbidden,
		},
		"own post without delete_post": {
			setup:  func(api *fakeAPI) { api.revoke(channel, "delete_post") },
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
		"a system message as the target": {
			setup:  func(api *fakeAPI) { api.posts["target00000000000000000000"].Type = model.PostTypeJoinChannel },
			ids:    []string{"aaaa00000000000000000000aa"},
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
		"both a target and a channel": {
			body:   map[string]any{"target_id": "target00000000000000000000", "channel_id": other, "post_ids": []string{"aaaa00000000000000000000aa"}},
			status: http.StatusBadRequest,
		},
		"neither a target nor a channel": {
			body:   map[string]any{"post_ids": []string{"aaaa00000000000000000000aa"}},
			status: http.StatusBadRequest,
		},
		"the channel the posts are already in": {
			to:     channel,
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusBadRequest,
		},
		"a channel that does not exist": {
			to:     "nochannel00000000000000000",
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusBadRequest,
		},
		"a channel in another team": {
			to:     faraway,
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusBadRequest,
		},
		"an archived destination": {
			setup:  func(api *fakeAPI) { api.channels[other].DeleteAt = 5 },
			to:     other,
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusBadRequest,
		},
		"an archived source": {
			setup:  func(api *fakeAPI) { api.channels[channel].DeleteAt = 5 },
			to:     other,
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusBadRequest,
		},
		"a shared destination": {
			setup:  func(api *fakeAPI) { api.channels[other].Shared = model.NewPointer(true) },
			to:     other,
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusBadRequest,
		},
		"a shared source": {
			setup:  func(api *fakeAPI) { api.channels[channel].Shared = model.NewPointer(true) },
			to:     other,
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusBadRequest,
		},
		"a thread in an archived channel": {
			setup: func(api *fakeAPI) {
				api.channels[other].DeleteAt = 5
				api.posts["target00000000000000000000"].ChannelId = other
			},
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusBadRequest,
		},
		"not a member of the destination": {
			setup:  func(api *fakeAPI) { api.members[other] = false },
			to:     other,
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusForbidden,
		},
		"not a member of the source": {
			setup:  func(api *fakeAPI) { api.members[channel] = false },
			to:     other,
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusForbidden,
		},
		"no create_post in the destination": {
			setup:  func(api *fakeAPI) { api.revoke(other, "create_post") },
			to:     other,
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusForbidden,
		},
		"no delete_post in the source": {
			setup:  func(api *fakeAPI) { api.revoke(channel, "delete_post") },
			to:     other,
			ids:    []string{"aaaa00000000000000000000aa"},
			status: http.StatusForbidden,
		},
		"no delete_others_posts in the source": {
			setup:  func(api *fakeAPI) { api.revoke(channel, "delete_others_posts") },
			to:     other,
			ids:    []string{"bbbb00000000000000000000bb"},
			status: http.StatusForbidden,
		},
		"a call to another channel": {
			setup: func(api *fakeAPI) {
				api.add("call00000000000000000000aa", me, channel, "", "", 250).Type = "custom_calls"
			},
			to:     other,
			ids:    []string{"call00000000000000000000aa"},
			status: http.StatusBadRequest,
		},
		"a call into a thread in another channel": {
			setup: func(api *fakeAPI) {
				api.add("call00000000000000000000aa", me, channel, "", "", 250).Type = "custom_calls"
				api.posts["target00000000000000000000"].ChannelId = other
			},
			ids:    []string{"call00000000000000000000aa"},
			status: http.StatusBadRequest,
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
			before := places(api)
			user := me
			if tc.user == "-" {
				user = ""
			}
			body := tc.body
			switch {
			case body != nil:
			case tc.to != "":
				body = map[string]any{"channel_id": tc.to, "post_ids": tc.ids}
			default:
				body = map[string]any{"target_id": "target00000000000000000000", "post_ids": tc.ids}
			}

			response := post(t, api, user, body)

			assert.Equal(t, tc.status, response.Code, response.Body.String())
			assert.Equal(t, before, places(api))
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
	assert.Equal(t, channel, p.From)
	assert.Equal(t, channel, p.To)
	assert.Equal(t, team, p.ToTeamID)
	assert.Empty(t, p.MovedRoots)
	assert.Equal(t, []string{"aaaa00000000000000000000aa"}, p.FormerRoots)
	assert.Equal(t, []string{"source0000000000000000000a"}, p.SourceRoots)
	assert.Equal(t, []string{alex, me}, p.Authors)
	require.Len(t, api.events, 1)
	assert.Equal(t, []string{"source0000000000000000000a"}, api.events[0]["data"].(map[string]any)["source_roots"])
	assert.Equal(t, []relocation{
		{ID: "aaaa00000000000000000000aa", RootID: "target00000000000000000000", CreateAt: 200},
		{ID: "aaaareply00000000000000000", RootID: "target00000000000000000000", CreateAt: 300},
		{ID: "sourcereply000000000000000", RootID: "target00000000000000000000", CreateAt: 400},
	}, p.Moves)
}

func TestMoveToChannelKeepsTimesAndThreadShape(t *testing.T) {
	api := newFake()
	api.add("source0000000000000000000a", alex, channel, "", "other topic", 150)
	api.add("aaaa00000000000000000000aa", me, channel, "", "question", 200)
	api.add("aaaareply00000000000000000", alex, channel, "aaaa00000000000000000000aa", "answer", 300)
	api.add("sourcereply000000000000000", alex, channel, "source0000000000000000000a", "stray reply", 400)

	response := channelRequest(t, api, other, "sourcereply000000000000000", "aaaa00000000000000000000aa")

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, `{"channel_id":"otherchannel00000000000000"}`, response.Body.String())
	assert.Equal(t, map[string]string{
		"source0000000000000000000a": "channel0000000000000000000  150",
		"aaaa00000000000000000000aa": "otherchannel00000000000000  200",
		"aaaareply00000000000000000": "otherchannel00000000000000 aaaa00000000000000000000aa 300",
		"sourcereply000000000000000": "otherchannel00000000000000  400",
	}, places(api), "the root keeps its reply, the lone reply becomes a root, and no time changes")
	require.Len(t, api.plans, 1)
	assert.Equal(t, plan{
		From: channel, To: other, ToTeamID: team,
		Moves: []relocation{
			{ID: "aaaa00000000000000000000aa", RootID: "", CreateAt: 200},
			{ID: "aaaareply00000000000000000", RootID: "aaaa00000000000000000000aa", CreateAt: 300},
			{ID: "sourcereply000000000000000", RootID: "", CreateAt: 400},
		},
		MovedRoots:  []string{"aaaa00000000000000000000aa"},
		SourceRoots: []string{"source0000000000000000000a"},
	}, api.plans[0])
}

func TestMoveToChannelTellsTheSourceOnlyWhatLeft(t *testing.T) {
	api := newFake()
	api.add("source0000000000000000000a", alex, channel, "", "other topic", 150)
	api.add("aaaa00000000000000000000aa", me, channel, "", "question", 200)
	api.add("sourcereply000000000000000", alex, channel, "source0000000000000000000a", "stray reply", 400)
	api.add("stays00000000000000000000a", alex, channel, "", "stays", 500)
	api.add("joined00000000000000000000", alex, channel, "", "joined", 600).Type = model.PostTypeJoinChannel

	response := channelRequest(t, api, other, "aaaa00000000000000000000aa", "sourcereply000000000000000")

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	ids := []string{"aaaa00000000000000000000aa", "sourcereply000000000000000"}
	sources := []string{"source0000000000000000000a"}
	assert.Equal(t, []map[string]any{
		{"event": "moved_out", "broadcast": channel, "data": map[string]any{"post_ids": ids, "source_roots": sources}},
		{"event": "moved", "broadcast": other, "data": map[string]any{
			"root_id": "", "channel_id": other, "post_ids": ids, "source_roots": sources,
		}},
	}, api.events)
	assert.Equal(t, []string{"sourcereply000000000000000:stray reply", "stays00000000000000000000a:stays"}, api.updated,
		"the newest moved root and the newest post left behind are saved unchanged so both channels drop their cached listing")
	assert.Equal(t, []string{"sourcereply000000000000000", "stays00000000000000000000a"}, api.forgotten)
}

func TestMoveIntoThreadInAnotherChannel(t *testing.T) {
	api := newFake()
	api.add("target00000000000000000000", alex, other, "", "topic", 100)
	api.add("aaaa00000000000000000000aa", me, channel, "", "question", 200)
	api.add("aaaareply00000000000000000", alex, channel, "aaaa00000000000000000000aa", "answer", 300)
	api.add("stays00000000000000000000a", alex, channel, "", "stays", 500)

	response := moveRequest(t, api, "target00000000000000000000", "aaaa00000000000000000000aa")

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, `{"root_id":"target00000000000000000000"}`, response.Body.String())
	assert.Equal(t, map[string]string{
		"target00000000000000000000": "otherchannel00000000000000  100",
		"aaaa00000000000000000000aa": "otherchannel00000000000000 target00000000000000000000 200",
		"aaaareply00000000000000000": "otherchannel00000000000000 target00000000000000000000 300",
		"stays00000000000000000000a": "channel0000000000000000000  500",
	}, places(api))
	require.Len(t, api.plans, 1)
	assert.Equal(t, channel, api.plans[0].From)
	assert.Equal(t, other, api.plans[0].To)
	assert.Equal(t, []string{"aaaa00000000000000000000aa"}, api.plans[0].FormerRoots)
	ids := []string{"aaaa00000000000000000000aa", "aaaareply00000000000000000"}
	assert.Equal(t, []map[string]any{
		{"event": "moved_out", "broadcast": channel, "data": map[string]any{"post_ids": ids, "source_roots": []string{}}},
		{"event": "moved", "broadcast": other, "data": map[string]any{
			"root_id": "target00000000000000000000", "channel_id": other, "post_ids": ids, "source_roots": []string{},
		}},
	}, api.events)
	assert.Equal(t, []string{"target00000000000000000000:topic", "stays00000000000000000000a:stays"}, api.updated)
}

func TestMoveChecksEachPermissionInTheChannelItBelongsTo(t *testing.T) {
	cases := map[string]struct {
		setup func(api *fakeAPI)
		to    string
	}{
		"delete rights are needed only where the posts are": {
			setup: func(api *fakeAPI) {
				api.revoke(other, "delete_post")
				api.revoke(other, "delete_others_posts")
			},
			to: other,
		},
		"create_post is needed only where the posts go": {
			setup: func(api *fakeAPI) { api.revoke(channel, "create_post") },
			to:    other,
		},
		"from a channel to a direct message": {
			to: direct,
		},
		"from a direct message to a channel of any team": {
			setup: func(api *fakeAPI) {
				api.posts["aaaa00000000000000000000aa"].ChannelId = direct
				api.posts["bbbb00000000000000000000bb"].ChannelId = direct
			},
			to: faraway,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFake()
			api.add("aaaa00000000000000000000aa", me, channel, "", "mine", 200)
			api.add("bbbb00000000000000000000bb", alex, channel, "", "theirs", 300)
			if tc.setup != nil {
				tc.setup(api)
			}

			response := channelRequest(t, api, tc.to, "aaaa00000000000000000000aa", "bbbb00000000000000000000bb")

			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Equal(t, tc.to, api.posts["aaaa00000000000000000000aa"].ChannelId)
			assert.Equal(t, tc.to, api.posts["bbbb00000000000000000000bb"].ChannelId)
			assert.Equal(t, api.channels[tc.to].TeamId, api.plans[0].ToTeamID)
		})
	}
}

func TestMoveKeepsCallsInTheirChannel(t *testing.T) {
	api := newFake()
	api.add("target00000000000000000000", alex, channel, "", "topic", 100)
	api.add("call00000000000000000000aa", me, channel, "", "", 250).Type = "custom_calls"

	response := moveRequest(t, api, "target00000000000000000000", "call00000000000000000000aa")

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, "target00000000000000000000", api.posts["call00000000000000000000aa"].RootId)
}
