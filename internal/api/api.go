// Package api is the HTTP transport: routing, request decoding, error mapping.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"receipt-autoitemize/internal/domain"
	"receipt-autoitemize/internal/itemize"
	"receipt-autoitemize/internal/service"
)

type Server struct {
	svc            *service.Service
	log            *slog.Logger
	maxUploadBytes int64
}

func New(svc *service.Service, log *slog.Logger, maxUploadBytes int64) *Server {
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
	return s.withRequestID(s.withRecover(s.withAccessLog(mux)))
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
	t, err := s.svc.Process(r.Context(), r.PathValue("id"))
	s.respondTxn(w, r, t, err)
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
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "request body too large", nil)
		return
	}
	var req patchItemsRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "INVALID_JSON", err.Error(), nil)
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
// Absent or "*" means no precondition.
func (s *Server) ifMatch(w http.ResponseWriter, r *http.Request) (*int, bool) {
	h := strings.TrimSpace(r.Header.Get("If-Match"))
	if h == "" || h == "*" {
		return nil, true
	}
	v, err := strconv.Atoi(strings.Trim(strings.TrimPrefix(h, "W/"), `"`))
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
	case errors.Is(err, service.ErrUnsupportedMedia):
		s.writeError(w, r, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", err.Error(), nil)
	case errors.Is(err, service.ErrEmptyFile):
		s.writeError(w, r, http.StatusBadRequest, "EMPTY_FILE", "uploaded file is empty", nil)
	case errors.Is(err, service.ErrOCRFailed):
		s.writeError(w, r, http.StatusUnprocessableEntity, "OCR_FAILED", err.Error(), nil)
	case errors.As(err, &opErr):
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_OPERATION", opErr.Message, opErr)
	case errors.As(err, &recErr):
		s.writeError(w, r, http.StatusConflict, "ITEMS_DO_NOT_RECONCILE",
			"edited line items do not reconcile with the transaction total and stored taxes; nothing was saved",
			map[string]any{"issues": recErr.Issues, "proposed_line_items": recErr.Proposed})
	default:
		s.log.ErrorContext(r.Context(), "internal error", "err", err, "request_id", requestID(r.Context()))
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
	b.RequestID = requestID(r.Context())
	writeJSON(w, status, b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
