package automatic

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
)

func TestReconcileMappingsCleansMissingOrDeletedSource(t *testing.T) {
	for _, test := range []struct {
		name       string
		seedSource bool
	}{
		{name: "missing"},
		{name: "deleted", seedSource: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := newAutomaticTestEnvironment(t)
			source := automaticSourcePost("source-1", 101, "Hello")
			if test.seedSource {
				source.DeleteAt = 303
				environment.api.seedPost(source)
			}
			mapping := domain.SourceMapping{
				SourcePostID:       source.Id,
				SourceUpdateAt:     source.UpdateAt,
				ChannelID:          source.ChannelId,
				OperationID:        "operation-1",
				OperationStartedAt: automaticTestNow.Add(-time.Minute),
				TranslationPostID:  "translation-1",
			}
			translation := automaticMappedPost(mapping.TranslationPostID, mapping, automaticTestNow.UnixMilli())
			environment.api.seedPost(translation)
			require.NoError(t, environment.repository.PutMapping(mapping))

			require.NoError(t, environment.service.ReconcileMappings(t.Context()))
			require.NotZero(t, environment.api.post(translation.Id).DeleteAt)
			require.NoError(t, environment.service.HandleDeleted(environment.api.post(translation.Id)))

			reconciled := mustMapping(t, environment.repository, source.Id)
			require.Empty(t, reconciled.TranslationPostID)
			require.True(t, reconciled.InternallyDeleting)
			require.Zero(t, reconciled.SuppressedUpdateAt)
		})
	}
}

func TestReconcileMappingsEnqueuesEditMissedWhileDisabled(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 202, "Hello edited while disabled")
	environment.api.seedPost(source)
	mapping := domain.SourceMapping{
		SourcePostID:       source.Id,
		SourceUpdateAt:     101,
		ChannelID:          source.ChannelId,
		OperationID:        "operation-old",
		OperationStartedAt: automaticTestNow.Add(-time.Hour),
		TranslationPostID:  "translation-1",
	}
	environment.api.seedPost(automaticMappedPost(mapping.TranslationPostID, mapping, automaticTestNow.Add(-time.Hour).UnixMilli()))
	require.NoError(t, environment.repository.PutMapping(mapping))

	require.NoError(t, environment.service.ReconcileMappings(t.Context()))

	job := mustJob(t, environment.repository, source.Id, source.UpdateAt)
	require.Equal(t, domain.JobPending, job.State)
	require.NotEqual(t, mapping.OperationID, job.OperationID)
	reconciled := mustMapping(t, environment.repository, source.Id)
	require.Equal(t, source.UpdateAt, reconciled.SourceUpdateAt)
	require.Equal(t, job.OperationID, reconciled.OperationID)
	require.Equal(t, mapping.TranslationPostID, reconciled.TranslationPostID)
	require.Zero(t, reconciled.SuppressedUpdateAt)
	require.Equal(t, 1, environment.waker.wakes)
	require.True(t, environment.waker.persistedBeforeEveryWake)
	require.Empty(t, environment.translator.snapshot())
}

func TestReconcileMappingsKeepsPersistedKnownPostEditRecoverable(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 202, "Hello current")
	environment.api.seedPost(source)
	current := domain.SourceMapping{
		SourcePostID:       source.Id,
		SourceUpdateAt:     source.UpdateAt,
		ChannelID:          source.ChannelId,
		OperationID:        "operation-current",
		OperationStartedAt: automaticTestNow,
		TranslationPostID:  "translation-1",
	}
	prior := current
	prior.SourceUpdateAt = 101
	prior.OperationID = "operation-prior"
	environment.api.seedPost(automaticMappedPost(current.TranslationPostID, prior, automaticTestNow.Add(-time.Hour).UnixMilli()))
	require.NoError(t, environment.repository.PutMapping(current))
	require.NoError(t, environment.repository.PutJob(domain.AutomaticJob{
		SourcePostID:       current.SourcePostID,
		SourceUpdateAt:     current.SourceUpdateAt,
		ChannelID:          current.ChannelID,
		OperationID:        current.OperationID,
		OperationStartedAt: current.OperationStartedAt,
		State:              domain.JobPending,
	}))

	require.NoError(t, environment.service.ReconcileMappings(t.Context()))

	require.Equal(t, 1, environment.waker.wakes)
	require.Equal(t, current.TranslationPostID, mustMapping(t, environment.repository, source.Id).TranslationPostID)
}

func TestReconcileMappingsRecoversFutureLeasePublicationFenceAfterImmediateRestart(t *testing.T) {
	for _, test := range []struct {
		name              string
		postVersion       int64
		postOperation     string
		expectedVersion   int64
		expectedOperation string
	}{
		{
			name:              "committed desired update",
			postVersion:       202,
			postOperation:     "operation-202",
			expectedVersion:   202,
			expectedOperation: "operation-202",
		},
		{
			name:              "uncommitted old post",
			postVersion:       101,
			postOperation:     "operation-101",
			expectedVersion:   202,
			expectedOperation: "operation-202",
		},
		{
			name:              "advanced newer post",
			postVersion:       303,
			postOperation:     "operation-303",
			expectedVersion:   303,
			expectedOperation: "operation-303",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := newAutomaticTestEnvironment(t)
			source := automaticSourcePost("source-1", 202, "Hello current")
			environment.api.seedPost(source)
			mapping := domain.SourceMapping{
				SourcePostID:       source.Id,
				SourceUpdateAt:     source.UpdateAt,
				ChannelID:          source.ChannelId,
				OperationID:        "operation-202",
				OperationStartedAt: environment.now.Add(-time.Minute),
				TranslationPostID:  "translation-1",
				SuppressedUpdateAt: source.UpdateAt,
			}
			require.NoError(t, environment.repository.PutMapping(mapping))
			postMapping := mapping
			postMapping.SourceUpdateAt = test.postVersion
			postMapping.OperationID = test.postOperation
			environment.api.seedPost(automaticMappedPost(mapping.TranslationPostID, postMapping, environment.now.UnixMilli()))
			crashed := domain.AutomaticPublicationGuard{
				TranslationPostID: mapping.TranslationPostID,
				SourcePostID:      source.Id,
				SourceUpdateAt:    202,
				OperationID:       "operation-202",
				LeaseUntil:        environment.now.Add(time.Hour),
			}
			acquired, ok, err := environment.repository.AcquireAutomaticPublicationGuard(crashed, environment.now)
			require.NoError(t, err)
			require.True(t, ok)
			_, ok, err = environment.repository.BeginAutomaticPublicationUpdate(acquired)
			require.NoError(t, err)
			require.True(t, ok)

			require.NoError(t, environment.service.ReconcileMappings(t.Context()))

			recovered, found, err := environment.repository.GetAutomaticPublicationGuard(mapping.TranslationPostID)
			require.NoError(t, err)
			require.True(t, found)
			require.False(t, recovered.Updating)
			require.True(t, recovered.LeaseUntil.IsZero())
			require.Equal(t, test.expectedVersion, recovered.SourceUpdateAt)
			require.Equal(t, test.expectedOperation, recovered.OperationID)
		})
	}
}

func TestReconcileMappingsRecreatesMissingTranslationInline(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	mapping := domain.SourceMapping{
		SourcePostID:       source.Id,
		SourceUpdateAt:     source.UpdateAt,
		ChannelID:          source.ChannelId,
		OperationID:        "operation-recreate",
		OperationStartedAt: automaticTestNow.Add(-time.Minute),
		TranslationPostID:  "missing-translation",
	}
	require.NoError(t, environment.repository.PutMapping(mapping))

	require.NoError(t, environment.service.ReconcileMappings(t.Context()))
	job := mustJob(t, environment.repository, source.Id, source.UpdateAt)
	require.Equal(t, mapping.OperationID, job.OperationID)
	require.NoError(t, environment.service.ProcessJob(t.Context(), job))

	require.Equal(t, "안녕하세요\n\n> Hello", environment.api.post(source.Id).Message)
	require.Empty(t, environment.api.visibleBotPosts())
	require.Equal(t, source.Id, mustMapping(t, environment.repository, source.Id).TranslationPostID)
}

func TestReconcileMappingsSkipsCurrentVersionSuppression(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	mapping := domain.SourceMapping{
		SourcePostID:       source.Id,
		SourceUpdateAt:     source.UpdateAt,
		ChannelID:          source.ChannelId,
		OperationID:        "operation-suppressed",
		OperationStartedAt: automaticTestNow.Add(-time.Minute),
		SuppressedUpdateAt: source.UpdateAt,
	}
	require.NoError(t, environment.repository.PutMapping(mapping))

	require.NoError(t, environment.service.ReconcileMappings(t.Context()))

	_, found, err := environment.repository.GetJob(source.Id, source.UpdateAt)
	require.NoError(t, err)
	require.False(t, found)
	require.Zero(t, environment.waker.wakes)
	require.Empty(t, environment.translator.snapshot())
	require.Empty(t, environment.api.visibleBotPosts())
}

func TestReconcileMappingsAdoptsLostAcknowledgementAfterRestart(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	mapping := domain.SourceMapping{
		SourcePostID:       source.Id,
		SourceUpdateAt:     source.UpdateAt,
		ChannelID:          source.ChannelId,
		OperationID:        "operation-lost-ack",
		OperationStartedAt: automaticTestNow.Add(-time.Minute),
	}
	translation := automaticMappedPost("translation-existing", mapping, automaticTestNow.UnixMilli())
	translation.PendingPostId = mapping.OperationID
	environment.api.seedPost(translation)
	require.NoError(t, environment.repository.PutMapping(mapping))

	require.NoError(t, environment.service.ReconcileMappings(t.Context()))

	reconciled := mustMapping(t, environment.repository, source.Id)
	require.Equal(t, translation.Id, reconciled.TranslationPostID)
	require.Zero(t, environment.waker.wakes)
	require.Empty(t, environment.translator.snapshot())
}

func TestReconcileMappingsAdoptsOperationAfterPendingPostIDExpires(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	mapping := domain.SourceMapping{
		SourcePostID:       source.Id,
		SourceUpdateAt:     source.UpdateAt,
		ChannelID:          source.ChannelId,
		OperationID:        "operation-expired-pending-id",
		OperationStartedAt: automaticTestNow.Add(-24 * time.Hour),
	}
	translation := automaticMappedPost("translation-existing", mapping, automaticTestNow.Add(-23*time.Hour).UnixMilli())
	translation.PendingPostId = ""
	environment.api.seedPost(translation)
	require.NoError(t, environment.repository.PutMapping(mapping))

	require.NoError(t, environment.service.ReconcileMappings(t.Context()))

	require.Equal(t, translation.Id, mustMapping(t, environment.repository, source.Id).TranslationPostID)
	require.Zero(t, environment.waker.wakes)
}

func TestReconcileMappingsAcceptsJSONDecodedSourceTimestamp(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", automaticTestNow.UnixMilli(), "Hello")
	environment.api.seedPost(source)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))
	job := mustJob(t, environment.repository, source.Id, source.UpdateAt)
	mapping := mustMapping(t, environment.repository, source.Id)
	translation := automaticMappedPost("translation-existing", mapping, automaticTestNow.UnixMilli())
	translation.Props[publication.TranslatorSourceUpdateAtProp] = float64(source.UpdateAt)
	environment.api.seedPost(translation)
	publishing := domain.AutomaticPublicationOutcome{
		IntentID:           "published-intent",
		SourcePostID:       job.SourcePostID,
		SourceUpdateAt:     job.SourceUpdateAt,
		ChannelID:          job.ChannelID,
		OperationID:        job.OperationID,
		OperationStartedAt: job.OperationStartedAt,
		State:              domain.AutomaticPublicationPublishing,
	}
	created, err := environment.repository.CreateAutomaticPublicationOutcome(publishing)
	require.NoError(t, err)
	require.True(t, created)
	published := publishing
	published.State = domain.AutomaticPublicationPublished
	published.TranslationPostID = translation.Id
	stored, err := environment.repository.SaveAutomaticPublicationOutcomeCAS(publishing, published)
	require.NoError(t, err)
	require.True(t, stored)

	require.NoError(t, environment.service.ReconcileMappings(t.Context()))
	require.Equal(t, translation.Id, mustMapping(t, environment.repository, source.Id).TranslationPostID)
}

func TestReconcileMappingsDeletesDuplicateOperationMarkers(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	mapping := domain.SourceMapping{
		SourcePostID:       source.Id,
		SourceUpdateAt:     source.UpdateAt,
		ChannelID:          source.ChannelId,
		OperationID:        "operation-duplicates",
		OperationStartedAt: automaticTestNow.Add(-time.Minute),
	}
	winner := automaticMappedPost("translation-a", mapping, automaticTestNow.UnixMilli())
	duplicate := automaticMappedPost("translation-b", mapping, automaticTestNow.Add(time.Second).UnixMilli())
	environment.api.seedPost(winner)
	environment.api.seedPost(duplicate)
	require.NoError(t, environment.repository.PutMapping(mapping))

	require.NoError(t, environment.service.ReconcileMappings(t.Context()))

	require.Equal(t, winner.Id, mustMapping(t, environment.repository, source.Id).TranslationPostID)
	require.Zero(t, environment.api.post(winner.Id).DeleteAt)
	require.NotZero(t, environment.api.post(duplicate.Id).DeleteAt)
}
