package worker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/gemini"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/limit"
)

var workerTestNow = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

func TestCapacityLeavesExtraAutomaticJobsPending(t *testing.T) {
	repository := newWorkerRepository(workerJob("source-1", domain.JobPending), workerJob("source-2", domain.JobPending))
	entered := make(chan string, 2)
	release := make(chan struct{})
	processor := processorFunc(func(_ context.Context, job domain.AutomaticJob) error {
		entered <- job.SourcePostID
		<-release
		return nil
	})
	clock := newWorkerClock(workerTestNow)
	pool := newPool(repository, processor, limit.New(1), 1, "gemini-test", discardLogger{}, clock)
	pool.Start(t.Context())
	defer pool.Stop()

	require.Equal(t, "source-1", receiveWorker(t, entered))
	require.Equal(t, domain.JobPending, repository.job("source-2").State)

	close(release)
	require.Equal(t, domain.JobSucceeded, receiveWorker(t, repository.saved).State)
}

func TestRetryableFailuresSchedule10SecondsOneMinuteFiveMinutesThenTerminal(t *testing.T) {
	repository := newWorkerRepository(workerJob("source-1", domain.JobPending))
	processed := make(chan int, 4)
	processor := processorFunc(func(_ context.Context, job domain.AutomaticJob) error {
		processed <- job.Attempt + 1
		return &gemini.Error{Category: gemini.ErrorCategoryRetryable, Status: 503, Model: "gemini-test", Attempt: 3}
	})
	clock := newWorkerClock(workerTestNow)
	pool := newPool(repository, processor, limit.New(1), 1, "gemini-test", discardLogger{}, clock)
	pool.Start(t.Context())
	defer pool.Stop()

	wantDelays := []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute}
	for attempt, delay := range wantDelays {
		require.Equal(t, attempt+1, receiveWorker(t, processed))
		saved := receiveWorker(t, repository.saved)
		require.Equal(t, domain.JobRetryableFailed, saved.State)
		require.Equal(t, attempt+1, saved.Attempt)
		require.Equal(t, clock.Now().Add(delay), saved.NextAttemptAt)
		require.Equal(t, "retryable", saved.LastErrorCategory)

		clock.Advance(delay)
	}

	require.Equal(t, 4, receiveWorker(t, processed))
	terminal := receiveWorker(t, repository.saved)
	require.Equal(t, domain.JobTerminalFailed, terminal.State)
	require.Equal(t, 4, terminal.Attempt)
	require.True(t, terminal.NextAttemptAt.IsZero())
	require.Equal(t, "retryable", terminal.LastErrorCategory)
}

func TestTerminalFailureLogsMetadataOnly(t *testing.T) {
	job := workerJob("source-sensitive", domain.JobPending)
	job.ChannelID = "channel-safe-id"
	job.RootID = "root-safe-id"
	job.OperationID = "correlation-safe-id"
	repository := newWorkerRepository(job)
	logger := newRecordingLogger()
	processor := processorFunc(func(context.Context, domain.AutomaticJob) error {
		return fmt.Errorf("wrapped secret message and key: %w", &gemini.Error{
			Category: gemini.ErrorCategoryAuthentication,
			Status:   401,
			Model:    "gemini-3.7-flash",
			Attempt:  1,
		})
	})
	clock := newWorkerClock(workerTestNow)
	pool := newPool(repository, processor, limit.New(1), 1, "gemini-configured", logger, clock)
	pool.Start(t.Context())
	defer pool.Stop()

	terminal := receiveWorker(t, repository.saved)
	require.Equal(t, domain.JobTerminalFailed, terminal.State)
	entry := receiveWorker(t, logger.entries)
	require.Equal(t, OperationAutomaticTranslation, entry.Operation)
	require.Equal(t, "gemini-3.7-flash", entry.Model)
	require.Equal(t, "correlation-safe-id", entry.CorrelationID)
	require.Equal(t, "source-sensitive", entry.SourcePostID)
	require.Equal(t, "channel-safe-id", entry.ChannelID)
	require.Equal(t, "root-safe-id", entry.RootID)
	require.Equal(t, 1, entry.Attempt)
	require.Equal(t, "authentication", entry.Category)
	require.Equal(t, int64(0), entry.Latency)

	logged := fmt.Sprint(entry)
	require.NotContains(t, logged, "secret message")
	require.NotContains(t, logged, "key:")
}

func TestTerminalPlainFailureLogsConfiguredModel(t *testing.T) {
	job := workerJob("source-plain", domain.JobRetryableFailed)
	job.Attempt = 3
	job.NextAttemptAt = workerTestNow
	repository := newWorkerRepository(job)
	logger := newRecordingLogger()
	processor := processorFunc(func(context.Context, domain.AutomaticJob) error {
		return errors.New("secret publication response")
	})
	clock := newWorkerClock(workerTestNow)
	pool := newPool(repository, processor, limit.New(1), 1, "gemini-3.7-flash", logger, clock)
	pool.Start(t.Context())
	defer pool.Stop()

	terminal := receiveWorker(t, repository.saved)
	require.Equal(t, domain.JobTerminalFailed, terminal.State)
	entry := receiveWorker(t, logger.entries)
	require.Equal(t, "gemini-3.7-flash", entry.Model)
	require.NotContains(t, fmt.Sprint(entry), "secret publication response")
}

func TestRestartClaimsExpiredProcessingLease(t *testing.T) {
	job := workerJob("source-1", domain.JobProcessing)
	job.ProcessingLeaseUntil = workerTestNow.Add(-time.Second)
	repository := newWorkerRepository(job)
	processed := make(chan domain.AutomaticJob, 1)
	processor := processorFunc(func(_ context.Context, claimed domain.AutomaticJob) error {
		processed <- claimed
		return nil
	})
	clock := newWorkerClock(workerTestNow)
	pool := newPool(repository, processor, limit.New(1), 1, "gemini-test", discardLogger{}, clock)
	pool.Start(t.Context())
	defer pool.Stop()

	claimed := receiveWorker(t, processed)
	require.Equal(t, domain.JobProcessing, claimed.State)
	require.Equal(t, workerTestNow.Add(30*time.Second), claimed.ProcessingLeaseUntil)
	require.Equal(t, domain.JobSucceeded, receiveWorker(t, repository.saved).State)
}

func TestStopWaitsForWorkersAndLeavesDurableJobs(t *testing.T) {
	repository := newWorkerRepository(workerJob("source-1", domain.JobPending))
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	processor := processorFunc(func(ctx context.Context, _ domain.AutomaticJob) error {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return ctx.Err()
	})
	clock := newWorkerClock(workerTestNow)
	pool := newPool(repository, processor, limit.New(1), 1, "gemini-test", discardLogger{}, clock)
	pool.Start(t.Context())
	receiveWorker(t, entered)

	stopped := make(chan struct{})
	go func() {
		pool.Stop()
		close(stopped)
	}()
	receiveWorker(t, cancelled)
	select {
	case <-stopped:
		t.Fatal("Stop returned before the active worker exited")
	default:
	}
	close(release)
	receiveWorker(t, stopped)

	stored := repository.job("source-1")
	require.Equal(t, domain.JobProcessing, stored.State)
	require.Equal(t, workerTestNow.Add(30*time.Second), stored.ProcessingLeaseUntil)
}

func TestSharedGateCapsCombinedAutomaticAndManualWork(t *testing.T) {
	repository := newWorkerRepository(workerJob("source-1", domain.JobPending))
	listed := repository.listed
	processed := make(chan struct{})
	processorRelease := make(chan struct{})
	processor := processorFunc(func(context.Context, domain.AutomaticJob) error {
		close(processed)
		<-processorRelease
		return nil
	})
	sharedGate := limit.New(1)
	releaseManual, ok := sharedGate.TryAcquire()
	require.True(t, ok)
	clock := newWorkerClock(workerTestNow)
	pool := newPool(repository, processor, sharedGate, 1, "gemini-test", discardLogger{}, clock)
	pool.Start(t.Context())
	defer pool.Stop()

	receiveWorker(t, listed)
	select {
	case <-processed:
		t.Fatal("automatic processing entered while manual work held the shared gate")
	default:
	}
	require.Equal(t, domain.JobPending, repository.job("source-1").State)
	_, ok = sharedGate.TryAcquire()
	require.False(t, ok)

	releaseManual()
	receiveWorker(t, processed)
	_, ok = sharedGate.TryAcquire()
	require.False(t, ok)
	close(processorRelease)
	require.Equal(t, domain.JobSucceeded, receiveWorker(t, repository.saved).State)
}

func TestNilProcessingResultRecordsStaleCancellationAsSuccess(t *testing.T) {
	repository := newWorkerRepository(workerJob("source-1", domain.JobPending))
	clock := newWorkerClock(workerTestNow)
	pool := newPool(repository, processorFunc(func(context.Context, domain.AutomaticJob) error {
		return nil
	}), limit.New(1), 1, "gemini-test", discardLogger{}, clock)
	pool.Start(t.Context())
	defer pool.Stop()

	saved := receiveWorker(t, repository.saved)
	require.Equal(t, domain.JobSucceeded, saved.State)
	require.Equal(t, 0, saved.Attempt)
	require.True(t, saved.NextAttemptAt.IsZero())
	require.True(t, saved.ProcessingLeaseUntil.IsZero())
}

func TestStopBeforeStartPermanentlyLeavesBufferedJobPending(t *testing.T) {
	repository := newWorkerRepository(workerJob("source-1", domain.JobPending))
	clock := newWorkerClock(workerTestNow)
	pool := newPool(repository, processorFunc(func(context.Context, domain.AutomaticJob) error {
		return nil
	}), limit.New(1), 1, "gemini-test", discardLogger{}, clock)
	pool.jobs <- jobID{sourcePostID: "source-1", sourceUpdateAt: 101}

	pool.Stop()
	ctx, cancel := context.WithCancel(t.Context())
	pool.Start(ctx)
	launchedAfterStop := pool.cancel != nil
	cancel()
	pool.wg.Wait()

	require.False(t, launchedAfterStop)
	require.Equal(t, domain.JobPending, repository.job("source-1").State)
	require.Len(t, pool.jobs, 1)
}

func TestConcurrentStopWaitsForStartToRegisterEveryGoroutine(t *testing.T) {
	priorProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(priorProcs) })

	parent := newBlockingParentContext()
	repository := newWorkerRepository()
	pool := newPool(repository, processorFunc(func(context.Context, domain.AutomaticJob) error {
		return nil
	}), limit.New(1), 1, "gemini-test", discardLogger{}, newWorkerClock(workerTestNow))
	startDone := make(chan struct{})
	go func() {
		pool.Start(parent)
		close(startDone)
	}()
	receiveWorker(t, parent.entered)

	stopStarted := make(chan struct{})
	stopDone := make(chan struct{})
	go func() {
		close(stopStarted)
		pool.Stop()
		close(stopDone)
	}()
	receiveWorker(t, stopStarted)
	runtime.Gosched()
	select {
	case <-stopDone:
		t.Fatal("Stop returned while Start had not finished registering its goroutines")
	default:
	}

	close(parent.release)
	receiveWorker(t, startDone)
	receiveWorker(t, stopDone)
}

func TestStopPreventsFinalSaveFromBeginningAfterProcessorReturns(t *testing.T) {
	priorProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(priorProcs) })

	repository := newWorkerRepository(workerJob("source-1", domain.JobPending))
	clock := newBlockingCompletionClock(workerTestNow)
	pool := newPool(repository, processorFunc(func(context.Context, domain.AutomaticJob) error {
		clock.BlockNextNow()
		return nil
	}), limit.New(1), 1, "gemini-test", discardLogger{}, clock)
	pool.Start(t.Context())
	receiveWorker(t, clock.blocked)

	stopStarted := make(chan struct{})
	stopDone := make(chan struct{})
	go func() {
		close(stopStarted)
		pool.Stop()
		close(stopDone)
	}()
	receiveWorker(t, stopStarted)
	runtime.Gosched()
	select {
	case <-stopDone:
		t.Fatal("Stop returned before the blocked worker drained")
	default:
	}
	close(clock.release)
	receiveWorker(t, stopDone)

	select {
	case saved := <-repository.saved:
		t.Fatalf("saved %s after Stop began", saved.State)
	default:
	}
	require.Equal(t, domain.JobProcessing, repository.job("source-1").State)
}

func TestBufferedIDSelectedDuringCancellationLeavesJobPending(t *testing.T) {
	repository := newWorkerRepository(
		workerJob("source-1", domain.JobPending),
		workerJob("source-2", domain.JobPending),
	)
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	unexpected := make(chan string, 1)
	pool := newPool(repository, processorFunc(func(ctx context.Context, job domain.AutomaticJob) error {
		if job.SourcePostID != "source-1" {
			unexpected <- job.SourcePostID
			return nil
		}
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return ctx.Err()
	}), limit.New(1), 1, "gemini-test", discardLogger{}, newWorkerClock(workerTestNow))
	pool.Start(t.Context())
	receiveWorker(t, entered)
	receiveWorker(t, repository.listed)
	pool.Wake()
	receiveWorker(t, repository.listed)
	// ListDueJobs signals before scan enqueues its result.
	require.Eventually(t, func() bool { return len(pool.jobs) == 1 }, time.Second, time.Millisecond)

	stopDone := make(chan struct{})
	go func() {
		pool.Stop()
		close(stopDone)
	}()
	receiveWorker(t, cancelled)
	close(release)
	receiveWorker(t, stopDone)

	require.Equal(t, domain.JobProcessing, repository.job("source-1").State)
	require.Equal(t, domain.JobPending, repository.job("source-2").State)
	select {
	case sourcePostID := <-unexpected:
		t.Fatalf("processed buffered job %s after cancellation", sourcePostID)
	default:
	}
}

type processorFunc func(context.Context, domain.AutomaticJob) error

func (f processorFunc) ProcessJob(ctx context.Context, job domain.AutomaticJob) error {
	return f(ctx, job)
}

type workerRepository struct {
	mu     sync.Mutex
	jobs   map[string]domain.AutomaticJob
	saved  chan domain.AutomaticJob
	listed chan struct{}
}

func newWorkerRepository(jobs ...domain.AutomaticJob) *workerRepository {
	repository := &workerRepository{
		jobs:   make(map[string]domain.AutomaticJob, len(jobs)),
		saved:  make(chan domain.AutomaticJob, 16),
		listed: make(chan struct{}, 16),
	}
	for _, job := range jobs {
		repository.jobs[workerJobKey(job.SourcePostID, job.SourceUpdateAt)] = job
	}
	return repository
}

func (r *workerRepository) ListDueJobs(now time.Time) ([]domain.AutomaticJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listed <- struct{}{}
	jobs := make([]domain.AutomaticJob, 0, len(r.jobs))
	for _, job := range r.jobs {
		if workerJobDue(job, now) {
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].SourcePostID == jobs[j].SourcePostID {
			return jobs[i].SourceUpdateAt < jobs[j].SourceUpdateAt
		}
		return jobs[i].SourcePostID < jobs[j].SourcePostID
	})
	return jobs, nil
}

func (r *workerRepository) ClaimDueJob(sourcePostID string, sourceUpdateAt int64, now, leaseUntil time.Time) (domain.AutomaticJob, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := workerJobKey(sourcePostID, sourceUpdateAt)
	job, ok := r.jobs[key]
	if !ok || !workerJobDue(job, now) {
		return job, false, nil
	}
	job.State = domain.JobProcessing
	job.ProcessingLeaseUntil = leaseUntil
	r.jobs[key] = job
	return job, true, nil
}

func (r *workerRepository) SaveJobCAS(before, after domain.AutomaticJob) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := workerJobKey(before.SourcePostID, before.SourceUpdateAt)
	current, ok := r.jobs[key]
	if !ok || !reflect.DeepEqual(current, before) {
		return false, nil
	}
	r.jobs[key] = after
	r.saved <- after
	return true, nil
}

func (r *workerRepository) job(sourcePostID string) domain.AutomaticJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, job := range r.jobs {
		if job.SourcePostID == sourcePostID {
			return job
		}
	}
	return domain.AutomaticJob{}
}

func workerJob(sourcePostID string, state domain.JobState) domain.AutomaticJob {
	return domain.AutomaticJob{
		SourcePostID:       sourcePostID,
		SourceUpdateAt:     101,
		ChannelID:          "channel-1",
		OperationID:        "operation-" + sourcePostID,
		OperationStartedAt: workerTestNow.Add(-time.Minute),
		State:              state,
	}
}

func workerJobKey(sourcePostID string, sourceUpdateAt int64) string {
	return fmt.Sprintf("%s/%d", sourcePostID, sourceUpdateAt)
}

func workerJobDue(job domain.AutomaticJob, now time.Time) bool {
	switch job.State {
	case domain.JobPending:
		return true
	case domain.JobRetryableFailed:
		return !job.NextAttemptAt.After(now)
	case domain.JobProcessing:
		return !job.ProcessingLeaseUntil.After(now)
	default:
		return false
	}
}

type workerClock struct {
	mu    sync.Mutex
	now   time.Time
	ticks chan time.Time
}

func newWorkerClock(now time.Time) *workerClock {
	return &workerClock{now: now, ticks: make(chan time.Time, 16)}
}

func (c *workerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *workerClock) NewTicker(time.Duration) Ticker {
	return workerTicker{ticks: c.ticks}
}

func (c *workerClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	now := c.now
	c.mu.Unlock()
	c.ticks <- now
}

type workerTicker struct {
	ticks <-chan time.Time
}

func (t workerTicker) C() <-chan time.Time { return t.ticks }
func (workerTicker) Stop()                 {}

type blockingParentContext struct {
	entered chan struct{}
	release chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newBlockingParentContext() *blockingParentContext {
	return &blockingParentContext{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (c *blockingParentContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *blockingParentContext) Done() <-chan struct{} {
	c.once.Do(func() {
		close(c.entered)
		<-c.release
	})
	return c.done
}
func (c *blockingParentContext) Err() error { return nil }
func (c *blockingParentContext) Value(any) any {
	return nil
}

type blockingCompletionClock struct {
	*workerClock
	muBlock sync.Mutex
	armed   bool
	blocked chan struct{}
	release chan struct{}
}

func newBlockingCompletionClock(now time.Time) *blockingCompletionClock {
	return &blockingCompletionClock{
		workerClock: newWorkerClock(now),
		blocked:     make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (c *blockingCompletionClock) BlockNextNow() {
	c.muBlock.Lock()
	c.armed = true
	c.muBlock.Unlock()
}

func (c *blockingCompletionClock) Now() time.Time {
	c.muBlock.Lock()
	if c.armed {
		c.armed = false
		close(c.blocked)
		c.muBlock.Unlock()
		<-c.release
		return c.workerClock.Now()
	}
	c.muBlock.Unlock()
	return c.workerClock.Now()
}

type recordingLogger struct {
	entries chan FailureMetadata
}

func newRecordingLogger() *recordingLogger {
	return &recordingLogger{entries: make(chan FailureMetadata, 4)}
}

func (l *recordingLogger) LogTerminalFailure(metadata FailureMetadata) {
	l.entries <- metadata
}

type discardLogger struct{}

func (discardLogger) LogTerminalFailure(FailureMetadata) {}

func receiveWorker[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-t.Context().Done():
		t.Fatal("test ended before expected channel value arrived")
		var zero T
		return zero
	}
}
