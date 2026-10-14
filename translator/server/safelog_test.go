package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/worker"
)

type capturedSafeLog struct {
	mu      sync.Mutex
	entries []string
}

func (c *capturedSafeLog) LogError(message string, keyValuePairs ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, fmt.Sprint(append([]any{message}, keyValuePairs...)...))
}

func (c *capturedSafeLog) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.entries, "\n")
}

func TestSafeLoggerRejectsSensitiveValuesAtEveryTypedEntryPoint(t *testing.T) {
	sink := &capturedSafeLog{}
	logger := newSafeLogger(sink)
	sensitive := "AIza-secret source sentence 번역문 {\"raw\":true}"

	logger.LogRuntimeFailure(RuntimeFailureMetadata{
		Operation:     LogOperation(sensitive),
		CorrelationID: sensitive,
		SourcePostID:  sensitive,
		ChannelID:     sensitive,
		Model:         sensitive,
		Attempt:       2,
		Category:      sensitive,
	})
	logger.LogTerminalFailure(worker.FailureMetadata{
		Operation:     worker.Operation(sensitive),
		Model:         sensitive,
		CorrelationID: sensitive,
		SourcePostID:  sensitive,
		ChannelID:     sensitive,
		RootID:        sensitive,
		Attempt:       3,
		Category:      sensitive,
		Latency:       42,
	})

	logged := sink.joined()
	require.NotContains(t, logged, sensitive)
	require.NotContains(t, logged, "AIza")
	require.NotContains(t, logged, "source sentence")
	require.NotContains(t, logged, "번역문")
	require.NotContains(t, logged, "raw")
	require.Contains(t, logged, "attempt")
	require.Contains(t, logged, "category")
}

func TestSafeLoggerRejectsSingleTokenSecretsFromEveryStringField(t *testing.T) {
	sink := &capturedSafeLog{}
	logger := newSafeLogger(sink)
	secret := "AIzaSyD_FAKEKEY1234567890"
	logger.LogRuntimeFailure(RuntimeFailureMetadata{
		Operation: LogOperation(secret), CorrelationID: secret, SourcePostID: secret,
		ChannelID: secret, Model: secret, Category: secret,
	})
	logger.LogTerminalFailure(worker.FailureMetadata{
		Operation: worker.Operation(secret), Model: secret, CorrelationID: secret,
		SourcePostID: secret, ChannelID: secret, RootID: secret, Category: secret,
	})
	require.NotContains(t, sink.joined(), secret)
}

func TestSafeLoggerIncludesOnlyApprovedTerminalOperationAndModel(t *testing.T) {
	sink := &capturedSafeLog{}
	logger := newSafeLogger(sink)
	logger.LogTerminalFailure(worker.FailureMetadata{
		Operation: worker.OperationAutomaticTranslation,
		Model:     "gemini-3.7-flash",
		Category:  "authentication",
	})

	logged := sink.joined()
	require.Contains(t, logged, "operationautomatic_translation")
	require.Contains(t, logged, "modelgemini-3.7-flash")
}
