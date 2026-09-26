// Package service is the application layer: it orchestrates storage, OCR, extraction
// and itemize rules for each use case. It knows nothing about HTTP.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"receipt-autoitemize/internal/domain"
	"receipt-autoitemize/internal/extract"
	"receipt-autoitemize/internal/itemize"
	"receipt-autoitemize/internal/ocr"
	"receipt-autoitemize/internal/store"
)

// Errors the transport layer maps to status codes.
var (
	ErrNotFound           = errors.New("not found")
	ErrPreconditionFailed = errors.New("version does not match If-Match")
	ErrOCRFailed          = errors.New("ocr failed")
	ErrUnsupportedMedia   = errors.New("unsupported media type")
	ErrEmptyFile          = errors.New("empty file")
)

// ReconcileError is returned when edited items do not add up. Nothing was saved.
type ReconcileError struct {
	Issues   []domain.Issue
	Proposed []domain.LineItem
}

func (e *ReconcileError) Error() string { return "line items do not reconcile with the transaction" }

type Service struct {
	store      *store.Store
	ocr        ocr.Engine
	uploadsDir string
	now        func() time.Time
	queue      Queue // nil = synchronous processing
}

func New(st *store.Store, engine ocr.Engine, uploadsDir string) *Service {
	return &Service{store: st, ocr: engine, uploadsDir: uploadsDir, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) OCREngineName() string          { return s.ocr.Name() }
func (s *Service) Ping(ctx context.Context) error { return s.store.Ping(ctx) }

// NewID returns a prefixed random ID such as "txn_3f9c0a1b2c3d4e5f".
func NewID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// ---------------------------------------------------------------- upload

// UploadResult is the upload response.
type UploadResult struct {
	domain.Receipt
	DuplicateOf *string `json:"duplicate_of"`
}

var allowedTypes = []string{"image/jpeg", "image/png", "image/webp", "image/gif", "image/heic", "application/pdf", "text/plain"}

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
		ID:          NewID("rcpt"),
		Filename:    filepath.Base(filename),
		ContentType: contentType,
		SizeBytes:   int64(len(data)),
		SHA256:      hex.EncodeToString(sum[:]),
		Status:      domain.ReceiptUploaded,
		CreatedAt:   s.now(),
	}
	r.StoragePath = filepath.Join(s.uploadsDir, r.ID+strings.ToLower(filepath.Ext(r.Filename)))
	if err := os.WriteFile(r.StoragePath, data, 0o600); err != nil {
		return UploadResult{}, fmt.Errorf("store file: %w", err)
	}
	if err := s.store.CreateReceipt(ctx, r); err != nil {
		_ = os.Remove(r.StoragePath)
		return UploadResult{}, err
	}
	res := UploadResult{Receipt: r}
	// Same bytes uploaded before is a likely duplicate expense; flag it, don't block it.
	if dup, ok, err := s.store.FindDuplicate(ctx, r.SHA256, r.ID); err != nil {
		return UploadResult{}, err
	} else if ok {
		res.DuplicateOf = &dup
	}
	return res, nil
}

func allowed(ct string) bool {
	base, _, _ := strings.Cut(ct, ";")
	for _, a := range allowedTypes {
		if strings.TrimSpace(base) == a {
			return true
		}
	}
	return false
}

// ReceiptView is a receipt with its latest OCR text and transaction link.
type ReceiptView struct {
	domain.Receipt
	OCREngine     *string `json:"ocr_engine"`
	OCRText       *string `json:"ocr_text"`
	TransactionID *string `json:"transaction_id"`
}

func (s *Service) GetReceipt(ctx context.Context, id string) (ReceiptView, error) {
	r, err := s.store.GetReceipt(ctx, id)
	if err != nil {
		return ReceiptView{}, mapStoreErr(err)
	}
	v := ReceiptView{Receipt: r}
	if o, err := s.store.LatestOCR(ctx, id); err == nil {
		v.OCREngine, v.OCRText = &o.Engine, &o.Text
	} else if !errors.Is(err, store.ErrNotFound) {
		return v, err
	}
	if txnID, ok, err := s.store.TransactionIDForReceipt(ctx, id); err != nil {
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
	if err != nil {
		if errors.Is(err, ErrOCRFailed) {
			if markErr := s.store.MarkReceiptFailed(ctx, receiptID, err.Error(), s.now()); markErr != nil {
				return domain.Transaction{}, markErr
			}
		}
		return domain.Transaction{}, err
	}
	return s.GetTransaction(ctx, txnID)
}

// processOnce is one attempt: OCR, extract, reconcile, save atomically. It is
// idempotent (the save is an upsert on receipt_id), so the async worker can retry it.
// It does not record failures; the caller decides whether a failure is final.
func (s *Service) processOnce(ctx context.Context, receiptID string) (string, error) {
	r, err := s.store.GetReceipt(ctx, receiptID)
	if err != nil {
		return "", mapStoreErr(err)
	}

	text, err := s.ocr.ExtractText(ctx, ocr.Input{Path: r.StoragePath, Filename: r.Filename, ContentType: r.ContentType})
	if err != nil {
		return "", &ocrError{err: err}
	}

	now := s.now()
	ocrResult := domain.OCRResult{ID: NewID("ocr"), ReceiptID: r.ID, Engine: s.ocr.Name(), Text: text, CreatedAt: now}

	h := extract.ParseHeader(text)
	items := withIDs(extract.AutoItemize(text))
	status, issues := itemize.Reconcile(itemize.Input{
		Items: items, Taxes: h.Taxes, GrandTotal: h.GrandTotal, Subtotal: h.Subtotal,
	})
	for i := range h.Taxes {
		h.Taxes[i].ID = NewID("tax")
	}

	return s.store.SaveProcessed(ctx, ocrResult, domain.Transaction{
		ID:            NewID("txn"), // ignored when the receipt already has a transaction
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

// ocrError wraps an engine failure so callers can match ErrOCRFailed and still reach
// the engine's own error (e.g. ocr.ErrNoText) with errors.Is.
type ocrError struct{ err error }

func (e *ocrError) Error() string        { return fmt.Sprintf("%v: %v", ErrOCRFailed, e.err) }
func (e *ocrError) Is(target error) bool { return target == ErrOCRFailed }
func (e *ocrError) Unwrap() error        { return e.err }

func (s *Service) GetTransaction(ctx context.Context, id string) (domain.Transaction, error) {
	t, err := s.store.GetTransaction(ctx, id)
	return t, mapStoreErr(err)
}

// ---------------------------------------------------------------- itemize

// Reitemize re-runs auto-itemize from the OCR text the transaction was built from.
// It replaces line items only (user edits included); header and taxes are untouched.
// ifMatch, when non-nil, must equal the current version.
func (s *Service) Reitemize(ctx context.Context, txnID string, ifMatch *int) (domain.Transaction, error) {
	t, err := s.loadForWrite(ctx, txnID, ifMatch)
	if err != nil {
		return t, err
	}
	o, err := s.store.OCRByID(ctx, t.OCRResultID)
	if err != nil {
		return t, fmt.Errorf("load stored OCR: %w", err)
	}
	items := withIDs(extract.AutoItemize(o.Text))
	status, issues := itemize.Reconcile(itemize.Input{
		Items: items, Taxes: t.Taxes, GrandTotal: t.GrandTotal, Subtotal: t.Subtotal,
	})
	if err := s.store.ReplaceItems(ctx, t.ID, t.Version, items, status, issues, s.now()); err != nil {
		return t, mapStoreErr(err)
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
	proposed, err := itemize.Apply(t.LineItems, ops, func() string { return NewID("li") })
	if err != nil {
		return t, err
	}
	// The printed subtotal is not enforced here: the user may legitimately re-split
	// items; what must hold is items + added taxes == grand total.
	status, issues := itemize.Reconcile(itemize.Input{Items: proposed, Taxes: t.Taxes, GrandTotal: t.GrandTotal})
	if status != domain.ItemizeComplete {
		return t, &ReconcileError{Issues: issues, Proposed: proposed}
	}
	if err := s.store.ReplaceItems(ctx, t.ID, t.Version, proposed, status, issues, s.now()); err != nil {
		return t, mapStoreErr(err)
	}
	return s.GetTransaction(ctx, t.ID)
}

func (s *Service) loadForWrite(ctx context.Context, txnID string, ifMatch *int) (domain.Transaction, error) {
	t, err := s.store.GetTransaction(ctx, txnID)
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
		items[i].ID = NewID("li")
	}
	return items
}

func mapStoreErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, store.ErrVersionConflict):
		// Someone else wrote between our read and write.
		return ErrPreconditionFailed
	}
	return err
}
