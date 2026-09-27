package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"receipt-autoitemize/internal/domain"
	"receipt-autoitemize/internal/itemize"
	"receipt-autoitemize/internal/ocr"
	"receipt-autoitemize/internal/repository"
	"receipt-autoitemize/internal/repository/memory"
)

// ---------------------------------------------------------------- fakes

// memFiles is an in-memory FileStore.
type memFiles struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newMemFiles() *memFiles { return &memFiles{files: map[string][]byte{}} }

func (m *memFiles) Put(_ context.Context, name string, data []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[name] = data
	return name, nil
}

func (m *memFiles) Get(_ context.Context, ref string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[ref]
	if !ok {
		return nil, fmt.Errorf("open %s: %w", ref, fs.ErrNotExist)
	}
	return b, nil
}

func (m *memFiles) Delete(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, ref)
	return nil
}

func (m *memFiles) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.files)
}

// engineFunc is an ocr.Engine made from a function.
type engineFunc func(ctx context.Context, in ocr.Input) (string, error)

func (engineFunc) Name() string { return "fake" }
func (f engineFunc) ExtractText(ctx context.Context, in ocr.Input) (string, error) {
	return f(ctx, in)
}

func textEngine(text string) engineFunc {
	return func(context.Context, ocr.Input) (string, error) { return text, nil }
}

func failingEngine(err error) engineFunc {
	return func(context.Context, ocr.Input) (string, error) { return "", err }
}

// faultyRepo is the memory repository with injectable write failures.
type faultyRepo struct {
	repository.Repository
	createErr, replaceErr, dupErr error
}

func (r *faultyRepo) FindDuplicate(ctx context.Context, sha, exclude string) (string, bool, error) {
	if r.dupErr != nil {
		return "", false, r.dupErr
	}
	return r.Repository.FindDuplicate(ctx, sha, exclude)
}

func (r *faultyRepo) CreateReceipt(ctx context.Context, rc domain.Receipt) error {
	if r.createErr != nil {
		return r.createErr
	}
	return r.Repository.CreateReceipt(ctx, rc)
}

func (r *faultyRepo) ReplaceItems(ctx context.Context, id string, v int, items []domain.LineItem,
	st domain.ItemizeStatus, issues []domain.Issue, at time.Time) error {
	if r.replaceErr != nil {
		return r.replaceErr
	}
	return r.Repository.ReplaceItems(ctx, id, v, items, st, issues, at)
}

// fakeQueue records submissions; full makes TrySubmit refuse.
type fakeQueue struct {
	full      bool
	submitted []string
}

func (q *fakeQueue) TrySubmit(id string) bool {
	if q.full {
		return false
	}
	q.submitted = append(q.submitted, id)
	return true
}

func (q *fakeQueue) Submit(_ context.Context, id string) error {
	q.submitted = append(q.submitted, id)
	return nil
}

// ---------------------------------------------------------------- helpers

var ctx = context.Background()

func cleanText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../fixtures/task-a/receipt-clean.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type fixture struct {
	svc   *Service
	repo  *faultyRepo
	files *memFiles
}

func newFixture(engine ocr.Engine) *fixture {
	f := &fixture{repo: &faultyRepo{Repository: memory.New()}, files: newMemFiles()}
	f.svc = New(f.repo, engine, f.files, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return f
}

func (f *fixture) upload(t *testing.T, text string) string {
	t.Helper()
	res, err := f.svc.Upload(ctx, "r.txt", "text/plain; charset=utf-8", []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return res.ID
}

func (f *fixture) status(t *testing.T, id string) domain.ReceiptStatus {
	t.Helper()
	r, err := f.repo.GetReceipt(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return r.Status
}

// ---------------------------------------------------------------- upload

func TestUpload(t *testing.T) {
	f := newFixture(textEngine(""))
	if _, err := f.svc.Upload(ctx, "a.txt", "text/plain", nil); !errors.Is(err, ErrEmptyFile) {
		t.Errorf("empty: %v", err)
	}
	if _, err := f.svc.Upload(ctx, "a.exe", "application/octet-stream", []byte("MZ")); !errors.Is(err, ErrUnsupportedMedia) {
		t.Errorf("unsupported: %v", err)
	}

	first, err := f.svc.Upload(ctx, "../../a.TXT", "text/plain; charset=utf-8", []byte("TOTAL 1.00"))
	if err != nil || first.Filename != "a.TXT" || first.DuplicateOf != nil || f.files.count() != 1 {
		t.Fatalf("first: %+v %v", first, err)
	}
	if b, _ := f.files.Get(ctx, first.StoragePath); string(b) != "TOTAL 1.00" {
		t.Errorf("stored bytes = %q", b)
	}
	second, _ := f.svc.Upload(ctx, "b.txt", "text/plain", []byte("TOTAL 1.00"))
	if second.DuplicateOf == nil || *second.DuplicateOf != first.ID {
		t.Errorf("same bytes must be flagged as a duplicate of %s: %+v", first.ID, second.DuplicateOf)
	}
}

func TestUploadRemovesFileWhenReceiptCannotBeSaved(t *testing.T) {
	f := newFixture(textEngine(""))
	f.repo.createErr = errors.New("disk full")
	if _, err := f.svc.Upload(ctx, "a.txt", "text/plain", []byte("x")); err == nil {
		t.Fatal("want error")
	}
	if n := f.files.count(); n != 0 {
		t.Fatalf("%d orphan file(s) left behind", n)
	}
}

// The receipt is already saved when the duplicate check runs; failing the upload
// would make the client retry and create a real duplicate.
func TestUploadSurvivesDuplicateCheckFailure(t *testing.T) {
	f := newFixture(textEngine(""))
	f.repo.dupErr = errors.New("db hiccup")
	res, err := f.svc.Upload(ctx, "a.txt", "text/plain", []byte("x"))
	if err != nil || res.ID == "" || res.DuplicateOf != nil {
		t.Fatalf("%+v %v", res, err)
	}
}

// ---------------------------------------------------------------- process

func TestProcessCreatesTransaction(t *testing.T) {
	f := newFixture(textEngine(cleanText(t)))
	id := f.upload(t, "ignored by the fake engine")
	txn, err := f.svc.Process(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if txn.ItemizeStatus != domain.ItemizeComplete || len(txn.LineItems) != 3 || txn.Version != 1 {
		t.Fatalf("%+v", txn)
	}
	if f.status(t, id) != domain.ReceiptProcessed {
		t.Fatalf("receipt status %s", f.status(t, id))
	}
}

// Permanent failures mark the receipt OCR_FAILED; transient ones leave it untouched
// so the client can retry. The classification is shared with the async worker.
func TestProcessClassifiesOCRFailures(t *testing.T) {
	cases := []struct {
		name       string
		engineErr  error
		deleteFile bool
		want       error
		status     domain.ReceiptStatus
	}{
		{"no text is permanent", fmt.Errorf("%w: blank page", ocr.ErrNoText), false, ErrOCRFailed, domain.ReceiptFailed},
		{"missing upload is permanent", nil, true, ErrOCRFailed, domain.ReceiptFailed},
		{"timeout is transient", context.DeadlineExceeded, false, ErrOCRUnavailable, domain.ReceiptUploaded},
		{"vendor error is transient", errors.New("vendor 502"), false, ErrOCRUnavailable, domain.ReceiptUploaded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(failingEngine(tc.engineErr))
			id := f.upload(t, "x")
			if tc.deleteFile {
				r, _ := f.repo.GetReceipt(ctx, id)
				f.files.Delete(ctx, r.StoragePath)
			}
			_, err := f.svc.Process(ctx, id)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got := f.status(t, id); got != tc.status {
				t.Fatalf("status = %s, want %s", got, tc.status)
			}
			// ocr_error is shown to clients: it must never carry internal error text.
			if r, _ := f.repo.GetReceipt(ctx, id); r.OCRError != nil && strings.Contains(*r.OCRError, "read upload") {
				t.Fatalf("internal error text stored in ocr_error: %q", *r.OCRError)
			}
		})
	}
}

func TestProcessUnknownReceipt(t *testing.T) {
	f := newFixture(textEngine("x"))
	if _, err := f.svc.Process(ctx, "rcpt_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

// ---------------------------------------------------------------- async

func TestProcessAsyncQueueFullRestoresStatus(t *testing.T) {
	f := newFixture(textEngine(""))
	q := &fakeQueue{full: true}
	f.svc.UseQueue(q)
	id := f.upload(t, "x")
	if _, err := f.svc.ProcessAsync(ctx, id); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("%v", err)
	}
	if got := f.status(t, id); got != domain.ReceiptUploaded {
		t.Fatalf("status = %s, want UPLOADED so the client can retry", got)
	}
}

func TestProcessAsyncQueuesOnce(t *testing.T) {
	f := newFixture(textEngine(""))
	q := &fakeQueue{}
	f.svc.UseQueue(q)
	id := f.upload(t, "x")
	for i := 0; i < 3; i++ {
		v, err := f.svc.ProcessAsync(ctx, id)
		if err != nil || v.Status != domain.ReceiptProcessing {
			t.Fatalf("call %d: %+v %v", i, v, err)
		}
	}
	if len(q.submitted) != 1 {
		t.Fatalf("queued %d times, want 1", len(q.submitted))
	}
}

// A rejected claim (queue full) must restore the receipt exactly, ocr_error included.
func TestQueueFullKeepsPreviousFailure(t *testing.T) {
	f := newFixture(textEngine(""))
	f.svc.UseQueue(&fakeQueue{full: true})
	id := f.upload(t, "x")
	f.svc.FailJob(ctx, id, &ocrError{err: ocr.ErrNoText})
	before, _ := f.repo.GetReceipt(ctx, id)
	if _, err := f.svc.ProcessAsync(ctx, id); !errors.Is(err, ErrQueueFull) {
		t.Fatal(err)
	}
	after, _ := f.repo.GetReceipt(ctx, id)
	if after.Status != domain.ReceiptFailed || after.OCRError == nil || *after.OCRError != *before.OCRError {
		t.Fatalf("before %+v\nafter  %+v", before, after)
	}
}

func TestPendingJobsAndRequeue(t *testing.T) {
	f := newFixture(textEngine(""))
	if ids, err := f.svc.PendingJobs(ctx); ids != nil || err != nil {
		t.Fatalf("sync mode must not recover anything: %v %v", ids, err)
	}
	q := &fakeQueue{}
	f.svc.UseQueue(q)
	id := f.upload(t, "x")
	f.repo.MarkProcessing(ctx, id)
	ids, _ := f.svc.PendingJobs(ctx)
	if n, err := f.svc.Requeue(ctx, ids); n != 1 || err != nil || fmt.Sprint(q.submitted) != "["+id+"]" {
		t.Fatalf("%d %v %v", n, err, q.submitted)
	}
}

func TestFailJobMarksReceipt(t *testing.T) {
	f := newFixture(textEngine(""))
	id := f.upload(t, "x")
	if err := f.svc.FailJob(ctx, id, errors.New("gave up")); err != nil {
		t.Fatal(err)
	}
	if got := f.status(t, id); got != domain.ReceiptFailed {
		t.Fatalf("status = %s", got)
	}
}

// ---------------------------------------------------------------- edits

func processed(t *testing.T, text string) (*fixture, domain.Transaction) {
	t.Helper()
	f := newFixture(textEngine(text))
	txn, err := f.svc.Process(ctx, f.upload(t, "x"))
	if err != nil {
		t.Fatal(err)
	}
	return f, txn
}

func ptr[T any](v T) *T { return &v }

func TestWriteConflicts(t *testing.T) {
	f, txn := processed(t, cleanText(t))
	rename := []itemize.Operation{{Op: "update", ItemID: txn.LineItems[0].ID, Description: ptr("Latte")}}

	f.repo.replaceErr = repository.ErrVersionConflict
	if _, err := f.svc.PatchItems(ctx, txn.ID, nil, rename); !errors.Is(err, ErrConcurrentUpdate) {
		t.Errorf("lost race without If-Match: %v, want ErrConcurrentUpdate (409)", err)
	}
	if _, err := f.svc.PatchItems(ctx, txn.ID, ptr(txn.Version), rename); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("lost race with If-Match: %v, want ErrPreconditionFailed (412)", err)
	}
	f.repo.replaceErr = nil

	if _, err := f.svc.PatchItems(ctx, txn.ID, ptr(txn.Version+1), rename); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("stale If-Match: %v", err)
	}
	if _, err := f.svc.Reitemize(ctx, txn.ID, ptr(txn.Version+1)); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("re-itemize with stale If-Match: %v", err)
	}
	if got, err := f.svc.Reitemize(ctx, txn.ID, ptr(txn.Version)); err != nil || got.Version != txn.Version+1 {
		t.Errorf("re-itemize with If-Match: %v %v", got.Version, err)
	}
	if got, err := f.svc.Reitemize(ctx, txn.ID, nil); err != nil || got.Version != txn.Version+2 {
		t.Errorf("re-itemize without If-Match: %v %v", got.Version, err)
	}
}

func TestPatchItemsRules(t *testing.T) {
	f, txn := processed(t, cleanText(t))
	var deleteAll []itemize.Operation
	for _, it := range txn.LineItems {
		deleteAll = append(deleteAll, itemize.Operation{Op: "delete", ItemID: it.ID})
	}
	if _, err := f.svc.PatchItems(ctx, txn.ID, nil, deleteAll); !errors.Is(err, ErrNoItems) {
		t.Errorf("delete all: %v", err)
	}
	breakTotal := []itemize.Operation{{Op: "update", ItemID: txn.LineItems[0].ID, Amount: ptr(domain.Money(500))}}
	var recErr *ReconcileError
	if _, err := f.svc.PatchItems(ctx, txn.ID, nil, breakTotal); !errors.As(err, &recErr) {
		t.Errorf("breaking the total: %v", err)
	}
	if after, _ := f.svc.GetTransaction(ctx, txn.ID); after.Version != txn.Version {
		t.Error("a rejected PATCH wrote something")
	}

	g, noTotal := processed(t, "Coffee 3.00\n")
	add := []itemize.Operation{{Op: "add", Description: ptr("Coffee"), Amount: ptr(domain.Money(300))}}
	if _, err := g.svc.PatchItems(ctx, noTotal.ID, nil, add); !errors.Is(err, ErrNoTotal) {
		t.Errorf("no total: %v (status %s)", err, noTotal.ItemizeStatus)
	}
}

// ---------------------------------------------------------------- classification

func TestRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{context.DeadlineExceeded, true},
		{errors.New("vendor 502"), true},
		{fmt.Errorf("wrapped: %w", ocr.ErrNoText), false},
		{&ocrError{err: ocr.ErrNoText}, false},
		{fmt.Errorf("read upload: %w", fs.ErrNotExist), false},
		{ErrNotFound, false},
		{repository.ErrNotFound, false},
	}
	for _, c := range cases {
		if got := Retryable(c.err); got != c.want {
			t.Errorf("Retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestStoreErrorMapping(t *testing.T) {
	v := 1
	cases := []struct {
		err     error
		ifMatch *int
		want    error
	}{
		{repository.ErrNotFound, nil, ErrNotFound},
		{repository.ErrVersionConflict, nil, ErrConcurrentUpdate},
		{repository.ErrVersionConflict, &v, ErrPreconditionFailed},
		{repository.ErrNotFound, &v, ErrNotFound},
	}
	for _, c := range cases {
		if got := writeErr(c.err, c.ifMatch); !errors.Is(got, c.want) {
			t.Errorf("writeErr(%v, %v) = %v, want %v", c.err, c.ifMatch, got, c.want)
		}
	}
	other := errors.New("boom")
	if got := mapStoreErr(other); got != other {
		t.Errorf("unknown errors must pass through: %v", got)
	}
	if !errors.Is(&ocrError{err: ocr.ErrNoText}, ErrOCRFailed) || !errors.Is(&ocrError{err: ocr.ErrNoText}, ocr.ErrNoText) {
		t.Error("ocrError must match both ErrOCRFailed and the engine's error")
	}
}

func TestFailureReasonNeverLeaksInternals(t *testing.T) {
	secret := "/srv/data/uploads/rcpt_1.pdf"
	cases := []struct {
		err  error
		want string
	}{
		{&ocrError{err: fmt.Errorf("%w: blank page", ocr.ErrNoText)}, "blank page"},
		{&ocrError{err: fmt.Errorf("read upload: open %s: %w", secret, fs.ErrNotExist)}, "no longer available"},
		{&ocrError{err: fmt.Errorf("vendor said 400 for %s", secret)}, "could not read"},
		{fmt.Errorf("save %s: %w", secret, context.DeadlineExceeded), "did not complete"},
		{fmt.Errorf("%s: %w", secret, ErrNotFound), "processing failed"},
	}
	for _, c := range cases {
		got := FailureReason(c.err)
		if !strings.Contains(got, c.want) || strings.Contains(got, secret) {
			t.Errorf("FailureReason(%v) = %q, want it to mention %q and hide the path", c.err, got, c.want)
		}
	}
}
