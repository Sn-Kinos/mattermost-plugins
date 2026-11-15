package main

import (
	"net/http"
	"sort"

	"github.com/mattermost/mattermost/server/public/model"
)

const maxPosts = 200

type failure struct {
	status  int
	message string
}

func (f *failure) Error() string { return f.message }

func reject(status int, message string) error { return &failure{status, message} }

// relocation puts one existing post under the new root at CreateAt.
type relocation struct {
	ID       string
	CreateAt int64
}

// plan is everything the database change needs: the posts to move, oldest
// first, and the threads whose bookkeeping changes with them.
type plan struct {
	RootID     string
	RootUserID string
	ChannelID  string
	Moves      []relocation
	// FormerRoots were thread roots and become replies, so their own thread
	// records go away.
	FormerRoots []string
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
}

// move puts the selected posts, and the replies of any selected root, under
// the target's thread without creating or deleting a post. Mattermost's API
// cannot change a post's root, so store does it in the database.
func move(api API, db store, userID, targetID string, postIDs []string) (string, error) {
	if len(postIDs) == 0 || len(postIDs) > maxPosts {
		return "", reject(http.StatusBadRequest, "옮길 메시지를 1개에서 200개 사이로 골라 주세요.")
	}
	target, appErr := api.GetPost(targetID)
	if appErr != nil {
		return "", reject(http.StatusBadRequest, "답글을 넣을 메시지를 찾지 못했습니다.")
	}
	root := target
	if target.RootId != "" {
		if root, appErr = api.GetPost(target.RootId); appErr != nil {
			return "", reject(http.StatusBadRequest, "답글을 넣을 글타래를 찾지 못했습니다.")
		}
	}
	channelID := target.ChannelId
	if _, appErr := api.GetChannelMember(channelID, userID); appErr != nil ||
		!api.HasPermissionToChannel(userID, channelID, model.PermissionCreatePost) {
		return "", reject(http.StatusForbidden, "이 채널에 글을 쓸 권한이 없습니다.")
	}

	posts, err := collect(api, userID, channelID, root.Id, postIDs)
	if err != nil {
		return "", err
	}
	p := planMove(root, posts)
	if err := db.Relocate(p); err != nil {
		return "", reject(http.StatusInternalServerError, "메시지를 옮기지 못했습니다. 아무것도 바뀌지 않았습니다.")
	}

	// Mattermost keeps the channel's latest post time in a cache for 15
	// minutes and answers reloads from it with 304 Not Modified. Saving the
	// root unchanged is the plugin API's only way to clear it; the save also
	// leaves an edit-history row, which goes again.
	since := model.GetMillis()
	if _, appErr := api.UpdatePost(root); appErr == nil {
		_ = db.ForgetEdits(root.Id, since)
	}

	ids := make([]string, len(p.Moves))
	for i, m := range p.Moves {
		ids[i] = m.ID
	}
	api.PublishWebSocketEvent("moved", map[string]any{"root_id": root.Id, "post_ids": ids, "source_roots": p.SourceRoots},
		&model.WebsocketBroadcast{ChannelId: channelID})
	return root.Id, nil
}

// planMove takes posts oldest first. A thread reads top down, so posts older
// than the root move to just after it, keeping their order.
func planMove(root *model.Post, posts []*model.Post) plan {
	p := plan{RootID: root.Id, RootUserID: root.UserId, ChannelID: root.ChannelId}
	moving := map[string]bool{}
	for _, post := range posts {
		moving[post.Id] = true
	}
	sources := map[string]bool{}
	authors := map[string]bool{}
	createAt := root.CreateAt
	for _, post := range posts {
		createAt = max(createAt+1, post.CreateAt)
		p.Moves = append(p.Moves, relocation{ID: post.Id, CreateAt: createAt})
		authors[post.UserId] = true
		switch {
		case post.RootId == "":
			p.FormerRoots = append(p.FormerRoots, post.Id)
		case !moving[post.RootId]:
			sources[post.RootId] = true
		}
	}
	p.SourceRoots = sortedKeys(sources)
	p.Authors = sortedKeys(authors)
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
		case post.Id == rootID || post.RootId == rootID:
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
