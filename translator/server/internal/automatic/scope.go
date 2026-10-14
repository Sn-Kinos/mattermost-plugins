package automatic

import (
	"errors"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
)

const (
	conversationPageSize = 200

	TranslatorKindProp           = publication.TranslatorKindProp
	TranslatorOperationIDProp    = publication.TranslatorOperationIDProp
	TranslatorSourcePostIDProp   = publication.TranslatorSourcePostIDProp
	TranslatorSourceUpdateAtProp = publication.TranslatorSourceUpdateAtProp
	TranslatorModelProp          = publication.TranslatorModelProp
)

type ScopeAPI interface {
	GetChannel(channelID string) (*model.Channel, *model.AppError)
	GetUsersInChannel(channelID, sortBy string, page, perPage int) ([]*model.User, *model.AppError)
	GetTeamMember(teamID, userID string) (*model.TeamMember, *model.AppError)
}

type Scope struct {
	api                   ScopeAPI
	teamID                string
	sourceUserIDs         map[string]struct{}
	translatorBotUserID   string
	includeDirectMessages bool
}

func NewScope(api ScopeAPI, teamID string, sourceUserIDs []string, translatorBotUserID string, includeDirectMessages bool) *Scope {
	sources := make(map[string]struct{}, len(sourceUserIDs))
	for _, userID := range sourceUserIDs {
		sources[userID] = struct{}{}
	}
	return &Scope{
		api:                   api,
		teamID:                teamID,
		sourceUserIDs:         sources,
		translatorBotUserID:   translatorBotUserID,
		includeDirectMessages: includeDirectMessages,
	}
}

func (s *Scope) Eligible(post *model.Post, activationAt int64) (bool, error) {
	if post == nil || post.CreateAt <= activationAt || post.UserId == s.translatorBotUserID {
		return false, nil
	}
	if _, configured := s.sourceUserIDs[post.UserId]; !configured {
		return false, nil
	}
	if isBotPost(post) || isTranslatorGenerated(post) {
		return false, nil
	}
	if s.api == nil {
		return false, errors.New("automatic scope API is unavailable")
	}

	channel, appErr := s.api.GetChannel(post.ChannelId)
	if appErr != nil || channel == nil {
		return false, errors.New("automatic scope channel lookup failed")
	}
	switch channel.Type {
	case model.ChannelTypeOpen, model.ChannelTypePrivate:
		return channel.TeamId == s.teamID, nil
	case model.ChannelTypeDirect, model.ChannelTypeGroup:
		if !s.includeDirectMessages {
			return false, nil
		}
		return s.conversationParticipantsEligible(channel.Id)
	default:
		return false, nil
	}
}

func (s *Scope) conversationParticipantsEligible(channelID string) (bool, error) {
	participantIDs := make(map[string]struct{})
	for page := 0; ; page++ {
		participants, appErr := s.api.GetUsersInChannel(channelID, "", page, conversationPageSize)
		if appErr != nil {
			return false, errors.New("automatic scope participant lookup failed")
		}
		for _, participant := range participants {
			if participant == nil || participant.Id == "" || participant.DeleteAt != 0 {
				return false, nil
			}
			if _, duplicate := participantIDs[participant.Id]; duplicate {
				continue
			}

			membership, membershipErr := s.api.GetTeamMember(s.teamID, participant.Id)
			if membershipErr != nil {
				if membershipErr.StatusCode == http.StatusNotFound {
					return false, nil
				}
				return false, errors.New("automatic scope team membership lookup failed")
			}
			if membership == nil || membership.TeamId != s.teamID || membership.UserId != participant.Id || membership.DeleteAt != 0 {
				return false, nil
			}
			participantIDs[participant.Id] = struct{}{}
		}
		if len(participants) < conversationPageSize {
			break
		}
	}
	return len(participantIDs) >= 2, nil
}

func isBotPost(post *model.Post) bool {
	switch value := post.GetProp(model.PostPropsFromBot).(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(strings.TrimSpace(value), "true")
	default:
		return false
	}
}

func isTranslatorGenerated(post *model.Post) bool {
	for _, key := range []string{
		TranslatorKindProp,
		TranslatorOperationIDProp,
		TranslatorSourcePostIDProp,
		TranslatorSourceUpdateAtProp,
		TranslatorModelProp,
	} {
		if post.GetProp(key) != nil {
			return true
		}
	}
	return false
}
