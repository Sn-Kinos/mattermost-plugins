package domain

import "time"

type JobState string

const (
	JobPending         JobState = "pending"
	JobProcessing      JobState = "processing"
	JobSucceeded       JobState = "succeeded"
	JobRetryableFailed JobState = "retryable_failed"
	JobTerminalFailed  JobState = "terminal_failed"
)

type AutomaticPublicationState string

const (
	AutomaticPublicationPublishing AutomaticPublicationState = "publishing"
	AutomaticPublicationPublished  AutomaticPublicationState = "published"
)

// AutomaticJob is intentionally compact. Workers reload source content from
// Mattermost instead of retaining message text in durable job records.
type AutomaticJob struct {
	SourcePostID         string    `json:"source_post_id"`
	SourceUpdateAt       int64     `json:"source_update_at"`
	ChannelID            string    `json:"channel_id"`
	RootID               string    `json:"root_id"`
	OperationID          string    `json:"operation_id"`
	OperationStartedAt   time.Time `json:"operation_started_at"`
	State                JobState  `json:"state"`
	Attempt              int       `json:"attempt"`
	NextAttemptAt        time.Time `json:"next_attempt_at"`
	ProcessingLeaseUntil time.Time `json:"processing_lease_until"`
	LastErrorCategory    string    `json:"last_error_category"`
}

type SourceMapping struct {
	SourcePostID       string    `json:"source_post_id"`
	SourceUpdateAt     int64     `json:"source_update_at"`
	ChannelID          string    `json:"channel_id"`
	OperationID        string    `json:"operation_id"`
	OperationStartedAt time.Time `json:"operation_started_at"`
	TranslationPostID  string    `json:"translation_post_id"`
	SuppressedUpdateAt int64     `json:"suppressed_update_at"`
	InternallyDeleting bool      `json:"internally_deleting"`
}

type DeletionIntent struct {
	TranslationPostID string `json:"translation_post_id"`
	SourcePostID      string `json:"source_post_id"`
	OperationID       string `json:"operation_id"`
}

// AutomaticPublicationOutcome is a text-free write-ahead record. Publishing
// is persisted before any public side effect; Published adds the returned ID.
type AutomaticPublicationOutcome struct {
	IntentID           string                    `json:"intent_id"`
	SourcePostID       string                    `json:"source_post_id"`
	SourceUpdateAt     int64                     `json:"source_update_at"`
	ChannelID          string                    `json:"channel_id"`
	OperationID        string                    `json:"operation_id"`
	OperationStartedAt time.Time                 `json:"operation_started_at"`
	KnownPostID        string                    `json:"known_post_id"`
	TranslationPostID  string                    `json:"translation_post_id"`
	State              AutomaticPublicationState `json:"state"`
}

// AutomaticPublicationGuard serializes updates to a shared translation post.
// SourceUpdateAt and OperationID remain as a monotonic watermark after release.
type AutomaticPublicationGuard struct {
	TranslationPostID string    `json:"translation_post_id"`
	SourcePostID      string    `json:"source_post_id"`
	SourceUpdateAt    int64     `json:"source_update_at"`
	OperationID       string    `json:"operation_id"`
	LeaseUntil        time.Time `json:"lease_until"`
	Updating          bool      `json:"updating"`
}

type KoreanTranslation struct {
	Text string `json:"text"`
}
