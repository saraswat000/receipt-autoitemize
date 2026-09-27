// Package api is the HTTP transport: routing, request decoding, error mapping.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"receipt-autoitemize/internal/domain"
	"receipt-autoitemize/internal/itemize"
	"receipt-autoitemize/internal/reqid"
	"receipt-autoitemize/internal/service"
)

// Service is what the HTTP layer needs from the application layer. It is declared
// here, where it is consumed, so handlers can be tested against a fake.
type Service interface {
	Ping(ctx context.Context) error
	OCREngineName() string
	Async() bool
	Upload(ctx context.Context, filename, contentType string, data []byte) (service.UploadResult, error)
	GetReceipt(ctx context.Context, id string) (service.ReceiptView, error)
	Process(ctx context.Context, receiptID string) (domain.Transaction, error)
	ProcessAsync(ctx context.Context, receiptID string) (service.ReceiptView, error)
	GetTransaction(ctx context.Context, id string) (domain.Transaction, error)
	Reitemize(ctx context.Context, txnID string, ifMatch *int) (domain.Transaction, error)
	PatchItems(ctx context.Context, txnID string, ifMatch *int, ops []itemize.Operation) (domain.Transaction, error)
}

var _ Service = (*service.Service)(nil)

type Server struct {
	svc            Service
	log            *slog.Logger
	maxUploadBytes int64
}

func New(svc Service, log *slog.Logger, maxUploadBytes int64) *Server {
	return &Server{svc: svc, log: log, maxUploadBytes: maxUploadBytes}
}

// Handler returns the routed handler wrapped in middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /receipts", s.uploadReceipt)
	mux.HandleFunc("GET /receipts/{id}", s.getReceipt)
	mux.HandleFunc("POST /receipts/{id}/process", s.processReceipt)
	mux.HandleFunc("GET /transactions/{id}", s.getTransaction)
	mux.HandleFunc("POST /transactions/{id}/itemize", s.reitemize)
	mux.HandleFunc("PATCH /transactions/{id}/items", s.patchItems)
	// Anything unmatched still gets the JSON error shape (the mux's own 404/405 are
	// plain text): 405 with Allow when the path exists under another method.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var allow []string
		for _, m := range []string{"GET", "POST", "PATCH"} {
			probe := r.Clone(r.Context())
			probe.Method = m
			if _, pattern := mux.Handler(probe); pattern != "/" && pattern != "" {
				allow = append(allow, m)
			}
		}
		if len(allow) > 0 {
			w.Header().Set("Allow", strings.Join(allow, ", "))
			s.writeError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", r.Method+" is not supported here", nil)
			return
		}
		s.writeError(w, r, http.StatusNotFound, "NOT_FOUND", "no such endpoint", nil)
	})
	return s.middleware(mux)
}

// ---------------------------------------------------------------- handlers

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Ping(r.Context()); err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "UNHEALTHY", "database unreachable", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "ocr_engine": s.svc.OCREngineName()})
}

func (s *Server) uploadReceipt(w http.ResponseWriter, r *http.Request) {
	// Allow some headroom for multipart framing on top of the file itself.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadBytes+1<<20)
	file, header, err := r.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.writeError(w, r, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "upload exceeds the size limit",
				map[string]int64{"max_bytes": s.maxUploadBytes})
			return
		}
		s.writeError(w, r, http.StatusBadRequest, "INVALID_UPLOAD", `expected multipart/form-data with a "file" field`, nil)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, s.maxUploadBytes+1))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if int64(len(data)) > s.maxUploadBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "upload exceeds the size limit",
			map[string]int64{"max_bytes": s.maxUploadBytes})
		return
	}
	// Trust the bytes, not the client's Content-Type header.
	contentType := http.DetectContentType(data)

	res, err := s.svc.Upload(r.Context(), header.Filename, contentType, data)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/receipts/"+res.ID)
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) getReceipt(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.GetReceipt(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) processReceipt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.svc.Async() {
		t, err := s.svc.Process(r.Context(), id)
		s.respondTxn(w, r, t, err)
		return
	}
	// Async mode: accept the job and let the client poll the receipt.
	v, err := s.svc.ProcessAsync(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/receipts/"+id)
	writeJSON(w, http.StatusAccepted, v)
}

func (s *Server) getTransaction(w http.ResponseWriter, r *http.Request) {
	t, err := s.svc.GetTransaction(r.Context(), r.PathValue("id"))
	s.respondTxn(w, r, t, err)
}

func (s *Server) reitemize(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := s.ifMatch(w, r)
	if !ok {
		return
	}
	t, err := s.svc.Reitemize(r.Context(), r.PathValue("id"), ifMatch)
	s.respondTxn(w, r, t, err)
}

type patchItemsRequest struct {
	Operations []itemize.Operation `json:"operations"`
}

func (s *Server) patchItems(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := s.ifMatch(w, r)
	if !ok {
		return
	}
	// The body is parsed as JSON whatever the Content-Type says: `curl -d` sends
	// form-urlencoded by default, and rejecting that would only trip up clients.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.writeError(w, r, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "request body too large", nil)
			return
		}
		s.writeError(w, r, http.StatusBadRequest, "INVALID_BODY", "could not read the request body", nil)
		return
	}
	var req patchItemsRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "INVALID_JSON", err.Error(), nil)
		return
	}
	if dec.More() {
		s.writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "unexpected data after the JSON object", nil)
		return
	}
	if len(req.Operations) == 0 {
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_OPERATION", "operations must not be empty", nil)
		return
	}
	t, err := s.svc.PatchItems(r.Context(), r.PathValue("id"), ifMatch, req.Operations)
	s.respondTxn(w, r, t, err)
}

// ---------------------------------------------------------------- helpers

func (s *Server) respondTxn(w http.ResponseWriter, r *http.Request, t domain.Transaction, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(t.Version)))
	writeJSON(w, http.StatusOK, t)
}

// ifMatch parses an optional If-Match header holding a transaction version ("3").
// Absent or "*" means no precondition. Weak tags (W/"3") are rejected: If-Match
// uses strong comparison.
func (s *Server) ifMatch(w http.ResponseWriter, r *http.Request) (*int, bool) {
	h := strings.TrimSpace(r.Header.Get("If-Match"))
	if h == "" || h == "*" {
		return nil, true
	}
	v, err := strconv.Atoi(strings.Trim(h, `"`))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "INVALID_IF_MATCH", `If-Match must be an ETag from this API, e.g. "3"`, nil)
		return nil, false
	}
	return &v, true
}

// fail maps service errors to HTTP responses.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var (
		opErr  *itemize.OpError
		recErr *service.ReconcileError
	)
	switch {
	case errors.Is(err, service.ErrNotFound):
		s.writeError(w, r, http.StatusNotFound, "NOT_FOUND", "resource not found", nil)
	case errors.Is(err, service.ErrPreconditionFailed):
		s.writeError(w, r, http.StatusPreconditionFailed, "VERSION_MISMATCH",
			"the transaction changed since you read it; GET it again and retry", nil)
	case errors.Is(err, service.ErrConcurrentUpdate):
		s.writeError(w, r, http.StatusConflict, "CONCURRENT_UPDATE",
			"another request changed the transaction at the same time; GET it again and retry", nil)
	case errors.Is(err, service.ErrNoTotal):
		s.writeError(w, r, http.StatusUnprocessableEntity, "NO_TOTAL",
			"this transaction has no grand total, so its items cannot be reconciled; re-process the receipt", nil)
	case errors.Is(err, service.ErrNoItems):
		s.writeError(w, r, http.StatusUnprocessableEntity, "NO_ITEMS", "a transaction needs at least one line item", nil)
	case errors.Is(err, service.ErrUnsupportedMedia):
		s.writeError(w, r, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", err.Error(), nil)
	case errors.Is(err, service.ErrEmptyFile):
		s.writeError(w, r, http.StatusBadRequest, "EMPTY_FILE", "uploaded file is empty", nil)
	case errors.Is(err, service.ErrQueueFull):
		w.Header().Set("Retry-After", "5")
		s.writeError(w, r, http.StatusServiceUnavailable, "QUEUE_FULL",
			"too many receipts are waiting for OCR; retry shortly", nil)
	case errors.Is(err, service.ErrOCRUnavailable) && errors.Is(err, context.DeadlineExceeded):
		s.writeError(w, r, http.StatusGatewayTimeout, "OCR_TIMEOUT",
			"OCR did not finish in time; the receipt was not changed, retry the request", nil)
	case errors.Is(err, service.ErrOCRUnavailable):
		s.log.WarnContext(r.Context(), "ocr unavailable", "err", err)
		w.Header().Set("Retry-After", "5")
		s.writeError(w, r, http.StatusServiceUnavailable, "OCR_UNAVAILABLE",
			"OCR is temporarily unavailable; the receipt was not changed, retry shortly", nil)
	case errors.Is(err, service.ErrOCRFailed):
		s.writeError(w, r, http.StatusUnprocessableEntity, "OCR_FAILED", service.FailureReason(err), nil)
	case errors.As(err, &opErr):
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_OPERATION", opErr.Message, opErr)
	case errors.As(err, &recErr):
		s.writeError(w, r, http.StatusConflict, "ITEMS_DO_NOT_RECONCILE",
			"edited line items do not reconcile with the transaction total and stored taxes; nothing was saved",
			map[string]any{"issues": recErr.Issues, "proposed_line_items": recErr.Proposed})
	default:
		s.log.ErrorContext(r.Context(), "internal error", "err", err)
		s.writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error", nil)
	}
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details any    `json:"details,omitempty"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string, details any) {
	var b errorBody
	b.Error.Code, b.Error.Message, b.Error.Details = code, msg, details
	b.RequestID = reqid.From(r.Context())
	writeJSON(w, status, b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
