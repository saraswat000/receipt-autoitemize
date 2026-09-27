package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression: int64 wrap-around in the reconcile sum lets a PATCH that is off by ~1.8e19
// be saved as COMPLETE. 184 items of the maximum amount plus one chosen remainder
// sum to 2^64 + 15.00 cents, which wraps to exactly the receipt's item total.
func TestPatchItemsThatOverflowAreRejected(t *testing.T) {
	e := newEnv(t)
	orig := e.process("receipt-clean") // items 15.00 + VAT 2.85 = 17.85
	ops := []string{deleteAll(orig.Items)}
	for i := 0; i < 184; i++ {
		ops = append(ops, fmt.Sprintf(`{"op":"add","description":"big %d","amount":"999999999999999.99"}`, i))
	}
	ops = append(ops, `{"op":"add","description":"rest","amount":"467440737095533.00"}`)
	res, body := e.patch(orig.ID, "["+strings.Join(ops, ",")+"]", nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("items summing to ~1.8e19 were accepted: %d %.300s", res.StatusCode, body)
	}
}

// Regression: a client-controlled file extension longer than the OS name limit makes
// os.WriteFile fail with ENAMETOOLONG, which surfaces as 500 INTERNAL instead of a 4xx.
func TestOverlongExtensionIsNotAServerError(t *testing.T) {
	e := newEnv(t)
	data, _ := os.ReadFile(filepath.Join(fixtures, "receipt-clean.txt"))
	res, body := e.upload("receipt."+strings.Repeat("a", 300), data)
	if res.StatusCode >= 500 {
		t.Fatalf("client-controlled filename caused %d: %s", res.StatusCode, body)
	}
}

// Regression: README says all errors share {"error":{...},"request_id"}; the mux's own
// 404 and 405 answers are text/plain with no JSON body and no request_id.
func TestRouterErrorsUseTheJSONShape(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/nope"},
		{"POST", "/receipts/rcpt_x/"}, // trailing slash
		{"DELETE", "/receipts"},
		{"PUT", "/transactions/txn_x/items"},
	} {
		res, body := e.do(c.method, c.path, nil, map[string]string{"X-Request-ID": "trace-1"})
		var ae apiError
		if !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") ||
			json.Unmarshal(body, &ae) != nil || ae.Error.Code == "" || ae.RequestID != "trace-1" {
			t.Errorf("%s %s: %d ct=%q body=%q", c.method, c.path, res.StatusCode, res.Header.Get("Content-Type"), body)
		}
	}
}

// Regression: a UTF-8 BOM (which Windows editors add to .txt files) is accepted as
// text/plain but is glued to the first line, so "MERCHANT:" no longer matches and
// the merchant is silently lost while every other field parses.
func TestUTF8BOMKeepsTheMerchant(t *testing.T) {
	e := newEnv(t)
	data, _ := os.ReadFile(filepath.Join(fixtures, "receipt-clean.txt"))
	rid := e.uploadFixtureBytes(append([]byte("\xef\xbb\xbf"), data...))
	res, body := e.do("POST", "/receipts/"+rid+"/process", nil, nil)
	tx := decode[txn](t, body)
	if res.StatusCode != 200 || tx.Merchant != "Cafe Mitte" {
		t.Fatalf("BOM upload: %d merchant=%q (status %s)", res.StatusCode, tx.Merchant, tx.Status)
	}
}

func (e *env) uploadFixtureBytes(data []byte) string {
	e.t.Helper()
	res, body := e.upload("bom.txt", data)
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("upload: %d %s", res.StatusCode, body)
	}
	var r struct {
		ReceiptID string `json:"receipt_id"`
	}
	json.Unmarshal(body, &r)
	return r.ReceiptID
}

// Quantities are exact decimals end to end (no float): what the client sends is what
// comes back, and zero or negative quantities are refused.
func TestQuantityRoundTripsExactly(t *testing.T) {
	e := newEnv(t)
	orig := e.process("receipt-clean")
	res, body := e.patch(orig.ID, `[{"op":"update","item_id":"`+orig.Items[0].ID+`","quantity":1.125}]`, nil)
	if res.StatusCode != 200 || !strings.Contains(string(body), `"quantity": 1.125`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	_, after := e.do("GET", "/transactions/"+orig.ID, nil, nil)
	if !strings.Contains(string(after), `"quantity": 1.125`) {
		t.Fatalf("stored quantity changed: %s", after)
	}
}
