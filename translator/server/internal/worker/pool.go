package worker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/gemini"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/limit"
)

const (
	scanInterval = time.Second
	jobLease     = 30 * time.Second
)

var retryDelays = []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute}

type Repository interface {
	ListDueJobs(now time.Time) ([]domain.AutomaticJob, error)
	ClaimDueJob(sourcePostID string, sourceUpdateAt int64, now, leaseUntil time.Time) (domain.AutomaticJob, bool, error)
	SaveJobCAS(before, after domain.AutomaticJob) (bool, error)
}

type Processor interface {
	ProcessJob(ctx context.Context, job domain.AutomaticJob) error
}

type Logger interface {
	LogTerminalFailure(metadata FailureMetadata)
}

type Operation string

const OperationAutomaticTranslation Operation = "automatic_translation"

// FailureMetadata is the complete, text-free terminal failure log contract.
// It deliberately cannot carry an error, message body, provider response, API
// key, or translation.
type FailureMetadata struct {
	Operation     Operation
	Model         string
	CorrelationID string
	SourcePostID  string
	ChannelID     string
	RootID        string
	Attempt       int
	Category      string
	Latency       int64
}

type Clock interface {
	Now() time.Time
	NewTicker(duration time.Duration) Ticker
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type Pool struct {
	repository  Repository
	processor   Processor
	gate        *limit.Gate
	concurrency int
	model       string
	logger      Logger
	clock       Clock

	wake chan struct{}
	jobs chan jobID

	lifecycleMu        sync.Mutex
	lifecycleCond      *sync.Cond
	state              poolState
	activeStateChanges int
	stopped            chan struct{}
	cancel             context.CancelFunc
	wg                 sync.WaitGroup

	inflightMu sync.Mutex
	inflight   map[jobID]struct{}
}

type poolState uint8

const (
	poolNew poolState = iota
	poolRunning
	poolStopping
	poolStopped
)

type jobID struct {
	sourcePostID   string
	sourceUpdateAt int64
}

func New(repository Repository, processor Processor, sharedGate *limit.Gate, concurrency int, model string, logger Logger) *Pool {
	return newPool(repository, processor, sharedGate, concurrency, model, logger, realClock{})
}

func newPool(
	repository Repository,
	processor Processor,
	sharedGate *limit.Gate,
	concurrency int,
	model string,
	logger Logger,
	clock Clock,
) *Pool {
	if concurrency < 1 {
		concurrency = 1
	}
	if sharedGate == nil {
		sharedGate = limit.New(concurrency)
	}
	if logger == nil {
		logger = noopLogger{}
	}
	if clock == nil {
		clock = realClock{}
	}
	pool := &Pool{
		repository:  repository,
		processor:   processor,
		gate:        sharedGate,
		concurrency: concurrency,
		model:       model,
		logger:      logger,
		clock:       clock,
		wake:        make(chan struct{}, 1),
		jobs:        make(chan jobID, concurrency),
		inflight:    make(map[jobID]struct{}),
		stopped:     make(chan struct{}),
	}
	pool.lifecycleCond = sync.NewCond(&pool.lifecycleMu)
	return pool
}

func (p *Pool) Start(parent context.Context) {
	if p == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	p.lifecycleMu.Lock()
	if p.state != poolNew {
		p.lifecycleMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	p.cancel = cancel
	p.state = poolRunning
	p.wg.Add(p.concurrency + 1)
	for range p.concurrency {
		go p.runWorker(ctx)
	}
	go p.runScheduler(ctx)
	p.lifecycleMu.Unlock()
	p.Wake()
}

func (p *Pool) Wake() {
	if p == nil {
		return
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Pool) Stop() {
	if p == nil {
		return
	}
	p.lifecycleMu.Lock()
	switch p.state {
	case poolNew:
		p.state = poolStopped
		close(p.stopped)
		p.lifecycleMu.Unlock()
		return
	case poolStopping:
		stopped := p.stopped
		p.lifecycleMu.Unlock()
		<-stopped
		return
	case poolStopped:
		p.lifecycleMu.Unlock()
		return
	case poolRunning:
		p.state = poolStopping
		p.cancel()
	}
	for p.activeStateChanges > 0 {
		p.lifecycleCond.Wait()
	}
	p.lifecycleMu.Unlock()

	p.wg.Wait()

	p.lifecycleMu.Lock()
	p.state = poolStopped
	close(p.stopped)
	p.lifecycleMu.Unlock()
}

func (p *Pool) runScheduler(ctx context.Context) {
	defer p.wg.Done()
	ticker := p.clock.NewTicker(scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
			p.scan(ctx)
		case <-ticker.C():
			p.scan(ctx)
		}
	}
}

func (p *Pool) scan(ctx context.Context) {
	jobs, err := p.repository.ListDueJobs(p.clock.Now())
	if err != nil {
		return
	}
	for _, job := range jobs {
		id := jobID{sourcePostID: job.SourcePostID, sourceUpdateAt: job.SourceUpdateAt}
		if !p.reserve(id) {
			continue
		}
		select {
		case p.jobs <- id:
		case <-ctx.Done():
			p.unreserve(id)
			return
		default:
			p.unreserve(id)
			return
		}
	}
}

func (p *Pool) reserve(id jobID) bool {
	p.inflightMu.Lock()
	defer p.inflightMu.Unlock()
	if _, exists := p.inflight[id]; exists {
		return false
	}
	p.inflight[id] = struct{}{}
	return true
}

func (p *Pool) unreserve(id jobID) {
	p.inflightMu.Lock()
	delete(p.inflight, id)
	p.inflightMu.Unlock()
}

func (p *Pool) runWorker(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-p.jobs:
			p.runJob(ctx, id)
			p.unreserve(id)
		}
	}
}

func (p *Pool) runJob(ctx context.Context, id jobID) {
	release, err := p.gate.Acquire(ctx)
	if err != nil {
		return
	}
	defer release()

	if !p.beginStateChange() {
		return
	}
	now := p.clock.Now()
	job, claimed, err := p.repository.ClaimDueJob(id.sourcePostID, id.sourceUpdateAt, now, now.Add(jobLease))
	p.endStateChange()
	if err != nil || !claimed {
		return
	}
	if ctx.Err() != nil {
		return
	}
	startedAt := p.clock.Now()
	err = p.processor.ProcessJob(ctx, job)
	if ctx.Err() != nil {
		return
	}
	completedAt := p.clock.Now()
	after, terminal := transitionJob(job, err, completedAt)
	if !p.beginStateChange() {
		return
	}
	saved, saveErr := p.repository.SaveJobCAS(job, after)
	p.endStateChange()
	if saveErr != nil || !saved || !terminal {
		return
	}
	p.logger.LogTerminalFailure(FailureMetadata{
		Operation:     OperationAutomaticTranslation,
		Model:         failureModel(err, p.model),
		CorrelationID: job.OperationID,
		SourcePostID:  job.SourcePostID,
		ChannelID:     job.ChannelID,
		RootID:        job.RootID,
		Attempt:       after.Attempt,
		Category:      after.LastErrorCategory,
		Latency:       completedAt.Sub(startedAt).Milliseconds(),
	})
}

func failureModel(err error, configuredModel string) string {
	var geminiErr *gemini.Error
	if errors.As(err, &geminiErr) && geminiErr.Model != "" {
		return geminiErr.Model
	}
	return configuredModel
}

func (p *Pool) beginStateChange() bool {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.state != poolRunning {
		return false
	}
	p.activeStateChanges++
	return true
}

func (p *Pool) endStateChange() {
	p.lifecycleMu.Lock()
	p.activeStateChanges--
	if p.activeStateChanges == 0 {
		p.lifecycleCond.Broadcast()
	}
	p.lifecycleMu.Unlock()
}

func transitionJob(before domain.AutomaticJob, processingErr error, now time.Time) (domain.AutomaticJob, bool) {
	after := before
	after.ProcessingLeaseUntil = time.Time{}
	after.NextAttemptAt = time.Time{}
	if processingErr == nil {
		after.State = domain.JobSucceeded
		after.LastErrorCategory = ""
		return after, false
	}

	category, retryable := errorCategory(processingErr)
	after.Attempt++
	after.LastErrorCategory = category
	if retryable && after.Attempt <= len(retryDelays) {
		after.State = domain.JobRetryableFailed
		after.NextAttemptAt = now.Add(retryDelays[after.Attempt-1])
		return after, false
	}
	after.State = domain.JobTerminalFailed
	return after, true
}

func errorCategory(err error) (string, bool) {
	var geminiErr *gemini.Error
	if errors.As(err, &geminiErr) {
		return string(geminiErr.Category), geminiErr.Category == gemini.ErrorCategoryRetryable
	}
	return "internal", true
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }
func (realClock) NewTicker(duration time.Duration) Ticker {
	return realTicker{ticker: time.NewTicker(duration)}
}

type realTicker struct {
	ticker *time.Ticker
}

func (t realTicker) C() <-chan time.Time { return t.ticker.C }
func (t realTicker) Stop()               { t.ticker.Stop() }

type noopLogger struct{}

func (noopLogger) LogTerminalFailure(FailureMetadata) {}
