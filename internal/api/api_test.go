package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"receipt-autoitemize/internal/api"
	"receipt-autoitemize/internal/ocr"
	"receipt-autoitemize/internal/repository"
	"receipt-autoitemize/internal/repository/memory"
	"receipt-autoitemize/internal/repository/sqlite"
	"receipt-autoitemize/internal/service"
	"receipt-autoitemize/internal/storage"
	"receipt-autoitemize/internal/worker"
)

const fixtures = "../../fixtures/task-a"

type txn struct {
	ID         string  `json:"id"`
	ReceiptID  string  `json:"receipt_id"`
	Merchant   string  `json:"merchant"`
	Date       string  `json:"date"`
	Currency   string  `json:"currency"`
	GrandTotal float64 `json:"grand_total"`
	Version    int     `json:"version"`
	Status     string  `json:"itemize_status"`
	Issues     []issue `json:"itemize_issues"`
	Taxes      []tax   `json:"taxes"`
	Items      []item  `json:"line_items"`
}

type tax struct {
	Name      string  `json:"name"`
	Rate      float64 `json:"rate"`
	Amount    float64 `json:"amount"`
	Inclusive bool    `json:"inclusive"`
}

type item struct {
	ID          string  `json:"id"`
	Description string  `json:"description"`
	Amount      float64 `json:"amount"`
	Source      string  `json:"source"`
}

type issue struct {
	Code          string  `json:"code"`
	ComputedTotal float64 `json:"computed_total"`
	Difference    float64 `json:"difference"`
}

type apiError struct {
	Error struct {
		Code    string          `json:"code"`
		Details json.RawMessage `json:"details"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

type env struct {
	t   *testing.T
	srv *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWithEngine(t, ocr.StubEngine{FixturesDir: fixtures})
}

func newEnvWithEngine(t *testing.T, engine ocr.Engine) *env {
	t.Helper()
	dir := t.TempDir()
	st := openRepo(t, dir)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(st, engine, disk(t, dir), log)
	srv := httptest.NewServer(api.New(svc, log, 64<<10).Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv}
}

func disk(t *testing.T, dir string) *storage.Disk {
	t.Helper()
	d, err := storage.NewDisk(filepath.Join(dir, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// jobHandler mirrors the adapter in cmd/server.
func jobHandler(svc *service.Service) worker.Handler {
	return worker.Handler{Run: svc.RunJob, Retryable: service.Retryable, Fail: svc.FailJob}
}

// openRepo builds the backend under test. The whole HTTP suite runs against SQLite by
// default and against the in-memory repository with TEST_STORE=memory (see Makefile),
// which shows the service does not depend on any particular database.
func openRepo(t *testing.T, dir string) repository.Repository {
	t.Helper()
	var repo repository.Repository
	if os.Getenv("TEST_STORE") == "memory" {
		repo = memory.New()
	} else {
		st, err := sqlite.Open(context.Background(), filepath.Join(dir, "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		repo = st
	}
	t.Cleanup(func() { repo.Close() })
	return repo
}

func (e *env) do(method, path string, body io.Reader, headers map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func (e *env) upload(filename string, data []byte) (*http.Response, []byte) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write(data)
	mw.Close()
	return e.do("POST", "/receipts", &buf, map[string]string{"Content-Type": mw.FormDataContentType()})
}

func (e *env) uploadFixture(name, asFilename string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtures, name+".txt"))
	if err != nil {
		e.t.Fatal(err)
	}
	res, body := e.upload(asFilename, data)
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("upload: %d %s", res.StatusCode, body)
	}
	var r struct {
		ReceiptID string `json:"receipt_id"`
	}
	json.Unmarshal(body, &r)
	return r.ReceiptID
}

func (e *env) process(name string) txn {
	e.t.Helper()
	rid := e.uploadFixture(name, name+".txt")
	res, body := e.do("POST", "/receipts/"+rid+"/process", nil, nil)
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("process: %d %s", res.StatusCode, body)
	}
	return decode[txn](e.t, body)
}

func (e *env) patch(id string, ops string, headers map[string]string) (*http.Response, []byte) {
	return e.patchRaw(id, `{"operations":`+ops+`}`, headers)
}

func (e *env) patchRaw(id, body string, headers map[string]string) (*http.Response, []byte) {
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		h[k] = v
	}
	return e.do("PATCH", "/transactions/"+id+"/items", strings.NewReader(body), h)
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

func descs(items []item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = fmt.Sprintf("%s=%.2f", it.Description, it.Amount)
	}
	return out
}

// ---------------------------------------------------------------- gold, end to end

func TestProcessMatchesGold(t *testing.T) {
	e := newEnv(t)
	goldBytes, _ := os.ReadFile(filepath.Join(fixtures, "gold.json"))
	var gold map[string]struct {
		txn
		LineItems []item `json:"line_items"`
	}
	json.Unmarshal(goldBytes, &gold)

	for name, want := range gold {
		t.Run(name, func(t *testing.T) {
			got := e.process(name)
			if got.Merchant != want.Merchant || got.Date != want.Date || got.Currency != want.Currency ||
				got.GrandTotal != want.GrandTotal || got.Status != want.Status {
				t.Errorf("header = %+v, want %+v", got, want.txn)
			}
			if len(got.Taxes) != len(want.Taxes) {
				t.Fatalf("taxes = %+v, want %+v", got.Taxes, want.Taxes)
			}
			for i := range want.Taxes {
				g, w := got.Taxes[i], want.Taxes[i]
				if g.Name != w.Name || g.Rate != w.Rate || g.Amount != w.Amount {
					t.Errorf("tax[%d] = %+v, want %+v", i, g, w)
				}
			}
			if fmt.Sprint(descs(got.Items)) != fmt.Sprint(descs(want.LineItems)) {
				t.Errorf("items = %v, want %v", descs(got.Items), descs(want.LineItems))
			}

			// GET returns exactly what process returned.
			res, body := e.do("GET", "/transactions/"+got.ID, nil, nil)
			if res.StatusCode != 200 || res.Header.Get("ETag") != `"1"` {
				t.Fatalf("get: %d etag=%s", res.StatusCode, res.Header.Get("ETag"))
			}
			if again := decode[txn](t, body); fmt.Sprint(again) != fmt.Sprint(got) {
				t.Errorf("GET differs from process response")
			}
		})
	}
}

func TestMismatchIsReportedNotForced(t *testing.T) {
	e := newEnv(t)
	got := e.process("receipt-mismatch")
	if got.GrandTotal != 18.5 || len(got.Items) != 2 {
		t.Fatalf("total/items changed: %+v", got)
	}
	if len(got.Issues) != 1 || got.Issues[0].Code != "TOTAL_MISMATCH" ||
		got.Issues[0].ComputedTotal != 11.9 || got.Issues[0].Difference != 6.6 {
		t.Fatalf("issues = %+v", got.Issues)
	}
}

func TestTaxOnlyHasInclusiveVATAndNoItems(t *testing.T) {
	e := newEnv(t)
	got := e.process("receipt-tax-only")
	if !got.Taxes[0].Inclusive || len(got.Items) != 0 || got.Issues[0].Code != "NO_ITEMS" {
		t.Fatalf("%+v", got)
	}
}

// ---------------------------------------------------------------- upload + OCR

func TestImageUploadUsesStubbedOCRByFilename(t *testing.T) {
	e := newEnv(t)
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 64)...)
	res, body := e.upload("receipt-clean.jpg", jpeg)
	if res.StatusCode != 201 || !strings.Contains(string(body), `"image/jpeg"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	rid := decode[struct {
		ID string `json:"receipt_id"`
	}](t, body).ID
	_, body = e.do("POST", "/receipts/"+rid+"/process", nil, nil)
	if got := decode[txn](t, body); got.Merchant != "Cafe Mitte" {
		t.Fatalf("%s", body)
	}
}

func TestUnknownImageFailsOCR(t *testing.T) {
	e := newEnv(t)
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	_, body := e.upload("mystery.png", png)
	rid := decode[struct {
		ID string `json:"receipt_id"`
	}](t, body).ID
	res, body := e.do("POST", "/receipts/"+rid+"/process", nil, nil)
	if res.StatusCode != 422 || decode[apiError](t, body).Error.Code != "OCR_FAILED" {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	_, body = e.do("GET", "/receipts/"+rid, nil, nil)
	if !strings.Contains(string(body), `"status": "OCR_FAILED"`) {
		t.Fatalf("%s", body)
	}
}

// A slow or flaky vendor is not the file's fault: sync /process answers 504 or 503,
// and the receipt keeps its status so the client can retry.
func TestSyncTransientOCRFailureLeavesReceiptRetryable(t *testing.T) {
	cases := []struct {
		name   string
		engine ocr.Engine
		status int
		code   string
	}{
		{"timeout", ocr.Chain(newGatedEngine(false), ocr.WithTimeout(20*time.Millisecond)), 504, "OCR_TIMEOUT"},
		{"vendor error", failingEngine{errors.New("vendor returned 502")}, 503, "OCR_UNAVAILABLE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnvWithEngine(t, tc.engine)
			rid := e.uploadFixture("receipt-clean", "receipt-clean.txt")
			res, body := e.do("POST", "/receipts/"+rid+"/process", nil, nil)
			if res.StatusCode != tc.status || decode[apiError](t, body).Error.Code != tc.code {
				t.Fatalf("%d %s", res.StatusCode, body)
			}
			if strings.Contains(string(body), "502") {
				t.Errorf("internal error text leaked to the client: %s", body)
			}
			_, body = e.do("GET", "/receipts/"+rid, nil, nil)
			if !strings.Contains(string(body), `"status": "UPLOADED"`) {
				t.Fatalf("receipt must stay retryable: %s", body)
			}
		})
	}
}

type failingEngine struct{ err error }

func (failingEngine) Name() string                                             { return "failing" }
func (f failingEngine) ExtractText(context.Context, ocr.Input) (string, error) { return "", f.err }

func TestUploadValidation(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name   string
		data   []byte
		status int
		code   string
	}{
		{"zip is rejected by sniffed type", []byte("PK\x03\x04" + strings.Repeat("x", 64)), 415, "UNSUPPORTED_MEDIA_TYPE"},
		{"empty", nil, 400, "EMPTY_FILE"},
		{"too large", bytes.Repeat([]byte("a"), 65<<10), 413, "FILE_TOO_LARGE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, body := e.upload("x.bin", c.data)
			if res.StatusCode != c.status || decode[apiError](t, body).Error.Code != c.code {
				t.Fatalf("%d %s", res.StatusCode, body)
			}
		})
	}
	res, _ := e.do("POST", "/receipts", strings.NewReader("{}"), map[string]string{"Content-Type": "application/json"})
	if res.StatusCode != 400 {
		t.Fatalf("non-multipart: %d", res.StatusCode)
	}
}

func TestDuplicateUploadIsFlagged(t *testing.T) {
	e := newEnv(t)
	first := e.uploadFixture("receipt-clean", "a.txt")
	data, _ := os.ReadFile(filepath.Join(fixtures, "receipt-clean.txt"))
	_, body := e.upload("b.txt", data)
	if !strings.Contains(string(body), `"duplicate_of": "`+first+`"`) {
		t.Fatalf("%s", body)
	}
}

func TestReprocessKeepsOneTransaction(t *testing.T) {
	e := newEnv(t)
	rid := e.uploadFixture("receipt-clean", "receipt-clean.txt")
	_, b1 := e.do("POST", "/receipts/"+rid+"/process", nil, nil)
	_, b2 := e.do("POST", "/receipts/"+rid+"/process", nil, nil)
	t1, t2 := decode[txn](t, b1), decode[txn](t, b2)
	if t1.ID != t2.ID || t2.Version != 2 {
		t.Fatalf("t1=%s t2=%s v=%d", t1.ID, t2.ID, t2.Version)
	}
}

func TestConcurrentProcessCreatesOneTransaction(t *testing.T) {
	e := newEnv(t)
	rid := e.uploadFixture("receipt-clean", "receipt-clean.txt")
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, b := e.do("POST", "/receipts/"+rid+"/process", nil, nil)
			ids[i] = decode[txn](t, b).ID
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("got several transactions: %v", ids)
		}
	}
}

// ---------------------------------------------------------------- re-itemize

func TestReitemizeReplacesItemsOnly(t *testing.T) {
	e := newEnv(t)
	orig := e.process("receipt-clean")
	res, _ := e.patch(orig.ID, `[{"op":"update","item_id":"`+orig.Items[0].ID+`","description":"Double espresso"}]`, nil)
	if res.StatusCode != 200 {
		t.Fatalf("patch %d", res.StatusCode)
	}
	// A stale If-Match is refused; without one, re-itemize just runs.
	if res, _ := e.do("POST", "/transactions/"+orig.ID+"/itemize", nil, map[string]string{"If-Match": `"1"`}); res.StatusCode != 412 {
		t.Fatalf("stale If-Match: %d", res.StatusCode)
	}
	res, body := e.do("POST", "/transactions/"+orig.ID+"/itemize", nil, nil)
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	got := decode[txn](t, body)
	if got.ID != orig.ID || got.GrandTotal != orig.GrandTotal || fmt.Sprint(got.Taxes) != fmt.Sprint(orig.Taxes) {
		t.Fatalf("header/taxes changed: %+v", got)
	}
	if fmt.Sprint(descs(got.Items)) != "[Espresso=3.50 Sandwich=8.90 Mineral water=2.60]" || got.Items[0].Source != "AUTO" {
		t.Fatalf("items = %v", descs(got.Items))
	}
}

// ---------------------------------------------------------------- PATCH items

func TestPatchMergeSplitThatReconciles(t *testing.T) {
	e := newEnv(t)
	orig := e.process("receipt-clean")
	espresso, sandwich, water := orig.Items[0].ID, orig.Items[1].ID, orig.Items[2].ID
	res, body := e.patch(orig.ID, `[
		{"op":"merge","item_ids":["`+espresso+`","`+water+`"],"description":"Drinks"},
		{"op":"split","item_id":"`+sandwich+`","into":[{"description":"Bread","amount":4.40},{"description":"Filling","amount":"4.50"}]}
	]`, map[string]string{"If-Match": `"1"`})
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	got := decode[txn](t, body)
	if fmt.Sprint(descs(got.Items)) != "[Drinks=6.10 Bread=4.40 Filling=4.50]" || got.Status != "COMPLETE" || got.Version != 2 {
		t.Fatalf("%v %s v%d", descs(got.Items), got.Status, got.Version)
	}
	if res.Header.Get("ETag") != `"2"` {
		t.Fatalf("etag %s", res.Header.Get("ETag"))
	}
}

func TestPatchThatBreaksTotalsIs409AndSavesNothing(t *testing.T) {
	e := newEnv(t)
	orig := e.process("receipt-clean")
	res, body := e.patch(orig.ID, `[{"op":"update","item_id":"`+orig.Items[0].ID+`","amount":5.00}]`, nil)
	if res.StatusCode != 409 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	apiErr := decode[apiError](t, body)
	details := decode[struct {
		Issues   []issue `json:"issues"`
		Proposed []item  `json:"proposed_line_items"`
	}](t, apiErr.Error.Details)
	if apiErr.Error.Code != "ITEMS_DO_NOT_RECONCILE" || details.Issues[0].Difference != -1.5 || details.Proposed[0].Amount != 5 {
		t.Fatalf("%s", body)
	}
	_, after := e.do("GET", "/transactions/"+orig.ID, nil, nil)
	if fmt.Sprint(decode[txn](t, after)) != fmt.Sprint(orig) {
		t.Fatal("transaction changed after a rejected PATCH")
	}
}

func TestPatchCanResolveTaxOnlyReview(t *testing.T) {
	e := newEnv(t)
	orig := e.process("receipt-tax-only")
	res, body := e.patch(orig.ID, `[{"op":"add","description":"Trip fare","amount":24.00}]`, nil)
	got := decode[txn](t, body)
	if res.StatusCode != 200 || got.Status != "COMPLETE" || got.GrandTotal != 24 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

func TestPatchOnMismatchReceiptStays409(t *testing.T) {
	e := newEnv(t)
	orig := e.process("receipt-mismatch")
	res, _ := e.patch(orig.ID, `[{"op":"update","item_id":"`+orig.Items[0].ID+`","description":"Still water"}]`, nil)
	if res.StatusCode != 409 {
		t.Fatalf("%d", res.StatusCode)
	}
}

func TestPatchStaleVersionIs412(t *testing.T) {
	e := newEnv(t)
	orig := e.process("receipt-clean")
	id := orig.Items[0].ID
	ok, _ := e.patch(orig.ID, `[{"op":"update","item_id":"`+id+`","description":"A"}]`, map[string]string{"If-Match": `"1"`})
	stale, body := e.patch(orig.ID, `[{"op":"update","item_id":"`+id+`","description":"B"}]`, map[string]string{"If-Match": `"1"`})
	if ok.StatusCode != 200 || stale.StatusCode != 412 || decode[apiError](t, body).Error.Code != "VERSION_MISMATCH" {
		t.Fatalf("%d %d %s", ok.StatusCode, stale.StatusCode, body)
	}
}

func TestPatchValidation(t *testing.T) {
	e := newEnv(t)
	orig := e.process("receipt-clean")
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"unknown item", `{"operations":[{"op":"delete","item_id":"nope"}]}`, 422, "INVALID_OPERATION"},
		{"no ops", `{"operations":[]}`, 422, "INVALID_OPERATION"},
		{"bad json", `{"operations":`, 400, "INVALID_JSON"},
		{"unknown field", `{"operations":[{"op":"delete","item_id":"x","colour":"red"}]}`, 400, "INVALID_JSON"},
		{"sub-cent amount", `{"operations":[{"op":"add","description":"x","amount":0.001}]}`, 400, "INVALID_JSON"},
		{"overflowing amount", `{"operations":[{"op":"add","description":"x","amount":"99999999999999999999"}]}`, 400, "INVALID_JSON"},
		{"trailing data", `{"operations":[{"op":"delete","item_id":"x"}]} {"operations":[]}`, 400, "INVALID_JSON"},
		{"field the op does not take", `{"operations":[{"op":"delete","item_id":"` + orig.Items[0].ID + `","amount":5}]}`, 422, "INVALID_OPERATION"},
		{"zero quantity", `{"operations":[{"op":"add","description":"x","amount":1,"quantity":0}]}`, 422, "INVALID_OPERATION"},
		{"negative tax", `{"operations":[{"op":"add","description":"x","amount":1,"tax_amount":-1}]}`, 422, "INVALID_OPERATION"},
		{"delete every item", `{"operations":[` + deleteAll(orig.Items) + `]}`, 422, "NO_ITEMS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, body := e.patchRaw(orig.ID, c.body, nil)
			if res.StatusCode != c.status || decode[apiError](t, body).Error.Code != c.code {
				t.Fatalf("%d %s", res.StatusCode, body)
			}
		})
	}
}

// `curl -d` without -H sends form-urlencoded; the JSON body must still be accepted.
func TestPatchAcceptsCurlDefaultContentType(t *testing.T) {
	e := newEnv(t)
	orig := e.process("receipt-tax-only")
	res, body := e.patchRaw(orig.ID, `{"operations":[{"op":"add","description":"Trip fare","amount":24.00}]}`,
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

func deleteAll(items []item) string {
	ops := make([]string, len(items))
	for i, it := range items {
		ops[i] = `{"op":"delete","item_id":"` + it.ID + `"}`
	}
	return strings.Join(ops, ",")
}

func TestNotFoundAndHealth(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/transactions/txn_missing"},
		{"POST", "/transactions/txn_missing/itemize"},
		{"PATCH", "/transactions/txn_missing/items"},
		{"GET", "/receipts/rcpt_missing"},
		{"POST", "/receipts/rcpt_missing/process"},
	} {
		body := io.Reader(nil)
		headers := map[string]string{"X-Request-ID": "trace-123", "If-Match": `"1"`}
		if c.method == "PATCH" {
			body = strings.NewReader(`{"operations":[{"op":"delete","item_id":"x"}]}`)
			headers["Content-Type"] = "application/json"
		}
		res, b := e.do(c.method, c.path, body, headers)
		if res.StatusCode != 404 || decode[apiError](t, b).RequestID != "trace-123" {
			t.Errorf("%s %s: %d %s", c.method, c.path, res.StatusCode, b)
		}
	}
	res, _ := e.do("GET", "/health", nil, nil)
	if res.StatusCode != 200 {
		t.Fatalf("health %d", res.StatusCode)
	}
}
