package main

import (
	"regexp"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/worker"
)

type safeLogSink interface {
	LogError(message string, keyValuePairs ...any)
}

type LogOperation string

const (
	LogOperationActivation    LogOperation = "activation"
	LogOperationConfiguration LogOperation = "configuration"
	LogOperationPostedHook    LogOperation = "posted_hook"
	LogOperationUpdatedHook   LogOperation = "updated_hook"
	LogOperationDeletedHook   LogOperation = "deleted_hook"
	LogOperationCommand       LogOperation = "command"
	logMessageRuntimeFailure               = "translator operation failed"
	logMessageTerminalFailure              = "automatic translation job failed"
)

var safeLogValuePattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,160}$`)

type RuntimeFailureMetadata struct {
	Operation     LogOperation
	CorrelationID string
	SourcePostID  string
	ChannelID     string
	Model         string
	Attempt       int
	Category      string
	Latency       int64
}

type safeLogger struct {
	sink safeLogSink
}

func newSafeLogger(sink safeLogSink) *safeLogger {
	return &safeLogger{sink: sink}
}

func (l *safeLogger) LogRuntimeFailure(metadata RuntimeFailureMetadata) {
	if l == nil || l.sink == nil {
		return
	}
	l.sink.LogError(
		logMessageRuntimeFailure,
		"operation", safeOperation(metadata.Operation),
		"correlation_id", safeIdentifier(metadata.CorrelationID),
		"source_post_id", safeIdentifier(metadata.SourcePostID),
		"channel_id", safeIdentifier(metadata.ChannelID),
		"model", safeModel(metadata.Model),
		"attempt", nonnegative(metadata.Attempt),
		"category", safeCategory(metadata.Category),
		"latency_ms", nonnegative64(metadata.Latency),
	)
}

func (l *safeLogger) LogTerminalFailure(metadata worker.FailureMetadata) {
	if l == nil || l.sink == nil {
		return
	}
	l.sink.LogError(
		logMessageTerminalFailure,
		"operation", safeWorkerOperation(metadata.Operation),
		"model", safeModel(metadata.Model),
		"correlation_id", safeIdentifier(metadata.CorrelationID),
		"source_post_id", safeIdentifier(metadata.SourcePostID),
		"channel_id", safeIdentifier(metadata.ChannelID),
		"root_id", safeIdentifier(metadata.RootID),
		"attempt", nonnegative(metadata.Attempt),
		"category", safeCategory(metadata.Category),
		"latency_ms", nonnegative64(metadata.Latency),
	)
}

func safeWorkerOperation(operation worker.Operation) string {
	if operation == worker.OperationAutomaticTranslation {
		return string(operation)
	}
	return "unknown"
}

func safeOperation(operation LogOperation) string {
	switch operation {
	case LogOperationActivation,
		LogOperationConfiguration,
		LogOperationPostedHook,
		LogOperationUpdatedHook,
		LogOperationDeletedHook,
		LogOperationCommand:
		return string(operation)
	default:
		return "unknown"
	}
}

func safeIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "none"
	}
	if model.IsValidId(value) || isLowerHex(value, 32) || isLowerHex(value, 64) {
		return value
	}
	return "invalid"
}

func safeModel(value string) string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "gemini-") || !safeLogValuePattern.MatchString(value) {
		return "unknown"
	}
	return value
}

func safeCategory(category string) string {
	category = strings.ToLower(strings.TrimSpace(category))
	switch category {
	case "authentication", "configuration", "structured_output", "retryable",
		"internal", "invalid", "runtime", "reconcile", "command":
		return category
	default:
		return "internal"
	}
}

func isLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func nonnegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func nonnegative64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

var _ worker.Logger = (*safeLogger)(nil)
