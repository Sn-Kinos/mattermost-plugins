package automatic

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

const (
	testTeamID       = "example-team-id"
	testSourceUserID = "source-user-id"
	testBotUserID    = "translator-bot-id"
)

type scopeTestAPI struct {
	channels            map[string]*model.Channel
	usersByChannel      map[string][]*model.User
	memberships         map[string]*model.TeamMember
	channelErr          *model.AppError
	usersErrByPage      map[int]*model.AppError
	membershipErrByUser map[string]*model.AppError
	requestedPages      []int
}

func (a *scopeTestAPI) GetChannel(channelID string) (*model.Channel, *model.AppError) {
	if a.channelErr != nil {
		return nil, a.channelErr
	}
	return a.channels[channelID], nil
}

func (a *scopeTestAPI) GetUsersInChannel(channelID, _ string, page, perPage int) ([]*model.User, *model.AppError) {
	a.requestedPages = append(a.requestedPages, page)
	if err := a.usersErrByPage[page]; err != nil {
		return nil, err
	}
	users := a.usersByChannel[channelID]
	start := page * perPage
	if start >= len(users) {
		return []*model.User{}, nil
	}
	end := min(start+perPage, len(users))
	return users[start:end], nil
}

func (a *scopeTestAPI) GetTeamMember(teamID, userID string) (*model.TeamMember, *model.AppError) {
	if err := a.membershipErrByUser[userID]; err != nil {
		return nil, err
	}
	membership := a.memberships[userID]
	if membership == nil || membership.TeamId != teamID {
		return nil, model.NewAppError("scopeTestAPI.GetTeamMember", "not_found", nil, "", http.StatusNotFound)
	}
	return membership, nil
}

func TestEligibleOrdinaryChannelScopeAndSourceGuards(t *testing.T) {
	activationAt := int64(1_000)
	api := &scopeTestAPI{
		channels: map[string]*model.Channel{
			"public-example":  {Id: "public-example", TeamId: testTeamID, Type: model.ChannelTypeOpen},
			"private-example": {Id: "private-example", TeamId: testTeamID, Type: model.ChannelTypePrivate},
			"other-team":      {Id: "other-team", TeamId: "other-team-id", Type: model.ChannelTypeOpen},
			"unsupported":     {Id: "unsupported", TeamId: testTeamID, Type: model.ChannelTypeSpace},
		},
	}
	scope := NewScope(api, testTeamID, []string{testSourceUserID, testBotUserID}, testBotUserID, true)

	tests := []struct {
		name string
		post *model.Post
		want bool
	}{
		{name: "public configured team", post: testSourcePost("public-example", activationAt+1), want: true},
		{name: "private configured team", post: testSourcePost("private-example", activationAt+1), want: true},
		{name: "another team", post: testSourcePost("other-team", activationAt+1), want: false},
		{name: "unsupported channel type", post: testSourcePost("unsupported", activationAt+1), want: false},
		{name: "created at activation", post: testSourcePost("public-example", activationAt), want: false},
		{name: "created before activation", post: testSourcePost("public-example", activationAt-1), want: false},
		{name: "unconfigured author", post: &model.Post{UserId: "other-user", ChannelId: "public-example", CreateAt: activationAt + 1}, want: false},
		{name: "translator author", post: &model.Post{UserId: testBotUserID, ChannelId: "public-example", CreateAt: activationAt + 1}, want: false},
		{name: "nil post", post: nil, want: false},
	}

	botPost := testSourcePost("public-example", activationAt+1)
	botPost.AddProp(model.PostPropsFromBot, "true")
	tests = append(tests, struct {
		name string
		post *model.Post
		want bool
	}{name: "bot property", post: botPost, want: false})

	generatedPost := testSourcePost("public-example", activationAt+1)
	generatedPost.AddProp(TranslatorKindProp, "automatic")
	tests = append(tests, struct {
		name string
		post *model.Post
		want bool
	}{name: "translator generated property", post: generatedPost, want: false})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := scope.Eligible(tt.post, activationAt)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestEligibleDirectAndGroupChannelsRequireEveryActiveParticipantInTeam(t *testing.T) {
	tests := []struct {
		name        string
		channelType model.ChannelType
	}{
		{name: "direct", channelType: model.ChannelTypeDirect},
		{name: "group", channelType: model.ChannelTypeGroup},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := eligibleConversationAPI(tt.channelType, 2)
			scope := NewScope(api, testTeamID, []string{testSourceUserID}, testBotUserID, true)

			eligible, err := scope.Eligible(testSourcePost("conversation", 1_001), 1_000)

			require.NoError(t, err)
			require.True(t, eligible)
		})
	}
}

func TestEligibleDirectMessagesCanBeDisabled(t *testing.T) {
	api := eligibleConversationAPI(model.ChannelTypeDirect, 2)
	scope := NewScope(api, testTeamID, []string{testSourceUserID}, testBotUserID, false)

	eligible, err := scope.Eligible(testSourcePost("conversation", 1_001), 1_000)

	require.NoError(t, err)
	require.False(t, eligible)
	require.Empty(t, api.requestedPages)
}

func TestEligibleConversationRejectsOutsiderInactiveUserAndTooFewActiveParticipants(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*scopeTestAPI)
	}{
		{
			name: "outsider",
			mutate: func(api *scopeTestAPI) {
				delete(api.memberships, "participant-001")
			},
		},
		{
			name: "deleted membership",
			mutate: func(api *scopeTestAPI) {
				api.memberships["participant-001"].DeleteAt = 99
			},
		},
		{
			name: "inactive participant",
			mutate: func(api *scopeTestAPI) {
				api.usersByChannel["conversation"][1].DeleteAt = 99
			},
		},
		{
			name: "one participant",
			mutate: func(api *scopeTestAPI) {
				api.usersByChannel["conversation"] = api.usersByChannel["conversation"][:1]
				delete(api.memberships, "participant-001")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := eligibleConversationAPI(model.ChannelTypeGroup, 2)
			tt.mutate(api)
			scope := NewScope(api, testTeamID, []string{testSourceUserID}, testBotUserID, true)

			eligible, err := scope.Eligible(testSourcePost("conversation", 1_001), 1_000)

			require.NoError(t, err)
			require.False(t, eligible)
		})
	}
}

func TestEligibleConversationChecksAll201ParticipantsAcrossPages(t *testing.T) {
	t.Run("all complete active memberships", func(t *testing.T) {
		api := eligibleConversationAPI(model.ChannelTypeGroup, 201)
		scope := NewScope(api, testTeamID, []string{testSourceUserID}, testBotUserID, true)

		eligible, err := scope.Eligible(testSourcePost("conversation", 1_001), 1_000)

		require.NoError(t, err)
		require.True(t, eligible)
		require.Equal(t, []int{0, 1}, api.requestedPages)
	})

	t.Run("deleted membership on second page", func(t *testing.T) {
		api := eligibleConversationAPI(model.ChannelTypeGroup, 201)
		api.memberships["participant-200"].DeleteAt = 99
		scope := NewScope(api, testTeamID, []string{testSourceUserID}, testBotUserID, true)

		eligible, err := scope.Eligible(testSourcePost("conversation", 1_001), 1_000)

		require.NoError(t, err)
		require.False(t, eligible)
		require.Equal(t, []int{0, 1}, api.requestedPages)
	})
}

func TestEligibleReturnsLookupErrors(t *testing.T) {
	t.Run("channel lookup", func(t *testing.T) {
		api := &scopeTestAPI{channelErr: model.NewAppError("test", "lookup", nil, "", http.StatusInternalServerError)}
		scope := NewScope(api, testTeamID, []string{testSourceUserID}, testBotUserID, true)

		eligible, err := scope.Eligible(testSourcePost("channel", 1_001), 1_000)

		require.Error(t, err)
		require.False(t, eligible)
	})

	t.Run("participant lookup", func(t *testing.T) {
		api := eligibleConversationAPI(model.ChannelTypeDirect, 2)
		api.usersErrByPage[0] = model.NewAppError("test", "lookup", nil, "", http.StatusInternalServerError)
		scope := NewScope(api, testTeamID, []string{testSourceUserID}, testBotUserID, true)

		eligible, err := scope.Eligible(testSourcePost("conversation", 1_001), 1_000)

		require.Error(t, err)
		require.False(t, eligible)
	})

	t.Run("membership lookup", func(t *testing.T) {
		api := eligibleConversationAPI(model.ChannelTypeDirect, 2)
		api.membershipErrByUser["participant-001"] = model.NewAppError("test", "lookup", nil, "", http.StatusInternalServerError)
		scope := NewScope(api, testTeamID, []string{testSourceUserID}, testBotUserID, true)

		eligible, err := scope.Eligible(testSourcePost("conversation", 1_001), 1_000)

		require.Error(t, err)
		require.False(t, eligible)
	})
}

func testSourcePost(channelID string, createAt int64) *model.Post {
	return &model.Post{UserId: testSourceUserID, ChannelId: channelID, CreateAt: createAt}
}

func eligibleConversationAPI(channelType model.ChannelType, participantCount int) *scopeTestAPI {
	api := &scopeTestAPI{
		channels: map[string]*model.Channel{
			"conversation": {Id: "conversation", Type: channelType},
		},
		usersByChannel:      map[string][]*model.User{"conversation": {}},
		memberships:         make(map[string]*model.TeamMember, participantCount),
		usersErrByPage:      make(map[int]*model.AppError),
		membershipErrByUser: make(map[string]*model.AppError),
	}
	for index := range participantCount {
		userID := testSourceUserID
		if index > 0 {
			userID = "participant-" + leftPadThree(index)
		}
		api.usersByChannel["conversation"] = append(api.usersByChannel["conversation"], &model.User{
			Id:       userID,
			Username: "user-" + leftPadThree(index),
			Email:    "user-" + leftPadThree(index) + "@example.test",
			Roles:    "system_user",
		})
		api.memberships[userID] = &model.TeamMember{TeamId: testTeamID, UserId: userID, Roles: "team_user"}
	}
	return api
}

func leftPadThree(value int) string {
	return fmt.Sprintf("%03d", value)
}
