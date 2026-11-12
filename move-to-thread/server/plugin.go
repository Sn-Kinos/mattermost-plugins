package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// API is the part of the Mattermost plugin API that moving posts needs.
type API interface {
	GetPost(postID string) (*model.Post, *model.AppError)
	GetPostThread(postID string) (*model.PostList, *model.AppError)
	GetChannelMember(channelID, userID string) (*model.ChannelMember, *model.AppError)
	HasPermissionToChannel(userID, channelID string, permission *model.Permission) bool
	CreatePost(post *model.Post) (*model.Post, *model.AppError)
	DeletePost(postID string) *model.AppError
	GetReactions(postID string) ([]*model.Reaction, *model.AppError)
	AddReaction(reaction *model.Reaction) (*model.Reaction, *model.AppError)
	GetFileInfo(fileID string) (*model.FileInfo, *model.AppError)
	GetFile(fileID string) ([]byte, *model.AppError)
	UploadFile(data []byte, channelID, filename string) (*model.FileInfo, *model.AppError)
}

type Plugin struct {
	plugin.MattermostPlugin
	api API
}

func (p *Plugin) OnActivate() error {
	p.api = p.API
	return nil
}

// ServeHTTP serves POST /move with {"target_id", "post_ids"} and answers
// {"root_id"} or a Korean error message.
func (p *Plugin) ServeHTTP(_ *plugin.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/move" {
		http.NotFound(w, r)
		return
	}
	userID := r.Header.Get("Mattermost-User-Id")
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var request struct {
		TargetID string   `json:"target_id"`
		PostIDs  []string `json:"post_ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	rootID, err := move(p.api, userID, request.TargetID, request.PostIDs)
	w.Header().Set("Content-Type", "application/json")
	var failed *failure
	if errors.As(err, &failed) {
		w.WriteHeader(failed.status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": failed.message})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"root_id": rootID})
}
