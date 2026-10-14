package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
)

const (
	JobPrefix                = "job/"
	MappingPrefix            = "mapping/"
	DeletionIntentPrefix     = "deletion-intent/"
	PublicationOutcomePrefix = "publication-outcome/"
	PublicationGuardPrefix   = "publication-guard/"

	listPageSize       = 200
	maxContentionTries = 8
)

var ErrContention = errors.New("store contention")

type Repository struct {
	kv KV
}

func NewRepository(kv KV) *Repository {
	return &Repository{kv: kv}
}

func (r *Repository) PutJob(job domain.AutomaticJob) error {
	value, err := encode(job)
	if err != nil {
		return err
	}
	return r.kv.Set(jobKey(job.SourcePostID, job.SourceUpdateAt), value)
}

func (r *Repository) GetJob(sourcePostID string, sourceUpdateAt int64) (domain.AutomaticJob, bool, error) {
	return get[domain.AutomaticJob](r.kv, jobKey(sourcePostID, sourceUpdateAt))
}

func (r *Repository) ClaimDueJob(sourcePostID string, sourceUpdateAt int64, now, leaseUntil time.Time) (domain.AutomaticJob, bool, error) {
	key := jobKey(sourcePostID, sourceUpdateAt)
	for range maxContentionTries {
		oldValue, err := r.kv.Get(key)
		if err != nil {
			return domain.AutomaticJob{}, false, err
		}
		if oldValue == nil {
			return domain.AutomaticJob{}, false, nil
		}

		job, err := decode[domain.AutomaticJob](oldValue)
		if err != nil {
			return domain.AutomaticJob{}, false, fmt.Errorf("decode job %q: %w", key, err)
		}
		if !jobIsDue(job, now) {
			return job, false, nil
		}

		job.State = domain.JobProcessing
		job.ProcessingLeaseUntil = leaseUntil
		newValue, err := encode(job)
		if err != nil {
			return domain.AutomaticJob{}, false, err
		}
		stored, err := r.kv.CompareAndSet(key, oldValue, newValue)
		if err != nil {
			return domain.AutomaticJob{}, false, err
		}
		if stored {
			return job, true, nil
		}
	}
	return domain.AutomaticJob{}, false, ErrContention
}

func (r *Repository) SaveJobCAS(before, after domain.AutomaticJob) (bool, error) {
	key := jobKey(before.SourcePostID, before.SourceUpdateAt)
	if key != jobKey(after.SourcePostID, after.SourceUpdateAt) {
		return false, nil
	}
	for range maxContentionTries {
		oldValue, err := r.kv.Get(key)
		if err != nil {
			return false, err
		}
		if oldValue == nil {
			return false, nil
		}
		current, err := decode[domain.AutomaticJob](oldValue)
		if err != nil {
			return false, fmt.Errorf("decode job %q: %w", key, err)
		}
		if !reflect.DeepEqual(current, before) {
			return false, nil
		}
		newValue, err := encode(after)
		if err != nil {
			return false, err
		}
		stored, err := r.kv.CompareAndSet(key, oldValue, newValue)
		if err != nil {
			return false, err
		}
		if stored {
			return true, nil
		}
	}
	return false, ErrContention
}

func (r *Repository) ListDueJobs(now time.Time) ([]domain.AutomaticJob, error) {
	jobs, err := listRecords[domain.AutomaticJob](r.kv, JobPrefix)
	if err != nil {
		return nil, err
	}
	due := make([]domain.AutomaticJob, 0, len(jobs))
	for _, job := range jobs {
		if jobIsDue(job, now) {
			due = append(due, job)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].SourcePostID == due[j].SourcePostID {
			return due[i].SourceUpdateAt < due[j].SourceUpdateAt
		}
		return due[i].SourcePostID < due[j].SourcePostID
	})
	return due, nil
}

func (r *Repository) PutMapping(mapping domain.SourceMapping) error {
	value, err := encode(mapping)
	if err != nil {
		return err
	}
	return r.kv.Set(MappingPrefix+mapping.SourcePostID, value)
}

func (r *Repository) CreateMapping(mapping domain.SourceMapping) (bool, error) {
	key := MappingPrefix + mapping.SourcePostID
	value, err := encode(mapping)
	if err != nil {
		return false, err
	}
	for range maxContentionTries {
		oldValue, err := r.kv.Get(key)
		if err != nil {
			return false, err
		}
		if oldValue != nil {
			return false, nil
		}
		stored, err := r.kv.CompareAndSet(key, nil, value)
		if err != nil {
			return false, err
		}
		if stored {
			return true, nil
		}
	}
	return false, ErrContention
}

func (r *Repository) SaveMappingCAS(beforeToken string, after domain.SourceMapping) (bool, error) {
	if beforeToken == "" {
		return false, nil
	}
	before, err := decode[domain.SourceMapping]([]byte(beforeToken))
	if err != nil {
		return false, fmt.Errorf("decode mapping CAS token: %w", err)
	}
	if before.SourcePostID != after.SourcePostID || after.SourceUpdateAt < before.SourceUpdateAt {
		return false, nil
	}
	key := MappingPrefix + before.SourcePostID
	newValue, err := encode(after)
	if err != nil {
		return false, err
	}
	for range maxContentionTries {
		stored, err := r.kv.CompareAndSet(key, []byte(beforeToken), newValue)
		if err != nil {
			return false, err
		}
		if stored {
			return true, nil
		}
		currentValue, err := r.kv.Get(key)
		if err != nil {
			return false, err
		}
		if !bytes.Equal(currentValue, []byte(beforeToken)) {
			return false, nil
		}
	}
	return false, ErrContention
}

func (r *Repository) GetMapping(sourcePostID string) (domain.SourceMapping, bool, error) {
	return get[domain.SourceMapping](r.kv, MappingPrefix+sourcePostID)
}

func (r *Repository) GetMappingForUpdate(sourcePostID string) (domain.SourceMapping, string, bool, error) {
	key := MappingPrefix + sourcePostID
	value, err := r.kv.Get(key)
	if err != nil {
		return domain.SourceMapping{}, "", false, err
	}
	if value == nil {
		return domain.SourceMapping{}, "", false, nil
	}
	mapping, err := decode[domain.SourceMapping](value)
	if err != nil {
		return domain.SourceMapping{}, "", false, fmt.Errorf("decode mapping %q: %w", key, err)
	}
	return mapping, string(value), true, nil
}

func (r *Repository) ListMappings() ([]domain.SourceMapping, error) {
	return listRecords[domain.SourceMapping](r.kv, MappingPrefix)
}

func (r *Repository) PutDeletionIntent(intent domain.DeletionIntent, expireInSeconds int64) error {
	value, err := encode(intent)
	if err != nil {
		return err
	}
	return r.kv.SetWithExpiry(DeletionIntentPrefix+intent.TranslationPostID, value, expireInSeconds)
}

func (r *Repository) GetDeletionIntent(translationPostID string) (domain.DeletionIntent, bool, error) {
	return get[domain.DeletionIntent](r.kv, DeletionIntentPrefix+translationPostID)
}

func (r *Repository) ConsumeDeletionIntent(translationPostID string) (bool, error) {
	key := DeletionIntentPrefix + translationPostID
	oldValue, err := r.kv.Get(key)
	if err != nil || oldValue == nil {
		return false, err
	}
	consumed, err := r.kv.CompareAndDelete(key, oldValue)
	if err != nil {
		return false, err
	}
	return consumed, nil
}

func (r *Repository) CreateAutomaticPublicationOutcome(outcome domain.AutomaticPublicationOutcome) (bool, error) {
	if outcome.IntentID == "" || outcome.OperationID == "" ||
		outcome.State != domain.AutomaticPublicationPublishing || outcome.TranslationPostID != "" {
		return false, nil
	}
	value, err := encode(outcome)
	if err != nil {
		return false, err
	}
	return r.kv.CompareAndSet(PublicationOutcomePrefix+outcome.OperationID, nil, value)
}

func (r *Repository) SaveAutomaticPublicationOutcomeCAS(
	before, after domain.AutomaticPublicationOutcome,
) (bool, error) {
	if before.OperationID == "" || before.OperationID != after.OperationID ||
		before.State != domain.AutomaticPublicationPublishing || before.TranslationPostID != "" ||
		after.State != domain.AutomaticPublicationPublished || after.TranslationPostID == "" ||
		!sameAutomaticPublicationIntent(before, after) {
		return false, nil
	}
	oldValue, err := encode(before)
	if err != nil {
		return false, err
	}
	newValue, err := encode(after)
	if err != nil {
		return false, err
	}
	return r.kv.CompareAndSet(PublicationOutcomePrefix+before.OperationID, oldValue, newValue)
}

func sameAutomaticPublicationIntent(before, after domain.AutomaticPublicationOutcome) bool {
	return before.IntentID == after.IntentID &&
		before.SourcePostID == after.SourcePostID &&
		before.SourceUpdateAt == after.SourceUpdateAt &&
		before.ChannelID == after.ChannelID &&
		before.OperationID == after.OperationID &&
		before.OperationStartedAt.Equal(after.OperationStartedAt) &&
		before.KnownPostID == after.KnownPostID
}

func (r *Repository) DeleteAutomaticPublicationOutcomeCAS(outcome domain.AutomaticPublicationOutcome) (bool, error) {
	if outcome.OperationID == "" {
		return false, nil
	}
	value, err := encode(outcome)
	if err != nil {
		return false, err
	}
	return r.kv.CompareAndDelete(PublicationOutcomePrefix+outcome.OperationID, value)
}

func (r *Repository) GetAutomaticPublicationOutcome(operationID string) (domain.AutomaticPublicationOutcome, bool, error) {
	return get[domain.AutomaticPublicationOutcome](r.kv, PublicationOutcomePrefix+operationID)
}

func (r *Repository) ListAutomaticPublicationOutcomes() ([]domain.AutomaticPublicationOutcome, error) {
	outcomes, err := listRecords[domain.AutomaticPublicationOutcome](r.kv, PublicationOutcomePrefix)
	if err != nil {
		return nil, err
	}
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].OperationID < outcomes[j].OperationID })
	return outcomes, nil
}

func (r *Repository) GetAutomaticPublicationGuard(translationPostID string) (domain.AutomaticPublicationGuard, bool, error) {
	return get[domain.AutomaticPublicationGuard](r.kv, PublicationGuardPrefix+translationPostID)
}

func (r *Repository) ListAutomaticPublicationGuards() ([]domain.AutomaticPublicationGuard, error) {
	guards, err := listRecords[domain.AutomaticPublicationGuard](r.kv, PublicationGuardPrefix)
	if err != nil {
		return nil, err
	}
	sort.Slice(guards, func(i, j int) bool { return guards[i].TranslationPostID < guards[j].TranslationPostID })
	return guards, nil
}

func (r *Repository) AcquireAutomaticPublicationGuard(
	desired domain.AutomaticPublicationGuard,
	now time.Time,
) (domain.AutomaticPublicationGuard, bool, error) {
	key := PublicationGuardPrefix + desired.TranslationPostID
	desired.Updating = false
	value, err := encode(desired)
	if err != nil {
		return domain.AutomaticPublicationGuard{}, false, err
	}
	for range maxContentionTries {
		oldValue, err := r.kv.Get(key)
		if err != nil {
			return domain.AutomaticPublicationGuard{}, false, err
		}
		if oldValue == nil {
			stored, err := r.kv.CompareAndSet(key, nil, value)
			if err != nil {
				return domain.AutomaticPublicationGuard{}, false, err
			}
			if stored {
				return desired, true, nil
			}
			continue
		}
		current, err := decode[domain.AutomaticPublicationGuard](oldValue)
		if err != nil {
			return domain.AutomaticPublicationGuard{}, false, fmt.Errorf("decode publication guard %q: %w", key, err)
		}
		if current.TranslationPostID != desired.TranslationPostID || current.SourcePostID != desired.SourcePostID ||
			current.Updating || current.LeaseUntil.After(now) || current.SourceUpdateAt > desired.SourceUpdateAt ||
			current.SourceUpdateAt == desired.SourceUpdateAt && current.OperationID != desired.OperationID {
			return current, false, nil
		}
		stored, err := r.kv.CompareAndSet(key, oldValue, value)
		if err != nil {
			return domain.AutomaticPublicationGuard{}, false, err
		}
		if stored {
			return desired, true, nil
		}
	}
	return domain.AutomaticPublicationGuard{}, false, ErrContention
}

func (r *Repository) BeginAutomaticPublicationUpdate(
	before domain.AutomaticPublicationGuard,
) (domain.AutomaticPublicationGuard, bool, error) {
	if before.Updating {
		return before, false, nil
	}
	after := before
	after.Updating = true
	return r.swapAutomaticPublicationGuard(before, after)
}

func (r *Repository) ReleaseAutomaticPublicationGuard(before domain.AutomaticPublicationGuard) (bool, error) {
	after := before
	after.LeaseUntil = time.Time{}
	after.Updating = false
	_, stored, err := r.swapAutomaticPublicationGuard(before, after)
	return stored, err
}

func (r *Repository) SaveAutomaticPublicationGuardCAS(
	before, after domain.AutomaticPublicationGuard,
) (bool, error) {
	if before.TranslationPostID == "" || before.TranslationPostID != after.TranslationPostID ||
		before.SourcePostID != after.SourcePostID || after.SourceUpdateAt < before.SourceUpdateAt ||
		after.SourceUpdateAt == before.SourceUpdateAt && after.OperationID != before.OperationID ||
		after.OperationID == "" || after.Updating || !after.LeaseUntil.IsZero() {
		return false, nil
	}
	_, stored, err := r.swapAutomaticPublicationGuard(before, after)
	return stored, err
}

func (r *Repository) swapAutomaticPublicationGuard(
	before, after domain.AutomaticPublicationGuard,
) (domain.AutomaticPublicationGuard, bool, error) {
	key := PublicationGuardPrefix + before.TranslationPostID
	for range maxContentionTries {
		oldValue, err := r.kv.Get(key)
		if err != nil {
			return domain.AutomaticPublicationGuard{}, false, err
		}
		if oldValue == nil {
			return domain.AutomaticPublicationGuard{}, false, nil
		}
		current, err := decode[domain.AutomaticPublicationGuard](oldValue)
		if err != nil {
			return domain.AutomaticPublicationGuard{}, false, fmt.Errorf("decode publication guard %q: %w", key, err)
		}
		if !reflect.DeepEqual(current, before) {
			return current, false, nil
		}
		newValue, err := encode(after)
		if err != nil {
			return domain.AutomaticPublicationGuard{}, false, err
		}
		stored, err := r.kv.CompareAndSet(key, oldValue, newValue)
		if err != nil {
			return domain.AutomaticPublicationGuard{}, false, err
		}
		if stored {
			return after, true, nil
		}
	}
	return domain.AutomaticPublicationGuard{}, false, ErrContention
}

func jobKey(sourcePostID string, sourceUpdateAt int64) string {
	return fmt.Sprintf("%s%s/%d", JobPrefix, sourcePostID, sourceUpdateAt)
}

func jobIsDue(job domain.AutomaticJob, now time.Time) bool {
	switch job.State {
	case domain.JobPending:
		return true
	case domain.JobProcessing:
		return !job.ProcessingLeaseUntil.After(now)
	case domain.JobRetryableFailed:
		return !job.NextAttemptAt.After(now)
	default:
		return false
	}
}

func listRecords[T any](kv KV, prefix string) ([]T, error) {
	keys := make([]string, 0)
	for page := 0; ; page++ {
		batch, err := kv.List(prefix, page, listPageSize)
		if err != nil {
			return nil, err
		}
		keys = append(keys, batch...)
		if len(batch) < listPageSize {
			break
		}
	}

	records := make([]T, 0, len(keys))
	for _, key := range keys {
		value, err := kv.Get(key)
		if err != nil {
			return nil, err
		}
		if value == nil {
			continue
		}
		record, err := decode[T](value)
		if err != nil {
			return nil, fmt.Errorf("decode record %q: %w", key, err)
		}
		records = append(records, record)
	}
	return records, nil
}

func get[T any](kv KV, key string) (T, bool, error) {
	var zero T
	value, err := kv.Get(key)
	if err != nil {
		return zero, false, err
	}
	if value == nil {
		return zero, false, nil
	}
	record, err := decode[T](value)
	if err != nil {
		return zero, false, fmt.Errorf("decode record %q: %w", key, err)
	}
	return record, true, nil
}

func encode(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode store record: %w", err)
	}
	return encoded, nil
}

func decode[T any](value []byte) (T, error) {
	var decoded T
	err := json.Unmarshal(value, &decoded)
	return decoded, err
}
