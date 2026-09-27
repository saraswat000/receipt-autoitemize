// Package worker runs OCR jobs on a fixed pool of goroutines.
//
// The in-memory channel is only a hand-off: the database is the durable queue.
// A job's receipt is marked PROCESSING before it is submitted, so anything lost
// from the channel (crash, shutdown) is found again by the startup recovery scan.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
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
	Backoff     time.Duration // doubled after each failed attempt...
	MaxBackoff  time.Duration // ...up to this cap (default 30s)
}

// PanicError is a job that panicked. It is never retried: the same input would
// panic again, and retrying (or re-queueing on restart) would loop forever.
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("job panicked: %v", e.Value) }

// Handler is what the pool calls.
type Handler struct {
	// Run processes one receipt. It must be idempotent: a job can run more than once.
	Run func(ctx context.Context, receiptID string) error
	// Retryable reports whether a failed attempt should be tried again.
	Retryable func(err error) bool
	// Fail records a job that will not be retried. If it cannot, the receipt stays
	// PROCESSING and startup recovery picks it up again.
	Fail func(ctx context.Context, receiptID string, err error) error
}

type Pool struct {
	cfg  Config
	h    Handler
	log  *slog.Logger
	jobs chan string

	quit     chan struct{} // closed by Shutdown: take no new jobs
	quitOnce sync.Once

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
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 30 * time.Second
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
	if p.stopping() {
		return false
	}
	select {
	case p.jobs <- receiptID:
		return true
	default:
		return false
	}
}

func (p *Pool) stopping() bool {
	select {
	case <-p.quit:
		return true
	default:
		return false
	}
}

// Submit queues a job, waiting for room. Used by startup recovery, which may have
// more pending receipts than the queue holds.
// jobs is never closed, so a blocked send is safe; quit and ctx unblock it. A job
// that slips in as Shutdown starts just stays PROCESSING and is recovered later.
func (p *Pool) Submit(ctx context.Context, receiptID string) error {
	if p.stopping() {
		return ErrStopped
	}
	select {
	case p.jobs <- receiptID:
		return nil
	case <-p.quit:
		return ErrStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops taking jobs and waits for in-flight ones to finish. If ctx expires
// first, in-flight attempts are cancelled. Jobs still queued or cancelled keep their
// PROCESSING status in the database and are picked up by recovery on the next start.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.quitOnce.Do(func() { close(p.quit) })

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
		err := p.attempt(id)
		if err == nil {
			return
		}
		var panicErr *PanicError
		isPanic := errors.As(err, &panicErr)
		if p.ctx.Err() != nil && !isPanic {
			// Shutdown cut this attempt short: leave the receipt PROCESSING for recovery.
			p.log.Warn("job interrupted by shutdown", "receipt_id", id)
			return
		}
		if isPanic || !p.h.Retryable(err) || attempt >= p.cfg.MaxAttempts {
			attrs := []any{"receipt_id", id, "attempt", attempt, "err", err}
			if isPanic {
				attrs = append(attrs, "stack", string(panicErr.Stack))
			}
			p.log.Error("job failed", attrs...)
			failCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if ferr := p.h.Fail(failCtx, id, err); ferr != nil {
				p.log.Error("could not record job failure; receipt stays PROCESSING until restart", "receipt_id", id, "err", ferr)
			}
			cancel()
			return
		}
		p.log.Warn("job attempt failed, retrying", "receipt_id", id, "attempt", attempt, "backoff", backoff, "err", err)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-p.quit:
			// Shutting down: do not hold shutdown for a backoff sleep. The receipt stays
			// PROCESSING and the next start retries it.
			timer.Stop()
			p.log.Warn("job retry abandoned for shutdown", "receipt_id", id)
			return
		}
		backoff = min(backoff*2, p.cfg.MaxBackoff)
	}
}

// attempt runs one try with its own timeout. A panic becomes a *PanicError, so one
// bad receipt cannot take the whole process down.
func (p *Pool) attempt(id string) (err error) {
	ctx, cancel := context.WithTimeout(p.ctx, p.cfg.JobTimeout)
	defer cancel()
	defer func() {
		if v := recover(); v != nil {
			err = &PanicError{Value: v, Stack: debug.Stack()}
		}
	}()
	return p.h.Run(ctx, id)
}
