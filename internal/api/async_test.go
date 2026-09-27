package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"receipt-autoitemize/internal/api"
	"receipt-autoitemize/internal/domain"
	"receipt-autoitemize/internal/ocr"
	"receipt-autoitemize/internal/repository"
	"receipt-autoitemize/internal/repository/sqlite"
	"receipt-autoitemize/internal/service"
	"receipt-autoitemize/internal/worker"
)

// gatedEngine wraps the stub OCR and blocks every call until the gate is opened,
// standing in for a slow vendor so tests can observe queued/in-flight states.
type gatedEngine struct {
	inner ocr.StubEngine
	gate  chan struct{}
	calls atomic.Int32
}

func newGatedEngine(open bool) *gatedEngine {
	g := &gatedEngine{inner: ocr.StubEngine{FixturesDir: fixtures}, gate: make(chan struct{})}
	if open {
		close(g.gate)
	}
	return g
}

func (g *gatedEngine) Name() string { return "gated-stub" }

func (g *gatedEngine) ExtractText(ctx context.Context, in ocr.Input) (string, error) {
	g.calls.Add(1)
	select {
	case <-g.gate:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return g.inner.ExtractText(ctx, in)
}

type asyncEnv struct {
	*env
	store repository.Repository
}

func newAsyncEnv(t *testing.T, engine ocr.Engine, workers, queueSize int) *asyncEnv {
	t.Helper()
	dir := t.TempDir()
	st := openRepo(t, dir)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(st, engine, disk(t, dir), log)
	pool := worker.New(worker.Config{
		Workers: workers, QueueSize: queueSize, JobTimeout: 5 * time.Second, MaxAttempts: 2, Backoff: time.Millisecond,
	}, jobHandler(svc), log)
	pool.Start()
	svc.UseQueue(pool)
	t.Cleanup(func() { pool.Shutdown(context.Background()) }) // runs before st.Close

	srv := httptest.NewServer(api.New(svc, log, 64<<10).Handler())
	t.Cleanup(srv.Close)
	return &asyncEnv{env: &env{t: t, srv: srv}, store: st}
}

type receiptView struct {
	ID            string  `json:"receipt_id"`
	Status        string  `json:"status"`
	OCRError      *string `json:"ocr_error"`
	TransactionID *string `json:"transaction_id"`
}

func (e *env) processAsync(rid string) (*http.Response, receiptView) {
	e.t.Helper()
	res, body := e.do("POST", "/receipts/"+rid+"/process", nil, nil)
	var v receiptView
	if res.StatusCode == http.StatusAccepted {
		v = decode[receiptView](e.t, body)
	}
	return res, v
}

func (e *env) waitReceipt(rid, status string) receiptView {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body := e.do("GET", "/receipts/"+rid, nil, nil)
		v := decode[receiptView](e.t, body)
		if v.Status == status {
			return v
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("receipt %s stuck in %s, want %s", rid, v.Status, status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitCalls(t *testing.T, g *gatedEngine, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for g.calls.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("OCR calls = %d, want %d", g.calls.Load(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestAsyncProcessReturns202ThenMatchesGold(t *testing.T) {
	e := newAsyncEnv(t, newGatedEngine(true), 2, 10)
	for _, c := range []struct {
		name, merchant, status string
		total                  float64
	}{
		{"receipt-clean", "Cafe Mitte", "COMPLETE", 17.85},
		{"receipt-tax-only", "Berlin Taxi GmbH", "NEEDS_REVIEW", 24},
		{"receipt-mismatch", "Hotel Shop", "NEEDS_REVIEW", 18.5},
	} {
		t.Run(c.name, func(t *testing.T) {
			rid := e.uploadFixture(c.name, c.name+".txt")
			res, v := e.processAsync(rid)
			if res.StatusCode != http.StatusAccepted || res.Header.Get("Location") != "/receipts/"+rid {
				t.Fatalf("status %d location %q", res.StatusCode, res.Header.Get("Location"))
			}
			if v.Status != "PROCESSING" && v.Status != "PROCESSED" {
				t.Fatalf("status right after accept = %s", v.Status)
			}
			done := e.waitReceipt(rid, "PROCESSED")
			_, body := e.do("GET", "/transactions/"+*done.TransactionID, nil, nil)
			got := decode[txn](t, body)
			if got.Merchant != c.merchant || got.GrandTotal != c.total || got.Status != c.status {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestAsyncDuplicateProcessCallsRunOnce(t *testing.T) {
	g := newGatedEngine(false)
	e := newAsyncEnv(t, g, 2, 10)
	rid := e.uploadFixture("receipt-clean", "receipt-clean.txt")

	for i := 0; i < 5; i++ {
		if res, _ := e.processAsync(rid); res.StatusCode != http.StatusAccepted {
			t.Fatalf("call %d: %d", i, res.StatusCode)
		}
	}
	waitCalls(t, g, 1)
	close(g.gate)
	e.waitReceipt(rid, "PROCESSED")
	time.Sleep(20 * time.Millisecond) // give any wrongly queued duplicate a chance to run
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("OCR ran %d times for one receipt, want 1", n)
	}
}

func TestAsyncQueueFullReturns503AndRestoresStatus(t *testing.T) {
	g := newGatedEngine(false)
	e := newAsyncEnv(t, g, 1, 1)
	a := e.uploadFixture("receipt-clean", "a.txt")
	b := e.uploadFixture("receipt-tax-only", "b.txt")
	c := e.uploadFixture("receipt-mismatch", "c.txt")

	e.processAsync(a)
	waitCalls(t, g, 1) // the only worker is busy with a
	if res, _ := e.processAsync(b); res.StatusCode != http.StatusAccepted {
		t.Fatalf("b should queue: %d", res.StatusCode)
	}
	res, body := e.do("POST", "/receipts/"+c+"/process", nil, nil)
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") == "" ||
		decode[apiError](t, body).Error.Code != "QUEUE_FULL" {
		t.Fatalf("c: %d %s", res.StatusCode, body)
	}
	e.waitReceipt(c, "UPLOADED") // rejected job was rolled back, not left PROCESSING

	close(g.gate)
	e.waitReceipt(a, "PROCESSED")
	e.waitReceipt(b, "PROCESSED")
	if res, _ := e.processAsync(c); res.StatusCode != http.StatusAccepted {
		t.Fatalf("retry of c: %d", res.StatusCode)
	}
	e.waitReceipt(c, "PROCESSED")
}

func TestAsyncUnreadableReceiptEndsOCRFailed(t *testing.T) {
	g := newGatedEngine(true)
	e := newAsyncEnv(t, g, 1, 5)
	_, body := e.upload("mystery.png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...))
	rid := decode[receiptView](t, body).ID

	e.processAsync(rid)
	v := e.waitReceipt(rid, "OCR_FAILED")
	if v.OCRError == nil || v.TransactionID != nil {
		t.Fatalf("%+v", v)
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("a permanent OCR failure was retried: %d calls", n)
	}
}

// TestRecoveryAfterCrash simulates a crash: the receipt was marked PROCESSING and
// accepted, but the process died before a worker ran it. On the next start the
// recovery scan finds it in the database and processes it.
func TestRecoveryAfterCrash(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	uploads := disk(t, dir)
	data, _ := os.ReadFile(filepath.Join(fixtures, "receipt-clean.txt"))

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Before the "crash".
	svc := service.New(st, ocr.StubEngine{FixturesDir: fixtures}, uploads, log)
	up, err := svc.Upload(ctx, "receipt-clean.txt", "text/plain; charset=utf-8", data)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := st.MarkProcessing(ctx, up.ID); err != nil || !claimed {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}

	// After restart: new service and pool over the same database.
	svc2 := service.New(st, ocr.StubEngine{FixturesDir: fixtures}, uploads, log)
	pool := worker.New(worker.Config{Workers: 1, QueueSize: 1, JobTimeout: time.Second, MaxAttempts: 1}, jobHandler(svc2), log)
	pool.Start()
	defer pool.Shutdown(ctx)
	svc2.UseQueue(pool)

	pending, err := svc2.PendingJobs(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending %v, err %v", pending, err)
	}
	// A receipt claimed by a live request after the snapshot is not re-queued.
	late, _ := svc2.Upload(ctx, "receipt-clean.txt", "text/plain; charset=utf-8", data)
	st.MarkProcessing(ctx, late.ID)
	n, err := svc2.Requeue(ctx, pending)
	if err != nil || n != 1 {
		t.Fatalf("recovered %d, err %v", n, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, _ := st.GetReceipt(ctx, up.ID)
		if r.Status == domain.ReceiptProcessed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("receipt still %s after recovery", r.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok, _ := st.TransactionIDForReceipt(ctx, up.ID); !ok {
		t.Fatal("no transaction after recovery")
	}
}
