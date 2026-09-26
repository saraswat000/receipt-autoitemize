package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var errTransient = errors.New("vendor 503")
var errPermanent = errors.New("unreadable")

type recorder struct {
	mu     sync.Mutex
	failed map[string]error
}

func (r *recorder) fail(_ context.Context, id string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed == nil {
		r.failed = map[string]error{}
	}
	r.failed[id] = err
}

func (r *recorder) get(id string) (error, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	err, ok := r.failed[id]
	return err, ok
}

func cfg() Config {
	return Config{Workers: 2, QueueSize: 4, JobTimeout: time.Second, MaxAttempts: 3, Backoff: time.Millisecond}
}

func retryable(err error) bool { return !errors.Is(err, errPermanent) }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestRetriesTransientErrorsThenSucceeds(t *testing.T) {
	var calls, done atomic.Int32
	rec := &recorder{}
	p := New(cfg(), Handler{
		Run: func(context.Context, string) error {
			if calls.Add(1) < 3 {
				return errTransient
			}
			done.Add(1)
			return nil
		},
		Retryable: retryable, Fail: rec.fail,
	}, quiet)
	p.Start()
	defer p.Shutdown(context.Background())

	if !p.TrySubmit("r1") {
		t.Fatal("submit refused")
	}
	waitFor(t, func() bool { return done.Load() == 1 })
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
	if _, failed := rec.get("r1"); failed {
		t.Fatal("job marked failed after succeeding")
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	rec := &recorder{}
	p := New(cfg(), Handler{
		Run:       func(context.Context, string) error { calls.Add(1); return errPermanent },
		Retryable: retryable, Fail: rec.fail,
	}, quiet)
	p.Start()
	defer p.Shutdown(context.Background())

	p.TrySubmit("r1")
	waitFor(t, func() bool { _, ok := rec.get("r1"); return ok })
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	rec := &recorder{}
	p := New(cfg(), Handler{
		Run:       func(context.Context, string) error { calls.Add(1); return errTransient },
		Retryable: retryable, Fail: rec.fail,
	}, quiet)
	p.Start()
	defer p.Shutdown(context.Background())

	p.TrySubmit("r1")
	waitFor(t, func() bool { _, ok := rec.get("r1"); return ok })
	if err, _ := rec.get("r1"); !errors.Is(err, errTransient) || calls.Load() != 3 {
		t.Fatalf("err = %v, calls = %d", err, calls.Load())
	}
}

func TestAttemptTimeout(t *testing.T) {
	rec := &recorder{}
	c := cfg()
	c.JobTimeout, c.MaxAttempts = 20*time.Millisecond, 1
	p := New(c, Handler{
		Run: func(ctx context.Context, _ string) error {
			<-ctx.Done() // a vendor call that hangs
			return ctx.Err()
		},
		Retryable: retryable, Fail: rec.fail,
	}, quiet)
	p.Start()
	defer p.Shutdown(context.Background())

	p.TrySubmit("slow")
	waitFor(t, func() bool { _, ok := rec.get("slow"); return ok })
	if err, _ := rec.get("slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestBoundedConcurrencyAndBackpressure(t *testing.T) {
	release := make(chan struct{})
	var running, maxRunning atomic.Int32
	c := cfg()
	c.Workers, c.QueueSize = 2, 2
	p := New(c, Handler{
		Run: func(context.Context, string) error {
			n := running.Add(1)
			for {
				m := maxRunning.Load()
				if n <= m || maxRunning.CompareAndSwap(m, n) {
					break
				}
			}
			<-release
			running.Add(-1)
			return nil
		},
		Retryable: retryable, Fail: (&recorder{}).fail,
	}, quiet)
	p.Start()
	defer p.Shutdown(context.Background())

	p.TrySubmit("a")
	p.TrySubmit("b")
	waitFor(t, func() bool { return running.Load() == 2 }) // both workers busy
	if !p.TrySubmit("c") || !p.TrySubmit("d") {
		t.Fatal("queue should hold two waiting jobs")
	}
	if p.TrySubmit("e") {
		t.Fatal("expected backpressure when workers and queue are full")
	}
	close(release)
	waitFor(t, func() bool { return running.Load() == 0 && len(p.jobs) == 0 })
	if maxRunning.Load() != 2 {
		t.Fatalf("max concurrent = %d, want 2", maxRunning.Load())
	}
}

func TestShutdownFinishesInFlightAndLeavesQueued(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var ran sync.Map
	rec := &recorder{}
	c := cfg()
	c.Workers = 1
	p := New(c, Handler{
		Run: func(_ context.Context, id string) error {
			if id == "inflight" {
				close(started)
				<-release
			}
			ran.Store(id, true)
			return nil
		},
		Retryable: retryable, Fail: rec.fail,
	}, quiet)
	p.Start()
	p.TrySubmit("inflight")
	<-started
	p.TrySubmit("queued")

	done := make(chan error)
	go func() { done <- p.Shutdown(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := ran.Load("inflight"); !ok {
		t.Fatal("in-flight job was not allowed to finish")
	}
	if _, ok := ran.Load("queued"); ok {
		t.Fatal("queued job ran after shutdown began; it should wait for recovery")
	}
	if p.TrySubmit("late") {
		t.Fatal("submit accepted after shutdown")
	}
	if err := p.Submit(context.Background(), "late"); !errors.Is(err, ErrStopped) {
		t.Fatalf("Submit after shutdown = %v", err)
	}
}

func TestShutdownDeadlineCancelsInFlightWithoutFailingIt(t *testing.T) {
	started := make(chan struct{})
	rec := &recorder{}
	p := New(cfg(), Handler{
		Run: func(ctx context.Context, _ string) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
		Retryable: retryable, Fail: rec.fail,
	}, quiet)
	p.Start()
	p.TrySubmit("stuck")
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v", err)
	}
	if _, failed := rec.get("stuck"); failed {
		t.Fatal("interrupted job must stay PROCESSING for recovery, not be marked failed")
	}
}

func TestSubmitWaitsForRoom(t *testing.T) {
	release := make(chan struct{})
	var done atomic.Int32
	c := cfg()
	c.Workers, c.QueueSize = 1, 1
	p := New(c, Handler{
		Run:       func(context.Context, string) error { <-release; done.Add(1); return nil },
		Retryable: retryable, Fail: (&recorder{}).fail,
	}, quiet)
	p.Start()
	defer p.Shutdown(context.Background())

	errc := make(chan error, 1)
	go func() {
		for _, id := range []string{"a", "b", "c"} {
			if err := p.Submit(context.Background(), id); err != nil {
				errc <- err
				return
			}
		}
		errc <- nil
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return done.Load() == 3 })
}
