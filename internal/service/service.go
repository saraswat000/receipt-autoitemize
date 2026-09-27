// Package service is the application layer: it orchestrates storage, OCR, extraction
// and itemize rules for each use case. It knows nothing about HTTP.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"receipt-autoitemize/internal/domain"
	"receipt-autoitemize/internal/extract"
	"receipt-autoitemize/internal/id"
	"receipt-autoitemize/internal/itemize"
	"receipt-autoitemize/internal/ocr"
	"receipt-autoitemize/internal/repository"
)

// Errors the transport layer maps to status codes.
var (
	ErrNotFound           = errors.New("not found")
	ErrPreconditionFailed = errors.New("version does not match If-Match")
	// ErrConcurrentUpdate: another write landed between our read and write, and the
	// client sent no If-Match (so no precondition failed; we lost a race).
	ErrConcurrentUpdate = errors.New("transaction changed concurrently")
	// ErrNoTotal: the transaction has no grand total, so edited items can never be
	// reconciled. Re-process the receipt instead.
	ErrNoTotal = errors.New("transaction has no grand total")
	// ErrNoItems: an edit would leave the transaction with no line items.
	ErrNoItems   = errors.New("a transaction needs at least one line item")
	ErrOCRFailed = errors.New("ocr failed")
	// ErrOCRUnavailable is a transient OCR failure (timeout, cancelled request, vendor
	// hiccup). The receipt is left as it was, so the client can simply retry.
	ErrOCRUnavailable   = errors.New("ocr temporarily unavailable")
	ErrUnsupportedMedia = errors.New("unsupported media type")
	ErrEmptyFile        = errors.New("empty file")
)

// ReconcileError is returned when edited items do not add up. Nothing was saved.
type ReconcileError struct {
	Issues   []domain.Issue
	Proposed []domain.LineItem
}

func (e *ReconcileError) Error() string { return "line items do not reconcile with the transaction" }

// FileStore keeps uploaded files. It is declared here, where it is used: local disk
// today (storage.Disk), object storage later, with no change to the service. Get must
// return an error wrapping fs.ErrNotExist for a missing file (fs is the standard,
// storage-neutral sentinel; an S3 store wraps it for NoSuchKey).
type FileStore interface {
	Put(ctx context.Context, name string, data []byte) (ref string, err error)
	Get(ctx context.Context, ref string) ([]byte, error)
	Delete(ctx context.Context, ref string) error
}

type Service struct {
	repo  repository.Repository
	ocr   ocr.Engine
	files FileStore
	log   *slog.Logger
	now   func() time.Time
	queue Queue // nil = synchronous processing
}

// New builds the service on any repository, OCR engine and file store.
func New(repo repository.Repository, engine ocr.Engine, files FileStore, log *slog.Logger) *Service {
	return &Service{repo: repo, ocr: engine, files: files, log: log, now: func() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }}
}

func (s *Service) OCREngineName() string          { return s.ocr.Name() }
func (s *Service) Ping(ctx context.Context) error { return s.repo.Ping(ctx) }

// ---------------------------------------------------------------- upload

// UploadResult is the upload response.
type UploadResult struct {
	domain.Receipt
	DuplicateOf *string `json:"duplicate_of"`
}

// allowedTypes are matched against http.DetectContentType, so only types it can
// sniff belong here (it never reports HEIC; supporting iPhone originals would need
// our own ftyp-box check).
var allowedTypes = []string{"image/jpeg", "image/png", "image/webp", "image/gif", "application/pdf", "text/plain"}

// Upload stores the file and creates a receipt. contentType must be sniffed from the
// bytes by the caller, never taken from the client's header.
func (s *Service) Upload(ctx context.Context, filename, contentType string, data []byte) (UploadResult, error) {
	if len(data) == 0 {
		return UploadResult{}, ErrEmptyFile
	}
	if !allowed(contentType) {
		return UploadResult{}, fmt.Errorf("%w: %s", ErrUnsupportedMedia, contentType)
	}
	sum := sha256.Sum256(data)
	r := domain.Receipt{
		ID:          id.New("rcpt"),
		Filename:    truncate(filepath.Base(filename), maxFilenameLen),
		ContentType: contentType,
		SizeBytes:   int64(len(data)),
		SHA256:      hex.EncodeToString(sum[:]),
		Status:      domain.ReceiptUploaded,
		CreatedAt:   s.now(),
	}
	// The stored name comes from our ID and the sniffed type, never from client input.
	ref, err := s.files.Put(ctx, r.ID+extensions[baseType(contentType)], data)
	if err != nil {
		return UploadResult{}, fmt.Errorf("store file: %w", err)
	}
	r.StoragePath = ref
	if err := s.repo.CreateReceipt(ctx, r); err != nil {
		// No row points at the file, so remove it; a crash right here leaves an
		// orphan file, which is harmless (a sweep could collect it).
		_ = s.files.Delete(context.WithoutCancel(ctx), ref)
		return UploadResult{}, err
	}
	res := UploadResult{Receipt: r}
	// Same bytes uploaded before is a likely duplicate expense; flag it, don't block it.
	// The receipt is already saved, so failing the upload here would make the client
	// retry and create a real duplicate: log and answer without the hint instead.
	if dup, ok, err := s.repo.FindDuplicate(ctx, r.SHA256, r.ID); err != nil {
		s.log.WarnContext(ctx, "duplicate check failed", "receipt_id", r.ID, "err", err)
	} else if ok {
		res.DuplicateOf = &dup
	}
	return res, nil
}

var extensions = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif",
	"application/pdf": ".pdf", "text/plain": ".txt",
}

const maxFilenameLen = 255

func baseType(ct string) string {
	base, _, _ := strings.Cut(ct, ";")
	return strings.TrimSpace(base)
}

func allowed(ct string) bool {
	for _, a := range allowedTypes {
		if baseType(ct) == a {
			return true
		}
	}
	return false
}

// truncate cuts s to at most n bytes without splitting a UTF-8 character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ReceiptView is a receipt with its latest OCR text and transaction link.
type ReceiptView struct {
	domain.Receipt
	OCREngine     *string `json:"ocr_engine"`
	OCRText       *string `json:"ocr_text"`
	TransactionID *string `json:"transaction_id"`
}

func (s *Service) GetReceipt(ctx context.Context, id string) (ReceiptView, error) {
	r, err := s.repo.GetReceipt(ctx, id)
	if err != nil {
		return ReceiptView{}, mapStoreErr(err)
	}
	v := ReceiptView{Receipt: r}
	if o, err := s.repo.LatestOCR(ctx, id); err == nil {
		v.OCREngine, v.OCRText = &o.Engine, &o.Text
	} else if !errors.Is(err, repository.ErrNotFound) {
		return v, err
	}
	if txnID, ok, err := s.repo.TransactionIDForReceipt(ctx, id); err != nil {
		return v, err
	} else if ok {
		v.TransactionID = &txnID
	}
	return v, nil
}

// ---------------------------------------------------------------- process

// Process runs OCR + extraction + auto-itemize synchronously and creates (or, on
// re-process, updates) the receipt's single transaction.
func (s *Service) Process(ctx context.Context, receiptID string) (domain.Transaction, error) {
	txnID, err := s.processOnce(ctx, receiptID)
	var oe *ocrError
	if errors.As(err, &oe) {
		if Retryable(oe.err) {
			// Not the file's fault: do not brand the receipt OCR_FAILED.
			return domain.Transaction{}, fmt.Errorf("%w: %w", ErrOCRUnavailable, oe.err)
		}
		s.log.WarnContext(ctx, "ocr failed", "receipt_id", receiptID, "err", err)
		// Record the failure even if the client has already hung up.
		if markErr := s.repo.MarkReceiptFailed(context.WithoutCancel(ctx), receiptID, FailureReason(err), s.now()); markErr != nil {
			return domain.Transaction{}, markErr
		}
	}
	if err != nil {
		return domain.Transaction{}, err
	}
	return s.GetTransaction(ctx, txnID)
}

// processOnce is one attempt: OCR, extract, reconcile, save atomically. It is
// idempotent (the save is an upsert on receipt_id), so the async worker can retry it.
// It does not record failures; the caller decides whether a failure is final.
func (s *Service) processOnce(ctx context.Context, receiptID string) (string, error) {
	r, err := s.repo.GetReceipt(ctx, receiptID)
	if err != nil {
		return "", mapStoreErr(err)
	}

	data, err := s.files.Get(ctx, r.StoragePath)
	if err != nil {
		return "", &ocrError{err: fmt.Errorf("read upload: %w", err)}
	}
	text, err := s.ocr.ExtractText(ctx, ocr.Input{Data: data, Filename: r.Filename, ContentType: r.ContentType, SHA256: r.SHA256})
	if err != nil {
		return "", &ocrError{err: err}
	}

	now := s.now()
	ocrResult := domain.OCRResult{ID: id.New("ocr"), ReceiptID: r.ID, Engine: s.ocr.Name(), Text: text, CreatedAt: now}

	h := extract.ParseHeader(text)
	items := withIDs(extract.AutoItemize(text))
	status, issues := itemize.Reconcile(itemize.Input{
		Items: items, Taxes: h.Taxes, GrandTotal: h.GrandTotal, Subtotal: h.Subtotal, Adjustments: h.Adjustments,
	})
	for i := range h.Taxes {
		h.Taxes[i].ID = id.New("tax")
	}

	return s.repo.SaveProcessed(ctx, ocrResult, domain.Transaction{
		ID:            id.New("txn"), // ignored when the receipt already has a transaction
		ReceiptID:     r.ID,
		Merchant:      h.Merchant,
		Date:          h.Date,
		Currency:      h.Currency,
		Subtotal:      h.Subtotal,
		GrandTotal:    h.GrandTotal,
		Taxes:         h.Taxes,
		LineItems:     items,
		ItemizeStatus: status,
		ItemizeIssues: issues,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
}

// FailureReason is the client-safe explanation of a processing failure. It is what
// ocr_error stores and what an OCR_FAILED response says. Raw errors can carry storage
// paths or vendor details, so callers log those instead of showing them.
func FailureReason(err error) string {
	switch {
	case errors.Is(err, ocr.ErrNoText):
		return err.Error() // the engine's own message is written for users
	case errors.Is(err, fs.ErrNotExist):
		return "the uploaded file is no longer available; upload it again"
	case errors.Is(err, ErrOCRFailed):
		return "OCR could not read this file"
	case Retryable(err):
		return "processing did not complete; process the receipt again later"
	}
	return "processing failed"
}

// ocrError wraps an engine failure so callers can match ErrOCRFailed and still reach
// the engine's own error (e.g. ocr.ErrNoText) with errors.Is.
type ocrError struct{ err error }

func (e *ocrError) Error() string        { return fmt.Sprintf("%v: %v", ErrOCRFailed, e.err) }
func (e *ocrError) Is(target error) bool { return target == ErrOCRFailed }
func (e *ocrError) Unwrap() error        { return e.err }

func (s *Service) GetTransaction(ctx context.Context, id string) (domain.Transaction, error) {
	t, err := s.repo.GetTransaction(ctx, id)
	return t, mapStoreErr(err)
}

// ---------------------------------------------------------------- itemize

// Reitemize re-runs auto-itemize from the OCR text the transaction was built from.
// It replaces line items only (user edits included); header and taxes are untouched.
// ifMatch, when non-nil, must equal the current version; clients that want to be
// sure they are not discarding an edit they have not seen should send it.
func (s *Service) Reitemize(ctx context.Context, txnID string, ifMatch *int) (domain.Transaction, error) {
	t, err := s.loadForWrite(ctx, txnID, ifMatch)
	if err != nil {
		return t, err
	}
	o, err := s.repo.OCRByID(ctx, t.OCRResultID)
	if err != nil {
		return t, fmt.Errorf("load stored OCR: %w", err)
	}
	items := withIDs(extract.AutoItemize(o.Text))
	status, issues := itemize.Reconcile(itemize.Input{
		Items: items, Taxes: t.Taxes, GrandTotal: t.GrandTotal, Subtotal: t.Subtotal,
		Adjustments: extract.ParseHeader(o.Text).Adjustments,
	})
	if err := s.repo.ReplaceItems(ctx, t.ID, t.Version, items, status, issues, s.now()); err != nil {
		return t, writeErr(err, ifMatch)
	}
	return s.GetTransaction(ctx, t.ID)
}

// PatchItems applies user edits atomically. If the result does not reconcile with the
// stored grand total and taxes, it returns *ReconcileError and saves nothing.
func (s *Service) PatchItems(ctx context.Context, txnID string, ifMatch *int, ops []itemize.Operation) (domain.Transaction, error) {
	t, err := s.loadForWrite(ctx, txnID, ifMatch)
	if err != nil {
		return t, err
	}
	if t.GrandTotal == nil {
		return t, ErrNoTotal
	}
	proposed, err := itemize.Apply(t.LineItems, ops, func() string { return id.New("li") })
	if err != nil {
		return t, err
	}
	if len(proposed) == 0 {
		return t, ErrNoItems
	}
	// The printed subtotal is not enforced here: the user may legitimately re-split
	// items; what must hold is items + added taxes == grand total.
	status, issues := itemize.Reconcile(itemize.Input{Items: proposed, Taxes: t.Taxes, GrandTotal: t.GrandTotal})
	if status != domain.ItemizeComplete {
		return t, &ReconcileError{Issues: issues, Proposed: proposed}
	}
	if err := s.repo.ReplaceItems(ctx, t.ID, t.Version, proposed, status, issues, s.now()); err != nil {
		return t, writeErr(err, ifMatch)
	}
	return s.GetTransaction(ctx, t.ID)
}

func (s *Service) loadForWrite(ctx context.Context, txnID string, ifMatch *int) (domain.Transaction, error) {
	t, err := s.repo.GetTransaction(ctx, txnID)
	if err != nil {
		return t, mapStoreErr(err)
	}
	if ifMatch != nil && *ifMatch != t.Version {
		return t, ErrPreconditionFailed
	}
	return t, nil
}

func withIDs(items []domain.LineItem) []domain.LineItem {
	for i := range items {
		items[i].ID = id.New("li")
	}
	return items
}

func mapStoreErr(err error) error {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, repository.ErrVersionConflict):
		// Someone else wrote between our read and write.
		return ErrConcurrentUpdate
	}
	return err
}

// writeErr maps a failed optimistic write. A version conflict is a failed
// precondition (412) only if the client actually sent one; otherwise the request
// lost a race (409).
func writeErr(err error, ifMatch *int) error {
	if ifMatch != nil && errors.Is(err, repository.ErrVersionConflict) {
		return ErrPreconditionFailed
	}
	return mapStoreErr(err)
}
