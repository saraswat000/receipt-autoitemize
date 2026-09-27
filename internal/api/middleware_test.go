package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"receipt-autoitemize/internal/reqid"
)

func newTestServer(buf *bytes.Buffer) *Server {
	return &Server{log: slog.New(reqid.Handler{Handler: slog.NewTextHandler(buf, nil)})}
}

// A panicking handler answers 500 JSON and still gets an access-log line, and every
// log line of the request carries its request ID.
func TestPanicIsRecoveredAndLogged(t *testing.T) {
	var buf bytes.Buffer
	s := newTestServer(&buf)
	h := s.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-ID", "trace-7")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"INTERNAL"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	logs := buf.String()
	for _, want := range []string{`msg=panic`, `msg=request`, `status=500`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
	if n := strings.Count(logs, "request_id=trace-7"); n != 2 {
		t.Errorf("request_id on %d lines, want both:\n%s", n, logs)
	}
}

// After the response has started, recovery must not append a JSON error body.
func TestPanicAfterWriteDoesNotCorruptResponse(t *testing.T) {
	var buf bytes.Buffer
	s := newTestServer(&buf)
	h := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial"))
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Body.String() != "partial" {
		t.Fatalf("body = %q", rec.Body)
	}
}

// http.ErrAbortHandler is net/http's intentional abort; it must propagate.
func TestErrAbortHandlerIsRepanicked(t *testing.T) {
	var buf bytes.Buffer
	s := newTestServer(&buf)
	h := s.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler to propagate", p)
		}
		if !strings.Contains(buf.String(), "msg=request") {
			t.Error("aborted request was not access-logged")
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
}
