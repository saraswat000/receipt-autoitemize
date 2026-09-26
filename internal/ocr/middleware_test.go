package ocr

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEngine counts calls and returns a canned result.
type fakeEngine struct {
	calls atomic.Int32
	text  string
	err   error
	delay time.Duration
}

func (*fakeEngine) Name() string { return "fake" }

func (f *fakeEngine) ExtractText(ctx context.Context, in Input) (string, error) {
	f.calls.Add(1)
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return f.text + in.Filename, f.err
}

func TestChainOrderAndName(t *testing.T) {
	var order []string
	tag := func(name string) Middleware {
		return func(next Engine) Engine {
			return funcEngine{next, func(ctx context.Context, in Input) (string, error) {
				order = append(order, name)
				return next.ExtractText(ctx, in)
			}}
		}
	}
	e := Chain(&fakeEngine{}, tag("a"), tag("b"), tag("c"))
	e.ExtractText(context.Background(), Input{})
	if strings.Join(order, ",") != "a,b,c" {
		t.Fatalf("order = %v", order)
	}
	if e.Name() != "fake" {
		t.Fatalf("decorators must keep the engine name, got %q", e.Name())
	}
}

type funcEngine struct {
	Engine
	fn func(context.Context, Input) (string, error)
}

func (f funcEngine) ExtractText(ctx context.Context, in Input) (string, error) { return f.fn(ctx, in) }

func TestCacheHitsSkipTheEngine(t *testing.T) {
	f := &fakeEngine{text: "text:"}
	e := Chain(f, WithCache(2))
	ctx := context.Background()
	a := Input{SHA256: "aa", Filename: "a.jpg"}

	for i := 0; i < 3; i++ {
		if got, err := e.ExtractText(ctx, a); err != nil || got != "text:a.jpg" {
			t.Fatalf("%q %v", got, err)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatalf("engine called %d times, want 1", f.calls.Load())
	}
	// Same bytes under another name is a different key (the stub maps names to text).
	e.ExtractText(ctx, Input{SHA256: "aa", Filename: "b.jpg"})
	// No hash means no caching.
	e.ExtractText(ctx, Input{Filename: "c.jpg"})
	e.ExtractText(ctx, Input{Filename: "c.jpg"})
	if f.calls.Load() != 4 {
		t.Fatalf("engine called %d times, want 4", f.calls.Load())
	}
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	f := &fakeEngine{}
	e := Chain(f, WithCache(2))
	ctx := context.Background()
	in := func(h string) Input { return Input{SHA256: h, Filename: h} }

	e.ExtractText(ctx, in("1"))
	e.ExtractText(ctx, in("2"))
	e.ExtractText(ctx, in("1")) // 1 is now most recent
	e.ExtractText(ctx, in("3")) // evicts 2
	e.ExtractText(ctx, in("1")) // hit
	if f.calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", f.calls.Load())
	}
	e.ExtractText(ctx, in("2")) // miss: was evicted
	if f.calls.Load() != 4 {
		t.Fatalf("calls = %d, want 4", f.calls.Load())
	}
}

func TestCacheDoesNotStoreErrors(t *testing.T) {
	f := &fakeEngine{err: ErrNoText}
	e := Chain(f, WithCache(10))
	in := Input{SHA256: "x", Filename: "x"}
	for i := 0; i < 2; i++ {
		if _, err := e.ExtractText(context.Background(), in); !errors.Is(err, ErrNoText) {
			t.Fatal(err)
		}
	}
	if f.calls.Load() != 2 {
		t.Fatalf("a failure was served from cache: %d calls", f.calls.Load())
	}
}

func TestTimeoutCancelsSlowEngine(t *testing.T) {
	e := Chain(&fakeEngine{delay: time.Second}, WithTimeout(10*time.Millisecond))
	start := time.Now()
	if _, err := e.ExtractText(context.Background(), Input{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("timeout did not cut the call short")
	}
}

func TestLoggingRecordsOutcome(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	Chain(&fakeEngine{}, WithLogging(log)).ExtractText(context.Background(), Input{SHA256: "ab"})
	Chain(&fakeEngine{err: ErrNoText}, WithLogging(log)).ExtractText(context.Background(), Input{})
	out := buf.String()
	if !strings.Contains(out, "ocr done") || !strings.Contains(out, "sha256=ab") || !strings.Contains(out, "ocr failed") {
		t.Fatal(out)
	}
}
