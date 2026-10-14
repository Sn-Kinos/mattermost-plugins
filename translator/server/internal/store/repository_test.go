package store

import (
	"bytes"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
)

func TestClaimDueJobOnlyChangesMatchingPendingBytes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	kv := newMemoryKV()
	kv.values[JobPrefix+"post-1/42"] = []byte(`{
        "state":"pending",
        "source_post_id":"post-1",
        "source_update_at":42,
        "channel_id":"channel-1",
        "root_id":"",
        "operation_id":"operation-1",
        "operation_started_at":"2026-09-01T11:59:00Z",
        "attempt":0,
        "next_attempt_at":"0001-01-01T00:00:00Z",
        "processing_lease_until":"0001-01-01T00:00:00Z",
        "last_error_category":""
    }`)

	claimed, ok, err := NewRepository(kv).ClaimDueJob("post-1", 42, now, now.Add(30*time.Second))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, domain.JobProcessing, claimed.State)
	require.Equal(t, now.Add(30*time.Second), claimed.ProcessingLeaseUntil)

	stored := kv.value(JobPrefix + "post-1/42")
	require.JSONEq(t, `{
        "source_post_id":"post-1",
        "source_update_at":42,
        "channel_id":"channel-1",
        "root_id":"",
        "operation_id":"operation-1",
        "operation_started_at":"2026-09-01T11:59:00Z",
        "state":"processing",
        "attempt":0,
        "next_attempt_at":"0001-01-01T00:00:00Z",
        "processing_lease_until":"2026-09-01T12:00:30Z",
        "last_error_category":""
    }`, string(stored))
}

func TestConsumeDeletionIntentIsOneShot(t *testing.T) {
	t.Parallel()

	kv := newMemoryKV()
	repository := NewRepository(kv)
	intent := domain.DeletionIntent{
		TranslationPostID: "translation-1",
		SourcePostID:      "source-1",
		OperationID:       "operation-1",
	}
	require.NoError(t, repository.PutDeletionIntent(intent, 300))
	stored, found, err := repository.GetDeletionIntent(intent.TranslationPostID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, intent, stored)

	consumed, err := repository.ConsumeDeletionIntent(intent.TranslationPostID)
	require.NoError(t, err)
	require.True(t, consumed)
	consumed, err = repository.ConsumeDeletionIntent(intent.TranslationPostID)
	require.NoError(t, err)
	require.False(t, consumed)
	require.Nil(t, kv.value(DeletionIntentPrefix+intent.TranslationPostID))
}

func TestListDueJobsIncludesExpiredProcessingLeaseButNotFutureRetry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	kv := newMemoryKV()
	repository := NewRepository(kv)
	jobs := []domain.AutomaticJob{
		jobFixture("pending", 1, domain.JobPending),
		jobFixture("expired-lease", 2, domain.JobProcessing),
		jobFixture("live-lease", 3, domain.JobProcessing),
		jobFixture("due-retry", 4, domain.JobRetryableFailed),
		jobFixture("future-retry", 5, domain.JobRetryableFailed),
		jobFixture("done", 6, domain.JobSucceeded),
	}
	jobs[1].ProcessingLeaseUntil = now.Add(-time.Second)
	jobs[2].ProcessingLeaseUntil = now.Add(time.Second)
	jobs[3].NextAttemptAt = now
	jobs[4].NextAttemptAt = now.Add(time.Second)
	for _, job := range jobs {
		require.NoError(t, repository.PutJob(job))
	}

	due, err := repository.ListDueJobs(now)
	require.NoError(t, err)
	ids := make([]string, 0, len(due))
	for _, job := range due {
		ids = append(ids, job.SourcePostID)
	}
	sort.Strings(ids)
	require.Equal(t, []string{"due-retry", "expired-lease", "pending"}, ids)
}

func TestListMappingsPaginatesPastTwoHundredKeys(t *testing.T) {
	t.Parallel()

	kv := newMemoryKV()
	repository := NewRepository(kv)
	for i := int64(0); i < 201; i++ {
		require.NoError(t, repository.PutMapping(domain.SourceMapping{
			SourcePostID:   "source-" + time.Unix(i, 0).UTC().Format("150405.000000000"),
			SourceUpdateAt: i,
			ChannelID:      "channel-1",
		}))
	}

	mappings, err := repository.ListMappings()
	require.NoError(t, err)
	require.Len(t, mappings, 201)
}

func TestSaveMappingCASDoesNotOverwriteNewerSourceVersion(t *testing.T) {
	t.Parallel()

	kv := newMemoryKV()
	repository := NewRepository(kv)
	old := domain.SourceMapping{
		SourcePostID:      "source-1",
		SourceUpdateAt:    101,
		ChannelID:         "channel-1",
		OperationID:       "operation-old",
		TranslationPostID: "translation-old",
	}
	require.NoError(t, repository.PutMapping(old))
	stale, token, found, err := repository.GetMappingForUpdate(old.SourcePostID)
	require.NoError(t, err)
	require.True(t, found)
	newer := old
	newer.SourceUpdateAt = 202
	newer.OperationID = "operation-new"
	require.NoError(t, repository.PutMapping(newer))
	staleFinalized := stale
	staleFinalized.TranslationPostID = "translation-stale-finalizer"

	stored, err := repository.SaveMappingCAS(token, staleFinalized)

	require.NoError(t, err)
	require.False(t, stored)
	current, found, err := repository.GetMapping(old.SourcePostID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, newer, current)
}

func TestCreateMappingDoesNotReplaceExistingMapping(t *testing.T) {
	t.Parallel()

	repository := NewRepository(newMemoryKV())
	existing := domain.SourceMapping{
		SourcePostID:   "source-1",
		SourceUpdateAt: 202,
		ChannelID:      "channel-1",
		OperationID:    "operation-new",
	}
	require.NoError(t, repository.PutMapping(existing))

	created, err := repository.CreateMapping(domain.SourceMapping{
		SourcePostID:   "source-1",
		SourceUpdateAt: 101,
		ChannelID:      "channel-1",
		OperationID:    "operation-old",
	})

	require.NoError(t, err)
	require.False(t, created)
	current, found, err := repository.GetMapping(existing.SourcePostID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, existing, current)
}

func TestSaveMappingCASRejectsSourceVersionRegression(t *testing.T) {
	t.Parallel()

	repository := NewRepository(newMemoryKV())
	current := domain.SourceMapping{
		SourcePostID:   "source-1",
		SourceUpdateAt: 202,
		ChannelID:      "channel-1",
		OperationID:    "operation-current",
	}
	require.NoError(t, repository.PutMapping(current))
	_, token, found, err := repository.GetMappingForUpdate(current.SourcePostID)
	require.NoError(t, err)
	require.True(t, found)
	regressed := current
	regressed.SourceUpdateAt = 101
	regressed.OperationID = "operation-stale"

	stored, err := repository.SaveMappingCAS(token, regressed)

	require.NoError(t, err)
	require.False(t, stored)
	require.Equal(t, current, mustStoredMapping(t, repository, current.SourcePostID))
}

func TestSaveMappingCASSwapsOnlyTheExactBytesRead(t *testing.T) {
	t.Parallel()

	kv := newMemoryKV()
	repository := NewRepository(kv)
	current := domain.SourceMapping{
		SourcePostID:   "source-1",
		SourceUpdateAt: 202,
		ChannelID:      "channel-1",
		OperationID:    "operation-current",
	}
	require.NoError(t, repository.PutMapping(current))
	read, token, found, err := repository.GetMappingForUpdate(current.SourcePostID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, current, read)
	changedBytes := append([]byte(token), '\n')
	require.NoError(t, kv.Set(MappingPrefix+current.SourcePostID, changedBytes))
	after := current
	after.TranslationPostID = "translation-stale"

	stored, err := repository.SaveMappingCAS(token, after)

	require.NoError(t, err)
	require.False(t, stored)
	require.Equal(t, changedBytes, kv.value(MappingPrefix+current.SourcePostID))
}

func TestAutomaticPublicationOutcomeRoundTripsAndLists(t *testing.T) {
	t.Parallel()

	repository := NewRepository(newMemoryKV())
	first := domain.AutomaticPublicationOutcome{
		IntentID:           "intent-202",
		SourcePostID:       "source-1",
		SourceUpdateAt:     202,
		ChannelID:          "channel-1",
		OperationID:        "operation-202",
		OperationStartedAt: time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC),
		State:              domain.AutomaticPublicationPublishing,
	}
	second := domain.AutomaticPublicationOutcome{
		IntentID:           "intent-303",
		SourcePostID:       "source-2",
		SourceUpdateAt:     303,
		ChannelID:          "channel-2",
		OperationID:        "operation-303",
		OperationStartedAt: time.Date(2026, 9, 2, 2, 0, 0, 0, time.UTC),
		State:              domain.AutomaticPublicationPublishing,
	}
	created, err := repository.CreateAutomaticPublicationOutcome(second)
	require.NoError(t, err)
	require.True(t, created)
	created, err = repository.CreateAutomaticPublicationOutcome(first)
	require.NoError(t, err)
	require.True(t, created)

	stored, found, err := repository.GetAutomaticPublicationOutcome(first.OperationID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, first, stored)
	outcomes, err := repository.ListAutomaticPublicationOutcomes()
	require.NoError(t, err)
	require.Equal(t, []domain.AutomaticPublicationOutcome{first, second}, outcomes)
	deleted, err := repository.DeleteAutomaticPublicationOutcomeCAS(first)
	require.NoError(t, err)
	require.True(t, deleted)
	_, found, err = repository.GetAutomaticPublicationOutcome(first.OperationID)
	require.NoError(t, err)
	require.False(t, found)
}

func TestAutomaticPublicationOutcomeTransitionsUseExactCAS(t *testing.T) {
	t.Parallel()

	repository := NewRepository(newMemoryKV())
	intent := domain.AutomaticPublicationOutcome{
		IntentID:           "intent-202",
		SourcePostID:       "source-1",
		SourceUpdateAt:     202,
		ChannelID:          "channel-1",
		OperationID:        "operation-202",
		OperationStartedAt: time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC),
		State:              domain.AutomaticPublicationPublishing,
	}
	created, err := repository.CreateAutomaticPublicationOutcome(intent)
	require.NoError(t, err)
	require.True(t, created)
	created, err = repository.CreateAutomaticPublicationOutcome(intent)
	require.NoError(t, err)
	require.False(t, created)

	published := intent
	published.State = domain.AutomaticPublicationPublished
	published.TranslationPostID = "translation-1"
	stored, err := repository.SaveAutomaticPublicationOutcomeCAS(intent, published)
	require.NoError(t, err)
	require.True(t, stored)
	stale := intent
	stale.TranslationPostID = "stale-post"
	stored, err = repository.SaveAutomaticPublicationOutcomeCAS(intent, stale)
	require.NoError(t, err)
	require.False(t, stored)
	deleted, err := repository.DeleteAutomaticPublicationOutcomeCAS(intent)
	require.NoError(t, err)
	require.False(t, deleted)
	deleted, err = repository.DeleteAutomaticPublicationOutcomeCAS(published)
	require.NoError(t, err)
	require.True(t, deleted)
	_, found, err := repository.GetAutomaticPublicationOutcome(intent.OperationID)
	require.NoError(t, err)
	require.False(t, found)
}

func TestAutomaticPublicationGuardRetainsVersionWatermarkAfterRelease(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)
	repository := NewRepository(newMemoryKV())
	newer := publicationGuardFixture(202, "operation-202", now.Add(time.Minute))
	acquired, ok, err := repository.AcquireAutomaticPublicationGuard(newer, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, newer, acquired)
	released, err := repository.ReleaseAutomaticPublicationGuard(acquired)
	require.NoError(t, err)
	require.True(t, released)
	stale := publicationGuardFixture(101, "operation-101", now.Add(2*time.Minute))

	current, ok, err := repository.AcquireAutomaticPublicationGuard(stale, now.Add(time.Minute))

	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, int64(202), current.SourceUpdateAt)
	require.Equal(t, "operation-202", current.OperationID)
	require.True(t, current.LeaseUntil.IsZero())
}

func TestAutomaticPublicationGuardLeaseSerializesAndAllowsExpiredTakeover(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)
	repository := NewRepository(newMemoryKV())
	older := publicationGuardFixture(202, "operation-202", now.Add(time.Minute))
	_, ok, err := repository.AcquireAutomaticPublicationGuard(older, now)
	require.NoError(t, err)
	require.True(t, ok)
	newer := publicationGuardFixture(303, "operation-303", now.Add(2*time.Minute))

	current, ok, err := repository.AcquireAutomaticPublicationGuard(newer, now.Add(30*time.Second))
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, older, current)
	current, ok, err = repository.AcquireAutomaticPublicationGuard(newer, now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, newer, current)
}

func TestAutomaticPublicationGuardUpdateFenceBlocksTakeoverPastLease(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)
	repository := NewRepository(newMemoryKV())
	older := publicationGuardFixture(202, "operation-202", now.Add(time.Minute))
	acquired, ok, err := repository.AcquireAutomaticPublicationGuard(older, now)
	require.NoError(t, err)
	require.True(t, ok)
	updating, ok, err := repository.BeginAutomaticPublicationUpdate(acquired)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, updating.Updating)
	newer := publicationGuardFixture(303, "operation-303", now.Add(3*time.Minute))

	current, ok, err := repository.AcquireAutomaticPublicationGuard(newer, now.Add(2*time.Minute))

	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, updating, current)
}

func TestAutomaticPublicationGuardRecoveryCASAdvancesButNeverLowersWatermark(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)
	repository := NewRepository(newMemoryKV())
	older := publicationGuardFixture(202, "operation-202", now.Add(time.Hour))
	acquired, ok, err := repository.AcquireAutomaticPublicationGuard(older, now)
	require.NoError(t, err)
	require.True(t, ok)
	updating, ok, err := repository.BeginAutomaticPublicationUpdate(acquired)
	require.NoError(t, err)
	require.True(t, ok)
	advanced := updating
	advanced.SourceUpdateAt = 303
	advanced.OperationID = "operation-303"
	advanced.LeaseUntil = time.Time{}
	advanced.Updating = false

	stored, err := repository.SaveAutomaticPublicationGuardCAS(updating, advanced)
	require.NoError(t, err)
	require.True(t, stored)
	lowered := advanced
	lowered.SourceUpdateAt = 101
	lowered.OperationID = "operation-101"
	stored, err = repository.SaveAutomaticPublicationGuardCAS(advanced, lowered)
	require.NoError(t, err)
	require.False(t, stored)
	current, found, err := repository.GetAutomaticPublicationGuard(older.TranslationPostID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, advanced, current)
}

func jobFixture(sourceID string, updateAt int64, state domain.JobState) domain.AutomaticJob {
	return domain.AutomaticJob{
		SourcePostID:       sourceID,
		SourceUpdateAt:     updateAt,
		ChannelID:          "channel-1",
		OperationID:        "operation-" + sourceID,
		OperationStartedAt: time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC),
		State:              state,
	}
}

func mustStoredMapping(t *testing.T, repository *Repository, sourceID string) domain.SourceMapping {
	t.Helper()
	mapping, found, err := repository.GetMapping(sourceID)
	require.NoError(t, err)
	require.True(t, found)
	return mapping
}

func publicationGuardFixture(sourceUpdateAt int64, operationID string, leaseUntil time.Time) domain.AutomaticPublicationGuard {
	return domain.AutomaticPublicationGuard{
		TranslationPostID: "translation-1",
		SourcePostID:      "source-1",
		SourceUpdateAt:    sourceUpdateAt,
		OperationID:       operationID,
		LeaseUntil:        leaseUntil,
	}
}

type memoryKV struct {
	mu          sync.Mutex
	values      map[string][]byte
	expiresIn   map[string]int64
	casFailures int
}

func newMemoryKV() *memoryKV {
	return &memoryKV{values: make(map[string][]byte), expiresIn: make(map[string]int64)}
}

func (kv *memoryKV) Get(key string) ([]byte, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return cloneBytes(kv.values[key]), nil
}

func (kv *memoryKV) Set(key string, value []byte) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.values[key] = cloneBytes(value)
	delete(kv.expiresIn, key)
	return nil
}

func (kv *memoryKV) SetWithExpiry(key string, value []byte, expireInSeconds int64) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.values[key] = cloneBytes(value)
	kv.expiresIn[key] = expireInSeconds
	return nil
}

func (kv *memoryKV) CompareAndSet(key string, oldValue, newValue []byte) (bool, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if kv.casFailures > 0 {
		kv.casFailures--
		return false, nil
	}
	current, exists := kv.values[key]
	if exists != (oldValue != nil) || !bytes.Equal(current, oldValue) {
		return false, nil
	}
	kv.values[key] = cloneBytes(newValue)
	delete(kv.expiresIn, key)
	return true, nil
}

func (kv *memoryKV) CompareAndSetWithExpiry(key string, oldValue, newValue []byte, expireInSeconds int64) (bool, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if kv.casFailures > 0 {
		kv.casFailures--
		return false, nil
	}
	current, exists := kv.values[key]
	if exists != (oldValue != nil) || !bytes.Equal(current, oldValue) {
		return false, nil
	}
	kv.values[key] = cloneBytes(newValue)
	kv.expiresIn[key] = expireInSeconds
	return true, nil
}

func (kv *memoryKV) CompareAndDelete(key string, oldValue []byte) (bool, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	current, exists := kv.values[key]
	if !exists || !bytes.Equal(current, oldValue) {
		return false, nil
	}
	delete(kv.values, key)
	delete(kv.expiresIn, key)
	return true, nil
}

func (kv *memoryKV) Delete(key string) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	delete(kv.values, key)
	delete(kv.expiresIn, key)
	return nil
}

func (kv *memoryKV) List(prefix string, page, perPage int) ([]string, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	keys := make([]string, 0)
	for key := range kv.values {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	start := page * perPage
	if start >= len(keys) {
		return []string{}, nil
	}
	end := start + perPage
	if end > len(keys) {
		end = len(keys)
	}
	return append([]string(nil), keys[start:end]...), nil
}

func (kv *memoryKV) value(key string) []byte {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return cloneBytes(kv.values[key])
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}
