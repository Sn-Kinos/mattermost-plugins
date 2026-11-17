package main

import (
	"net/http"
	"sort"

	"github.com/mattermost/mattermost/server/public/model"
)

const maxPosts = 200

// The Calls plugin keeps its own table of calls keyed by channel.
const postTypeCall = "custom_calls"

type failure struct {
	status  int
	message string
}

func (f *failure) Error() string { return f.message }

func reject(status int, message string) error { return &failure{status, message} }

// request names the posts and exactly one destination: a thread, through
// any post in it, or a channel.
type request struct {
	PostIDs   []string `json:"post_ids"`
	TargetID  string   `json:"target_id"`
	ChannelID string   `json:"channel_id"`
}

// relocation gives one existing post its new root ("" for a root) and
// CreateAt.
type relocation struct {
	ID       string
	RootID   string
	CreateAt int64
}

// plan is everything the database change needs: the posts to move, oldest
// first, and the threads whose bookkeeping changes with them. From and To are
// the same channel for a move within one channel.
type plan struct {
	From, To string
	ToTeamID string
	// RootID is the target thread, "" when the posts go to a channel.
	RootID     string
	RootUserID string
	Moves      []relocation
	// FormerRoots were thread roots and become replies, so their own thread
	// records go away.
	FormerRoots []string
	// MovedRoots stay roots and take their whole thread to another channel.
	MovedRoots []string
	// SourceRoots lose the replies that move out of them.
	SourceRoots []string
	// Authors of the moved posts, who now take part in the root's thread.
	Authors []string
}

// store changes the database.
type store interface {
	// Relocate applies the plan in one transaction.
	Relocate(p plan) error
	// ForgetEdits removes edit-history rows of postID saved since then.
	ForgetEdits(postID string, since int64) error
	// NewestRoot returns the channel's newest live root post that can be
	// edited, or "".
	NewestRoot(channelID string) (string, error)
}

// move puts the selected posts, and the replies of any selected root, under
// the target's thread or into another channel without creating or deleting
// a post. Mattermost's API cannot change a post's root or channel, so store
// does it in the database.
func move(api API, db store, userID string, req request) (plan, error) {
	if len(req.PostIDs) == 0 || len(req.PostIDs) > maxPosts {
		return plan{}, reject(http.StatusBadRequest, "옮길 메시지를 1개에서 200개 사이로 골라 주세요.")
	}
	if (req.TargetID == "") == (req.ChannelID == "") {
		return plan{}, reject(http.StatusBadRequest, "옮길 곳을 글타래나 채널 하나로 골라 주세요.")
	}
	first, appErr := api.GetPost(req.PostIDs[0])
	if appErr != nil {
		return plan{}, reject(http.StatusBadRequest, "선택한 메시지 중 사라진 것이 있습니다.")
	}

	var root *model.Post
	toID := req.ChannelID
	if req.TargetID != "" {
		target, appErr := api.GetPost(req.TargetID)
		if appErr != nil {
			return plan{}, reject(http.StatusBadRequest, "답글을 넣을 메시지를 찾지 못했습니다.")
		}
		root = target
		if target.RootId != "" {
			if root, appErr = api.GetPost(target.RootId); appErr != nil {
				return plan{}, reject(http.StatusBadRequest, "답글을 넣을 글타래를 찾지 못했습니다.")
			}
		}
		if root.IsSystemMessage() {
			return plan{}, reject(http.StatusBadRequest, "시스템 메시지에는 답글을 넣을 수 없습니다.")
		}
		toID = root.ChannelId
	} else if toID == first.ChannelId {
		return plan{}, reject(http.StatusBadRequest, "이미 이 채널에 있는 메시지입니다.")
	}

	from, to, err := channels(api, userID, first.ChannelId, toID)
	if err != nil {
		return plan{}, err
	}
	rootID := ""
	if root != nil {
		rootID = root.Id
	}
	posts, err := collect(api, userID, from.Id, rootID, req.PostIDs)
	if err != nil {
		return plan{}, err
	}
	if from.Id != to.Id {
		for _, post := range posts {
			if post.Type == postTypeCall {
				return plan{}, reject(http.StatusBadRequest, "통화 메시지는 다른 채널로 옮길 수 없습니다.")
			}
		}
	}

	p := planMove(from, to, root, posts)
	if err := db.Relocate(p); err != nil {
		return plan{}, reject(http.StatusInternalServerError, "메시지를 옮기지 못했습니다. 아무것도 바뀌지 않았습니다.")
	}

	if p.RootID != "" {
		resave(api, db, p.RootID)
	} else {
		resave(api, db, newestRoot(p.Moves))
	}
	if p.From != p.To {
		if id, err := db.NewestRoot(p.From); err == nil && id != "" {
			resave(api, db, id)
		}
	}

	ids := make([]string, len(p.Moves))
	for i, m := range p.Moves {
		ids[i] = m.ID
	}
	// From's members may not see To, so they only learn what left.
	if p.From != p.To {
		api.PublishWebSocketEvent("moved_out", map[string]any{"post_ids": ids, "source_roots": p.SourceRoots},
			&model.WebsocketBroadcast{ChannelId: p.From})
	}
	api.PublishWebSocketEvent("moved", map[string]any{
		"root_id": p.RootID, "channel_id": p.To, "post_ids": ids, "source_roots": p.SourceRoots,
	}, &model.WebsocketBroadcast{ChannelId: p.To})
	return p, nil
}

// channels loads both channels and checks that userID may take posts out of
// one and write them into the other.
func channels(api API, userID, fromID, toID string) (*model.Channel, *model.Channel, error) {
	from, appErr := api.GetChannel(fromID)
	if appErr != nil {
		return nil, nil, reject(http.StatusBadRequest, "선택한 메시지의 채널을 찾지 못했습니다.")
	}
	to, appErr := api.GetChannel(toID)
	if appErr != nil {
		return nil, nil, reject(http.StatusBadRequest, "옮길 채널을 찾지 못했습니다.")
	}
	switch {
	case from.DeleteAt != 0 || to.DeleteAt != 0:
		return nil, nil, reject(http.StatusBadRequest, "보관된 채널에서는 메시지를 옮길 수 없습니다.")
	case from.IsShared() || to.IsShared():
		return nil, nil, reject(http.StatusBadRequest, "공유 채널에서는 메시지를 옮길 수 없습니다.")
	case from.TeamId != "" && to.TeamId != "" && from.TeamId != to.TeamId:
		return nil, nil, reject(http.StatusBadRequest, "같은 팀의 채널로만 옮길 수 있습니다.")
	}
	if _, appErr := api.GetChannelMember(from.Id, userID); appErr != nil {
		return nil, nil, reject(http.StatusForbidden, "메시지가 있는 채널의 멤버만 옮길 수 있습니다.")
	}
	if _, appErr := api.GetChannelMember(to.Id, userID); appErr != nil ||
		!api.HasPermissionToChannel(userID, to.Id, model.PermissionCreatePost) {
		return nil, nil, reject(http.StatusForbidden, "이 채널에 글을 쓸 권한이 없습니다.")
	}
	return from, to, nil
}

// resave saves a post unchanged. Mattermost keeps each channel's latest post
// time in a cache for 15 minutes and answers reloads from it with 304 Not
// Modified; this is the plugin API's only way to clear it. The save also
// leaves an edit-history row, which goes again.
func resave(api API, db store, postID string) {
	post, appErr := api.GetPost(postID)
	if appErr != nil {
		return
	}
	since := model.GetMillis()
	if _, appErr := api.UpdatePost(post); appErr == nil {
		_ = db.ForgetEdits(postID, since)
	}
}

// newestRoot returns the last post of moves, oldest first, that is a root.
func newestRoot(moves []relocation) string {
	for i := len(moves) - 1; i >= 0; i-- {
		if moves[i].RootID == "" {
			return moves[i].ID
		}
	}
	return ""
}

// planMove takes posts oldest first. Into a thread, every post goes under
// root, and since a thread reads top down, posts older than the root move to
// just after it, keeping their order. Into a channel (root nil), posts keep
// their time and their thread when its root moves too; other replies become
// roots.
func planMove(from, to *model.Channel, root *model.Post, posts []*model.Post) plan {
	p := plan{From: from.Id, To: to.Id, ToTeamID: to.TeamId}
	moving := map[string]bool{}
	for _, post := range posts {
		moving[post.Id] = true
	}
	sources := map[string]bool{}
	authors := map[string]bool{}
	var createAt int64
	if root != nil {
		p.RootID, p.RootUserID = root.Id, root.UserId
		createAt = root.CreateAt
	}
	for _, post := range posts {
		m := relocation{ID: post.Id, RootID: p.RootID, CreateAt: post.CreateAt}
		switch {
		case root != nil:
			createAt = max(createAt+1, post.CreateAt)
			m.CreateAt = createAt
			authors[post.UserId] = true
		case moving[post.RootId]:
			m.RootID = post.RootId
		}
		p.Moves = append(p.Moves, m)
		switch {
		case post.RootId == "" && root != nil:
			p.FormerRoots = append(p.FormerRoots, post.Id)
		case post.RootId == "":
			p.MovedRoots = append(p.MovedRoots, post.Id)
		case !moving[post.RootId]:
			sources[post.RootId] = true
		}
	}
	p.SourceRoots = sortedKeys(sources)
	if root != nil {
		p.Authors = sortedKeys(authors)
	}
	return p
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// collect loads the selected posts plus the replies of any selected root,
// checks each one, and returns them oldest first.
func collect(api API, userID, channelID, rootID string, postIDs []string) ([]*model.Post, error) {
	seen := map[string]bool{}
	var posts []*model.Post
	add := func(post *model.Post) error {
		if seen[post.Id] {
			return nil
		}
		switch {
		case rootID != "" && (post.Id == rootID || post.RootId == rootID):
			return reject(http.StatusBadRequest, "이미 이 글타래에 있는 메시지는 옮길 수 없습니다.")
		case post.ChannelId != channelID:
			return reject(http.StatusBadRequest, "같은 채널의 메시지만 옮길 수 있습니다.")
		case post.IsSystemMessage() || post.Type == model.PostTypeBurnOnRead:
			return reject(http.StatusBadRequest, "시스템 메시지와 읽으면 사라지는 메시지는 옮길 수 없습니다.")
		}
		permission := model.PermissionDeleteOthersPosts
		if post.UserId == userID {
			permission = model.PermissionDeletePost
		}
		if !api.HasPermissionToChannel(userID, channelID, permission) {
			return reject(http.StatusForbidden, "지울 권한이 없는 메시지는 옮길 수 없습니다.")
		}
		seen[post.Id] = true
		posts = append(posts, post)
		return nil
	}

	for _, id := range postIDs {
		post, appErr := api.GetPost(id)
		if appErr != nil {
			return nil, reject(http.StatusBadRequest, "선택한 메시지 중 사라진 것이 있습니다.")
		}
		if err := add(post); err != nil {
			return nil, err
		}
		if post.RootId != "" {
			continue
		}
		thread, appErr := api.GetPostThread(post.Id)
		if appErr != nil {
			return nil, reject(http.StatusInternalServerError, "선택한 메시지의 답글을 불러오지 못했습니다.")
		}
		for _, reply := range thread.Posts {
			if reply.RootId == post.Id {
				if err := add(reply); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(posts) > maxPosts {
		return nil, reject(http.StatusBadRequest, "답글까지 합쳐 200개가 넘어 한 번에 옮길 수 없습니다.")
	}
	sort.SliceStable(posts, func(i, j int) bool {
		if posts[i].CreateAt != posts[j].CreateAt {
			return posts[i].CreateAt < posts[j].CreateAt
		}
		return posts[i].Id < posts[j].Id
	})
	return posts, nil
}
