package automatic

import (
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/gemini"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/limit"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/worker"
)

func TestProcessJobStopsWhenCurrentConfigurationNarrowsScope(t *testing.T) {
	for _, test := range []struct {
		name  string
		scope func(*automaticTestEnvironment) *Scope
	}{
		{
			name: "configured source removed",
			scope: func(environment *automaticTestEnvironment) *Scope {
				return NewScope(environment.api, automaticTestTeamID, nil, automaticTestBotID, false)
			},
		},
		{
			name: "configured team changed",
			scope: func(environment *automaticTestEnvironment) *Scope {
				return NewScope(environment.api, "replacement-team", []string{automaticTestSourceID}, automaticTestBotID, false)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := newAutomaticTestEnvironment(t)
			source := automaticSourcePost("source-1", 101, "Hello")
			environment.api.seedPost(source)
			require.NoError(t, environment.service.HandlePosted(source.Clone()))
			job := mustJob(t, environment.repository, source.Id, source.UpdateAt)
			environment.service = environment.serviceWithScope(test.scope(environment))

			require.NoError(t, environment.service.ProcessJob(t.Context(), job))

			require.Empty(t, environment.translator.snapshot())
			require.Empty(t, environment.api.visibleBotPosts())
			require.False(t, environment.api.createAttempted)
			require.False(t, environment.api.updateAttempted)
		})
	}
}

func TestProcessJobStopsWhenDirectMessagesAreDisabled(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := environment.seedEligibleConversation(t, model.ChannelTypeDirect)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))
	job := mustJob(t, environment.repository, source.Id, source.UpdateAt)
	environment.service = environment.serviceWithScope(NewScope(
		environment.api,
		automaticTestTeamID,
		[]string{automaticTestSourceID},
		automaticTestBotID,
		false,
	))

	require.NoError(t, environment.service.ProcessJob(t.Context(), job))

	require.Empty(t, environment.translator.snapshot())
	require.Empty(t, environment.api.visibleBotPosts())
	require.False(t, environment.api.createAttempted)
	require.False(t, environment.api.updateAttempted)
}

func TestProcessJobStopsWhenConversationMembershipIsLostBeforeProcessing(t *testing.T) {
	for _, channelType := range []model.ChannelType{model.ChannelTypeDirect, model.ChannelTypeGroup} {
		t.Run(string(channelType), func(t *testing.T) {
			environment := newAutomaticTestEnvironment(t)
			source := environment.seedEligibleConversation(t, channelType)
			require.NoError(t, environment.service.HandlePosted(source.Clone()))
			job := mustJob(t, environment.repository, source.Id, source.UpdateAt)
			environment.deleteConversationMembership("conversation-peer")

			require.NoError(t, environment.service.ProcessJob(t.Context(), job))

			require.Empty(t, environment.translator.snapshot())
			require.Empty(t, environment.api.visibleBotPosts())
			require.False(t, environment.api.createAttempted)
			require.False(t, environment.api.updateAttempted)
		})
	}
}

func TestWorkerConvergesIneligiblePendingAndRetryableJobsToSucceeded(t *testing.T) {
	for _, test := range []struct {
		name  string
		state domain.JobState
	}{
		{name: "pending", state: domain.JobPending},
		{name: "retryable", state: domain.JobRetryableFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := newAutomaticTestEnvironment(t)
			source := automaticSourcePost("source-1", 101, "Hello")
			environment.api.seedPost(source)
			require.NoError(t, environment.service.HandlePosted(source.Clone()))
			job := mustJob(t, environment.repository, source.Id, source.UpdateAt)
			job.State = test.state
			if test.state == domain.JobRetryableFailed {
				job.Attempt = 2
				job.NextAttemptAt = time.Now().UTC().Add(-time.Minute)
			}
			require.NoError(t, environment.repository.PutJob(job))
			environment.service = environment.serviceWithScope(NewScope(
				environment.api,
				automaticTestTeamID,
				nil,
				automaticTestBotID,
				false,
			))
			pool := worker.New(
				environment.repository,
				environment.service,
				limit.New(1),
				1,
				automaticTestModelID,
				nil,
			)
			pool.Start(t.Context())
			pool.Wake()
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				stored, found, err := environment.repository.GetJob(source.Id, source.UpdateAt)
				assert.NoError(collect, err)
				assert.True(collect, found)
				assert.Equal(collect, domain.JobSucceeded, stored.State)
			}, 2*time.Second, 10*time.Millisecond)
			pool.Stop()

			require.Empty(t, environment.translator.snapshot())
			require.Empty(t, environment.api.visibleBotPosts())
			require.False(t, environment.api.createAttempted)
			require.False(t, environment.api.updateAttempted)
		})
	}
}

func TestProcessJobRevalidatesConversationMembershipAfterGemini(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := environment.seedEligibleConversation(t, model.ChannelTypeGroup)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))
	job := mustJob(t, environment.repository, source.Id, source.UpdateAt)
	entered := make(chan struct{})
	resume := make(chan struct{})
	environment.translator.translate = func(input gemini.KoreanInput) string {
		close(entered)
		<-resume
		return input.Message
	}
	processed := make(chan error, 1)
	go func() {
		processed <- environment.service.ProcessJob(t.Context(), job)
	}()
	<-entered
	environment.deleteConversationMembership("conversation-peer")
	close(resume)

	require.NoError(t, <-processed)
	require.Len(t, environment.translator.snapshot(), 1)
	require.Empty(t, environment.api.visibleBotPosts())
	require.False(t, environment.api.createAttempted)
	require.False(t, environment.api.updateAttempted)
}

func TestReconcileMappingsDoesNotRecreateSameVersionOutsideCurrentScope(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	mapping := domain.SourceMapping{
		SourcePostID:       source.Id,
		SourceUpdateAt:     source.UpdateAt,
		ChannelID:          source.ChannelId,
		OperationID:        "operation-current-version",
		OperationStartedAt: automaticTestNow.Add(-time.Minute),
		TranslationPostID:  "translation-that-is-now-missing",
	}
	require.NoError(t, environment.repository.PutMapping(mapping))
	environment.service = environment.serviceWithScope(NewScope(
		environment.api,
		automaticTestTeamID,
		nil,
		automaticTestBotID,
		false,
	))

	require.NoError(t, environment.service.ReconcileMappings(t.Context()))

	_, found, err := environment.repository.GetJob(source.Id, source.UpdateAt)
	require.NoError(t, err)
	require.False(t, found)
	require.Zero(t, environment.waker.wakes)
	require.Empty(t, environment.translator.snapshot())
	require.False(t, environment.api.createAttempted)
	require.False(t, environment.api.updateAttempted)
}

func TestReconcileMappingsKeepsExistingPermanentTranslationWhenScopeNarrows(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	mapping := domain.SourceMapping{
		SourcePostID:       source.Id,
		SourceUpdateAt:     source.UpdateAt,
		ChannelID:          source.ChannelId,
		OperationID:        "operation-existing",
		OperationStartedAt: automaticTestNow.Add(-time.Minute),
		TranslationPostID:  "translation-existing",
	}
	translation := automaticMappedPost(mapping.TranslationPostID, mapping, automaticTestNow.UnixMilli())
	environment.api.seedPost(translation)
	require.NoError(t, environment.repository.PutMapping(mapping))
	environment.service = environment.serviceWithScope(NewScope(
		environment.api,
		automaticTestTeamID,
		nil,
		automaticTestBotID,
		false,
	))

	require.NoError(t, environment.service.ReconcileMappings(t.Context()))

	require.Zero(t, environment.api.post(translation.Id).DeleteAt)
	require.Equal(t, translation.Id, mustMapping(t, environment.repository, source.Id).TranslationPostID)
	require.Zero(t, environment.waker.wakes)
	require.Empty(t, environment.translator.snapshot())
}

func TestReconcileMappingsAdoptsUnresolvedPublicationBeforeCurrentScopeCheck(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))
	job := mustJob(t, environment.repository, source.Id, source.UpdateAt)
	mapping := mustMapping(t, environment.repository, source.Id)
	translation := automaticMappedPost("translation-already-created", mapping, automaticTestNow.UnixMilli())
	environment.api.seedPost(translation)
	created, err := environment.repository.CreateAutomaticPublicationOutcome(domain.AutomaticPublicationOutcome{
		IntentID:           "unresolved-intent",
		SourcePostID:       job.SourcePostID,
		SourceUpdateAt:     job.SourceUpdateAt,
		ChannelID:          job.ChannelID,
		OperationID:        job.OperationID,
		OperationStartedAt: job.OperationStartedAt,
		State:              domain.AutomaticPublicationPublishing,
	})
	require.NoError(t, err)
	require.True(t, created)
	environment.service = environment.serviceWithScope(NewScope(
		environment.api,
		automaticTestTeamID,
		nil,
		automaticTestBotID,
		false,
	))

	require.NoError(t, environment.service.ReconcileMappings(t.Context()))

	require.Equal(t, translation.Id, mustMapping(t, environment.repository, source.Id).TranslationPostID)
	require.Empty(t, environment.translator.snapshot())
	require.False(t, environment.api.createAttempted)
	require.False(t, environment.api.updateAttempted)
	_, found, err := environment.repository.GetAutomaticPublicationOutcome(job.OperationID)
	require.NoError(t, err)
	require.False(t, found)
}

func (environment *automaticTestEnvironment) serviceWithScope(scope *Scope) *Service {
	return newService(
		scope,
		environment.repository,
		environment.api,
		environment.translator,
		publication.NewPublisher(environment.api, environment.repository, environment.repository),
		environment.waker,
		ServiceConfig{
			ActivationAt:     100,
			TranslatorUserID: automaticTestBotID,
			SiteURL:          automaticTestSiteURL,
			ModelID:          automaticTestModelID,
			Glossary:         environment.configuredGlossary,
			Now:              func() time.Time { return environment.now },
		},
		environment.newPublicationIntentID,
	)
}

func (environment *automaticTestEnvironment) seedEligibleConversation(t *testing.T, channelType model.ChannelType) *model.Post {
	t.Helper()
	environment.api.mu.Lock()
	environment.api.channels[automaticTestChannelID] = &model.Channel{Id: automaticTestChannelID, Type: channelType}
	environment.api.conversationParticipants[automaticTestChannelID] = []*model.User{
		{Id: automaticTestSourceID},
		{Id: "conversation-peer"},
	}
	for _, userID := range []string{automaticTestSourceID, "conversation-peer"} {
		environment.api.teamMemberships[automaticTestTeamID+"/"+userID] = &model.TeamMember{
			TeamId: automaticTestTeamID,
			UserId: userID,
		}
	}
	environment.api.mu.Unlock()
	environment.service = environment.serviceWithScope(NewScope(
		environment.api,
		automaticTestTeamID,
		[]string{automaticTestSourceID},
		automaticTestBotID,
		true,
	))
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	return source
}

func (environment *automaticTestEnvironment) deleteConversationMembership(userID string) {
	environment.api.mu.Lock()
	membership := *environment.api.teamMemberships[automaticTestTeamID+"/"+userID]
	membership.DeleteAt = 1
	environment.api.teamMemberships[automaticTestTeamID+"/"+userID] = &membership
	environment.api.mu.Unlock()
}
