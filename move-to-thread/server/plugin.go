package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/pluginapi"
)

// API is the part of the Mattermost plugin API that moving posts needs.
type API interface {
	GetPost(postID string) (*model.Post, *model.AppError)
	GetPostThread(postID string) (*model.PostList, *model.AppError)
	GetChannelMember(channelID, userID string) (*model.ChannelMember, *model.AppError)
	HasPermissionToChannel(userID, channelID string, permission *model.Permission) bool
	PublishWebSocketEvent(event string, payload map[string]any, broadcast *model.WebsocketBroadcast)
	UpdatePost(post *model.Post) (*model.Post, *model.AppError)
}

type Plugin struct {
	plugin.MattermostPlugin
	api   API
	store store
}

func (p *Plugin) OnActivate() error {
	p.api = p.API
	client := pluginapi.NewClient(p.API, p.Driver)
	if client.Store.DriverName() != model.DatabaseDriverPostgres {
		p.store = unsupported{}
		return nil
	}
	db, err := client.Store.GetMasterDB()
	if err != nil {
		return err
	}
	p.store = postgres{db: db}
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

	rootID, err := move(p.api, p.store, userID, request.TargetID, request.PostIDs)
	w.Header().Set("Content-Type", "application/json")
	var failed *failure
	if errors.As(err, &failed) {
		w.WriteHeader(failed.status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": failed.message})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"root_id": rootID})
}
