// Package worker runs OCR jobs on a fixed pool of goroutines.
//
// The in-memory channel is only a hand-off: the database is the durable queue.
// A job's receipt is marked PROCESSING before it is submitted, so anything lost
// from the channel (crash, shutdown) is found again by the startup recovery scan.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// ErrStopped is returned by Submit after Shutdown has begun.
var ErrStopped = errors.New("worker pool stopped")

// Config sizes the pool. Workers caps concurrent OCR calls (vendor rate limits);
// QueueSize caps how much work can wait before callers get backpressure.
type Config struct {
	Workers     int
	QueueSize   int
	JobTimeout  time.Duration // per attempt
	MaxAttempts int
	Backoff     time.Duration // doubled after each failed attempt
}

// Handler is what the pool calls.
type Handler struct {
	// Run processes one receipt. It must be idempotent: a job can run more than once.
	Run func(ctx context.Context, receiptID string) error
	// Retryable reports whether a failed attempt should be tried again.
	Retryable func(err error) bool
	// Fail records a job that will not be retried.
	Fail func(ctx context.Context, receiptID string, err error)
}

type Pool struct {
	cfg  Config
	h    Handler
	log  *slog.Logger
	jobs chan string

	mu      sync.RWMutex // guards stopped against concurrent Submit
	stopped bool

	quit   chan struct{}      // closed by Shutdown: take no new jobs
	ctx    context.Context    // parent of every attempt; cancelled if shutdown times out
	cancel context.CancelFunc //
	wg     sync.WaitGroup
}

func New(cfg Config, h Handler, log *slog.Logger) *Pool {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.QueueSize < 1 {
		cfg.QueueSize = 1
	}
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Pool{
		cfg: cfg, h: h, log: log,
		jobs: make(chan string, cfg.QueueSize),
		quit: make(chan struct{}), ctx: ctx, cancel: cancel,
	}
}

// Start launches the workers.
func (p *Pool) Start() {
	for i := 0; i < p.cfg.Workers; i++ {
		p.wg.Add(1)
		go p.loop()
	}
}

// TrySubmit queues a job without blocking. It returns false when the queue is full
// or the pool is stopping; the caller turns that into backpressure (HTTP 503).
func (p *Pool) TrySubmit(receiptID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.stopped {
		return false
	}
	select {
	case p.jobs <- receiptID:
		return true
	default:
		return false
	}
}

// Submit queues a job, waiting for room. Used by startup recovery, which may have
// more pending receipts than the queue holds.
func (p *Pool) Submit(ctx context.Context, receiptID string) error {
	for {
		if p.TrySubmit(receiptID) {
			return nil
		}
		p.mu.RLock()
		stopped := p.stopped
		p.mu.RUnlock()
		if stopped {
			return ErrStopped
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.quit:
			return ErrStopped
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Shutdown stops taking jobs and waits for in-flight ones to finish. If ctx expires
// first, in-flight attempts are cancelled. Jobs still queued or cancelled keep their
// PROCESSING status in the database and are picked up by recovery on the next start.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if !p.stopped {
		p.stopped = true
		close(p.quit)
	}
	p.mu.Unlock()

	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
		p.cancel()
		return nil
	case <-ctx.Done():
		p.cancel()
		<-done
		return ctx.Err()
	}
}

func (p *Pool) loop() {
	defer p.wg.Done()
	for {
		// Check quit first so a stopping pool does not start another queued job.
		select {
		case <-p.quit:
			return
		default:
		}
		select {
		case <-p.quit:
			return
		case id := <-p.jobs:
			p.handle(id)
		}
	}
}

func (p *Pool) handle(id string) {
	backoff := p.cfg.Backoff
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(p.ctx, p.cfg.JobTimeout)
		err := p.h.Run(ctx, id)
		cancel()
		if err == nil {
			return
		}
		if p.ctx.Err() != nil {
			// Shutdown cut this attempt short: leave the receipt PROCESSING for recovery.
			p.log.Warn("job interrupted by shutdown", "receipt_id", id)
			return
		}
		if !p.h.Retryable(err) || attempt >= p.cfg.MaxAttempts {
			p.log.Error("job failed", "receipt_id", id, "attempt", attempt, "err", err)
			failCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			p.h.Fail(failCtx, id, err)
			cancel()
			return
		}
		p.log.Warn("job attempt failed, retrying", "receipt_id", id, "attempt", attempt, "backoff", backoff, "err", err)
		select {
		case <-time.After(backoff):
		case <-p.ctx.Done():
			return
		}
		backoff *= 2
	}
}
