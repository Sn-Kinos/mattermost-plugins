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

// move recreates the posts as replies in the target's thread, oldest first, and
// deletes the originals only after every copy exists. Mattermost cannot change
// an existing post's root, so this is how its own thread move works too.
func move(api API, userID, targetID string, postIDs []string) (string, error) {
	if len(postIDs) == 0 || len(postIDs) > maxPosts {
		return "", reject(http.StatusBadRequest, "옮길 메시지를 1개에서 200개 사이로 골라 주세요.")
	}
	target, appErr := api.GetPost(targetID)
	if appErr != nil {
		return "", reject(http.StatusBadRequest, "답글을 넣을 메시지를 찾지 못했습니다.")
	}
	rootID := target.RootId
	if rootID == "" {
		rootID = target.Id
	}
	channelID := target.ChannelId
	if _, appErr := api.GetChannelMember(channelID, userID); appErr != nil ||
		!api.HasPermissionToChannel(userID, channelID, model.PermissionCreatePost) {
		return "", reject(http.StatusForbidden, "이 채널에 글을 쓸 권한이 없습니다.")
	}

	posts, err := collect(api, userID, channelID, rootID, postIDs)
	if err != nil {
		return "", err
	}

	var copies []string
	for _, post := range posts {
		copyID, err := recreate(api, post, rootID)
		if err != nil {
			for i := len(copies) - 1; i >= 0; i-- {
				_ = api.DeletePost(copies[i])
			}
			return "", reject(http.StatusInternalServerError, "메시지를 옮기지 못했습니다. 원본은 그대로 있습니다.")
		}
		copies = append(copies, copyID)
	}

	// Newest first, so a thread's replies go before its root takes them along.
	failed := false
	for i := len(posts) - 1; i >= 0; i-- {
		if appErr := api.DeletePost(posts[i].Id); appErr != nil {
			failed = true
		}
	}
	if failed {
		return "", reject(http.StatusInternalServerError, "답글로 옮겼지만 원본 일부를 지우지 못했습니다.")
	}
	return rootID, nil
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
		case post.IsSystemMessage():
			return reject(http.StatusBadRequest, "시스템 메시지는 옮길 수 없습니다.")
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

// recreate posts a copy of post under rootID with the same author, time,
// content, files, reactions and pin, and returns the copy's ID.
func recreate(api API, post *model.Post, rootID string) (string, error) {
	fileIDs := make([]string, 0, len(post.FileIds))
	for _, fileID := range post.FileIds {
		info, appErr := api.GetFileInfo(fileID)
		if appErr != nil {
			return "", appErr
		}
		data, appErr := api.GetFile(fileID)
		if appErr != nil {
			return "", appErr
		}
		uploaded, appErr := api.UploadFile(data, post.ChannelId, info.Name)
		if appErr != nil {
			return "", appErr
		}
		fileIDs = append(fileIDs, uploaded.Id)
	}

	created, appErr := api.CreatePost(&model.Post{
		UserId:    post.UserId,
		ChannelId: post.ChannelId,
		RootId:    rootID,
		Message:   post.Message,
		Type:      post.Type,
		Props:     post.GetProps(),
		Hashtags:  post.Hashtags,
		FileIds:   fileIDs,
		IsPinned:  post.IsPinned,
		CreateAt:  post.CreateAt,
		EditAt:    post.EditAt,
	})
	if appErr != nil {
		return "", appErr
	}

	reactions, appErr := api.GetReactions(post.Id)
	if appErr != nil {
		return created.Id, nil
	}
	for _, reaction := range reactions {
		_, _ = api.AddReaction(&model.Reaction{UserId: reaction.UserId, PostId: created.Id, EmojiName: reaction.EmojiName})
	}
	return created.Id, nil
}
