// Package repotest is the contract every repository.Repository implementation must
// pass. Each backend's test file calls Run with a constructor; a new database is
// "pluggable" once it passes this suite.
package repotest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"receipt-autoitemize/internal/domain"
	"receipt-autoitemize/internal/repository"
)

// Factory returns a fresh, empty repository for one test.
type Factory func(t *testing.T) repository.Repository

// Run executes the whole contract against the repository the factory builds.
func Run(t *testing.T, newRepo Factory) {
	tests := []struct {
		name string
		fn   func(*testing.T, repository.Repository)
	}{
		{"ReceiptRoundTrip", testReceiptRoundTrip},
		{"FindDuplicate", testFindDuplicate},
		{"MarkProcessingClaimsOnce", testMarkProcessing},
		{"PendingReceiptsOldestFirst", testPendingReceipts},
		{"SubSecondOrdering", testSubSecondOrdering},
		{"MarkReceiptFailed", testMarkFailed},
		{"SaveProcessedCreates", testSaveProcessedCreates},
		{"SaveProcessedUpdatesInPlace", testSaveProcessedUpdates},
		{"SaveProcessedUnknownReceipt", testSaveProcessedUnknownReceipt},
		{"ReplaceItemsOptimisticLock", testReplaceItems},
		{"ReturnedValuesAreIsolated", testIsolation},
		{"ConcurrentSaveKeepsOneTransaction", testConcurrentSave},
		{"ConcurrentClaimHasOneWinner", testConcurrentClaim},
		{"TransactionIDCollisionIsRejected", testTxnIDCollision},
		{"ReadDuringWriteIsConsistent", testReadDuringWrite},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepo(t)
			t.Cleanup(func() { repo.Close() })
			tc.fn(t, repo)
		})
	}
}

var (
	ctx = context.Background()
	t0  = time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)
)

func receipt(id string, created time.Time, sha string) domain.Receipt {
	return domain.Receipt{
		ID: id, Filename: id + ".txt", ContentType: "text/plain", SizeBytes: 10,
		SHA256: hash(sha), StoragePath: "/tmp/" + id, Status: domain.ReceiptUploaded, CreatedAt: created,
	}
}

func money(v domain.Money) *domain.Money { return &v }
func str(v string) *string               { return &v }

func mustCreate(t *testing.T, r repository.Repository, rc domain.Receipt) {
	t.Helper()
	if err := r.CreateReceipt(ctx, rc); err != nil {
		t.Fatal(err)
	}
}

func sampleTxn(id, receiptID string, at time.Time) domain.Transaction {
	rate := domain.Rate{Percent: domain.NewDecimal(19123, 3)} // 19.123%: more precision than basis points
	return domain.Transaction{
		ID: id, ReceiptID: receiptID,
		Merchant: str("Cafe Mitte"), Date: str("2026-03-12"), Currency: str("EUR"),
		Subtotal: money(1500), GrandTotal: money(1785),
		Taxes: []domain.TaxLine{{ID: id + "_tax", Name: "VAT", Rate: &rate, Amount: 285}},
		LineItems: []domain.LineItem{
			{ID: id + "_li1", Description: "Espresso", Amount: 350, Source: domain.SourceAuto},
			{ID: id + "_li2", Description: "Sandwich", Amount: 890, Source: domain.SourceAuto},
			{ID: id + "_li3", Description: "Mineral water", Amount: 260, Source: domain.SourceAuto},
		},
		ItemizeStatus: domain.ItemizeComplete, ItemizeIssues: []domain.Issue{},
		CreatedAt: at, UpdatedAt: at,
	}
}

func ocrRun(id, receiptID string, at time.Time) domain.OCRResult {
	return domain.OCRResult{ID: id, ReceiptID: receiptID, Engine: "test", Text: "TOTAL  17.85 " + id, CreatedAt: at}
}

// sameJSON compares values the way API clients see them.
func sameJSON(t *testing.T, label string, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("%s:\n got  %s\n want %s", label, g, w)
	}
}

// ---------------------------------------------------------------- receipts

func testReceiptRoundTrip(t *testing.T, r repository.Repository) {
	rc := receipt("r1", t0, "abc")
	mustCreate(t, r, rc)
	got, err := r.GetReceipt(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, "receipt", got, rc)
	if got.StoragePath != rc.StoragePath {
		t.Errorf("storage path = %q", got.StoragePath)
	}
	if _, err := r.GetReceipt(ctx, "missing"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("missing receipt: %v", err)
	}
}

func testFindDuplicate(t *testing.T, r repository.Repository) {
	mustCreate(t, r, receipt("new", t0.Add(2*time.Minute), "same"))
	mustCreate(t, r, receipt("old", t0, "same"))
	mustCreate(t, r, receipt("other", t0, "different"))
	if id, ok, err := r.FindDuplicate(ctx, hash("same"), "new"); err != nil || !ok || id != "old" {
		t.Errorf("got %q %v %v, want oldest other receipt", id, ok, err)
	}
	if _, ok, _ := r.FindDuplicate(ctx, hash("different"), "other"); ok {
		t.Error("a receipt must not be its own duplicate")
	}
}

func testMarkProcessing(t *testing.T, r repository.Repository) {
	mustCreate(t, r, receipt("r1", t0, "a"))
	prev, claimed, err := r.MarkProcessing(ctx, "r1")
	if err != nil || !claimed || prev != domain.ReceiptUploaded {
		t.Fatalf("first claim: %s %v %v", prev, claimed, err)
	}
	if prev, claimed, _ := r.MarkProcessing(ctx, "r1"); claimed || prev != domain.ReceiptProcessing {
		t.Fatalf("second claim should be refused: %s %v", prev, claimed)
	}
	if err := r.RestoreStatus(ctx, "r1", domain.ReceiptUploaded); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.GetReceipt(ctx, "r1"); got.Status != domain.ReceiptUploaded {
		t.Fatalf("after restore: %s", got.Status)
	}
	// RestoreStatus only undoes a claim; it never overwrites another state.
	r.MarkReceiptFailed(ctx, "r1", "boom", t0)
	r.RestoreStatus(ctx, "r1", domain.ReceiptUploaded)
	if got, _ := r.GetReceipt(ctx, "r1"); got.Status != domain.ReceiptFailed {
		t.Fatalf("restore overwrote a non-PROCESSING status: %s", got.Status)
	}
	if _, _, err := r.MarkProcessing(ctx, "missing"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func testPendingReceipts(t *testing.T, r repository.Repository) {
	for i, id := range []string{"c", "a", "b", "idle"} {
		mustCreate(t, r, receipt(id, t0.Add(time.Duration(3-i)*time.Minute), id))
	}
	for _, id := range []string{"a", "b", "c"} {
		r.MarkProcessing(ctx, id)
	}
	ids, err := r.PendingReceipts(ctx)
	if err != nil || fmt.Sprint(ids) != "[b a c]" {
		t.Fatalf("pending = %v %v, want oldest first [b a c]", ids, err)
	}
}

// Timestamps within the same second must still order correctly. A text column
// holding RFC3339Nano gets this wrong ("09:00:00Z" sorts after "09:00:00.1Z").
func testSubSecondOrdering(t *testing.T, r repository.Repository) {
	mustCreate(t, r, receipt("later", t0.Add(100*time.Millisecond), "same"))
	mustCreate(t, r, receipt("first", t0, "same"))
	mustCreate(t, r, receipt("probe", t0.Add(time.Second), "same"))
	for _, id := range []string{"later", "first"} {
		r.MarkProcessing(ctx, id)
	}
	if ids, err := r.PendingReceipts(ctx); err != nil || fmt.Sprint(ids) != "[first later]" {
		t.Errorf("pending = %v %v, want [first later]", ids, err)
	}
	if id, _, err := r.FindDuplicate(ctx, hash("same"), "probe"); err != nil || id != "first" {
		t.Errorf("duplicate = %q %v, want the oldest (first)", id, err)
	}

	r.SaveProcessed(ctx, ocrRun("o_old", "first", t0), sampleTxn("t1", "first", t0))
	r.SaveProcessed(ctx, ocrRun("o_new", "first", t0.Add(500*time.Millisecond)), sampleTxn("t1", "first", t0))
	if o, err := r.LatestOCR(ctx, "first"); err != nil || o.ID != "o_new" {
		t.Errorf("latest OCR = %q %v, want o_new", o.ID, err)
	}
}

func testMarkFailed(t *testing.T, r repository.Repository) {
	mustCreate(t, r, receipt("r1", t0, "a"))
	if err := r.MarkReceiptFailed(ctx, "r1", "no text", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetReceipt(ctx, "r1")
	if got.Status != domain.ReceiptFailed || got.OCRError == nil || *got.OCRError != "no text" ||
		got.ProcessedAt == nil || !got.ProcessedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("%+v", got)
	}
}

// ---------------------------------------------------------------- transactions

func testSaveProcessedCreates(t *testing.T, r repository.Repository) {
	mustCreate(t, r, receipt("r1", t0, "a"))
	r.MarkProcessing(ctx, "r1")
	want := sampleTxn("t1", "r1", t0)
	id, err := r.SaveProcessed(ctx, ocrRun("o1", "r1", t0), want)
	if err != nil || id != "t1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	got, err := r.GetTransaction(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	want.OCRResultID, want.Version = "o1", 1
	sameJSON(t, "transaction", got, want)

	rc, _ := r.GetReceipt(ctx, "r1")
	if rc.Status != domain.ReceiptProcessed || rc.OCRError != nil || rc.ProcessedAt == nil {
		t.Errorf("receipt after save: %+v", rc)
	}
	if tid, ok, _ := r.TransactionIDForReceipt(ctx, "r1"); !ok || tid != "t1" {
		t.Errorf("TransactionIDForReceipt = %q %v", tid, ok)
	}
	if o, err := r.OCRByID(ctx, "o1"); err != nil || o.Text != "TOTAL  17.85 o1" {
		t.Errorf("OCRByID: %+v %v", o, err)
	}
	if _, err := r.GetTransaction(ctx, "missing"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("missing txn: %v", err)
	}
	if _, err := r.OCRByID(ctx, "missing"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("missing ocr: %v", err)
	}
	if _, err := r.LatestOCR(ctx, "missing"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("missing latest ocr: %v", err)
	}
}

func testSaveProcessedUpdates(t *testing.T, r repository.Repository) {
	mustCreate(t, r, receipt("r1", t0, "a"))
	r.SaveProcessed(ctx, ocrRun("o1", "r1", t0), sampleTxn("t1", "r1", t0))

	later := t0.Add(time.Hour)
	second := sampleTxn("ignored", "r1", later)
	second.Merchant = str("Cafe Mitte GmbH")
	second.LineItems = second.LineItems[:1]
	second.Taxes = nil
	id, err := r.SaveProcessed(ctx, ocrRun("o2", "r1", later), second)
	if err != nil || id != "t1" {
		t.Fatalf("re-process must keep the transaction id: %q %v", id, err)
	}
	got, _ := r.GetTransaction(ctx, "t1")
	if got.Version != 2 || !got.CreatedAt.Equal(t0) || !got.UpdatedAt.Equal(later) ||
		*got.Merchant != "Cafe Mitte GmbH" || len(got.LineItems) != 1 || len(got.Taxes) != 0 || got.OCRResultID != "o2" {
		t.Fatalf("%+v", got)
	}
	if o, _ := r.LatestOCR(ctx, "r1"); o.ID != "o2" {
		t.Errorf("latest OCR = %s, want o2", o.ID)
	}
	if o, err := r.OCRByID(ctx, "o1"); err != nil || o.ID != "o1" {
		t.Errorf("old OCR run must be kept: %v", err)
	}
}

func testSaveProcessedUnknownReceipt(t *testing.T, r repository.Repository) {
	_, err := r.SaveProcessed(ctx, ocrRun("o1", "nope", t0), sampleTxn("t1", "nope", t0))
	if !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, ok, _ := r.TransactionIDForReceipt(ctx, "nope"); ok {
		t.Fatal("a failed save left a transaction behind")
	}
}

func testReplaceItems(t *testing.T, r repository.Repository) {
	mustCreate(t, r, receipt("r1", t0, "a"))
	r.SaveProcessed(ctx, ocrRun("o1", "r1", t0), sampleTxn("t1", "r1", t0))
	before, _ := r.GetTransaction(ctx, "t1")

	items := []domain.LineItem{{ID: "u1", Description: "Everything", Amount: 1500, Source: domain.SourceUser, TaxAmount: money(285)}}
	issues := []domain.Issue{{Code: "X", Message: "m", Difference: money(1)}}
	at := t0.Add(time.Minute)
	if err := r.ReplaceItems(ctx, "t1", 1, items, domain.ItemizeNeedsReview, issues, at); err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetTransaction(ctx, "t1")
	sameJSON(t, "items", got.LineItems, items)
	sameJSON(t, "issues", got.ItemizeIssues, issues)
	sameJSON(t, "taxes untouched", got.Taxes, before.Taxes)
	if got.Version != 2 || got.ItemizeStatus != domain.ItemizeNeedsReview || *got.GrandTotal != 1785 || !got.UpdatedAt.Equal(at) {
		t.Fatalf("%+v", got)
	}

	if err := r.ReplaceItems(ctx, "t1", 1, nil, domain.ItemizeComplete, nil, at); !errors.Is(err, repository.ErrVersionConflict) {
		t.Fatalf("stale version: %v", err)
	}
	if after, _ := r.GetTransaction(ctx, "t1"); after.Version != 2 || len(after.LineItems) != 1 {
		t.Fatal("a rejected write changed the transaction")
	}
	if err := r.ReplaceItems(ctx, "missing", 1, nil, domain.ItemizeComplete, nil, at); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func testIsolation(t *testing.T, r repository.Repository) {
	mustCreate(t, r, receipt("r1", t0, "a"))
	in := sampleTxn("t1", "r1", t0)
	r.SaveProcessed(ctx, ocrRun("o1", "r1", t0), in)
	in.LineItems[0].Description = "mutated input"
	*in.Merchant = "mutated input"

	got, _ := r.GetTransaction(ctx, "t1")
	got.LineItems[0].Description = "mutated output"
	*got.GrandTotal = 1

	again, _ := r.GetTransaction(ctx, "t1")
	if again.LineItems[0].Description != "Espresso" || *again.Merchant != "Cafe Mitte" || *again.GrandTotal != 1785 {
		t.Fatalf("stored data shares memory with callers: %+v", again)
	}
}

func testConcurrentSave(t *testing.T, r repository.Repository) {
	mustCreate(t, r, receipt("r1", t0, "a"))
	const n = 10
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := r.SaveProcessed(ctx, ocrRun(fmt.Sprintf("o%d", i), "r1", t0), sampleTxn(fmt.Sprintf("t%d", i), "r1", t0))
			if err != nil {
				t.Error(err)
			}
			ids[i] = id
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("several transactions for one receipt: %v", ids)
		}
	}
	if got, _ := r.GetTransaction(ctx, ids[0]); got.Version != n {
		t.Fatalf("version = %d, want %d (one bump per save)", got.Version, n)
	}
}

func testConcurrentClaim(t *testing.T, r repository.Repository) {
	mustCreate(t, r, receipt("r1", t0, "a"))
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, claimed, err := r.MarkProcessing(ctx, "r1"); err == nil && claimed {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d goroutines claimed the same receipt, want 1", winners)
	}
}

// A transaction is read as one aggregate: the version (the ETag) always matches the
// items returned, even while another goroutine keeps replacing them.
func testReadDuringWrite(t *testing.T, r repository.Repository) {
	mustCreate(t, r, receipt("r1", t0, "a"))
	r.SaveProcessed(ctx, ocrRun("o1", "r1", t0), sampleTxn("t1", "r1", t0))
	// Each write stores one item whose description is the version it produces.
	itemsFor := func(v int) []domain.LineItem {
		return []domain.LineItem{{ID: "li" + strconv.Itoa(v), Description: strconv.Itoa(v), Amount: 1785, Source: domain.SourceUser}}
	}
	if err := r.ReplaceItems(ctx, "t1", 1, itemsFor(2), domain.ItemizeComplete, nil, t0); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for v := 2; v < 200; v++ {
			if err := r.ReplaceItems(ctx, "t1", v, itemsFor(v+1), domain.ItemizeComplete, nil, t0); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		got, err := r.GetTransaction(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.LineItems) != 1 || got.LineItems[0].Description != strconv.Itoa(got.Version) {
			<-done
			t.Fatalf("torn read: version %d with items %+v", got.Version, got.LineItems)
		}
	}
}

// A transaction ID already used by another receipt is refused, and nothing changes
// (SQLite enforces this with the primary key; memory must behave the same).
func testTxnIDCollision(t *testing.T, r repository.Repository) {
	mustCreate(t, r, receipt("r1", t0, "a"))
	mustCreate(t, r, receipt("r2", t0, "b"))
	if _, err := r.SaveProcessed(ctx, ocrRun("o1", "r1", t0), sampleTxn("tX", "r1", t0)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveProcessed(ctx, ocrRun("o2", "r2", t0), sampleTxn("tX", "r2", t0)); err == nil {
		t.Fatal("a second receipt was saved under an existing transaction id")
	}
	if got, err := r.GetTransaction(ctx, "tX"); err != nil || got.ReceiptID != "r1" {
		t.Fatalf("existing transaction damaged: %+v %v", got, err)
	}
	if _, ok, _ := r.TransactionIDForReceipt(ctx, "r2"); ok {
		t.Fatal("the rejected save left a transaction for r2")
	}
	if got, _ := r.GetReceipt(ctx, "r2"); got.Status != domain.ReceiptUploaded {
		t.Fatalf("the rejected save changed r2's status to %s", got.Status)
	}
}

// hash pads a short label to the 64 hex characters of a real SHA-256.
func hash(label string) string { return strings.Repeat("0", 64-len(label)) + label }
