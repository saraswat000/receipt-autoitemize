// Package memory implements repository.Repository in process memory.
//
// It exists to prove the repository boundary is real (the same contract suite runs
// against it and against SQLite) and for fast tests or demos with STORE=memory. Data is
// lost on restart, so async crash recovery has nothing to recover with this backend.
package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"receipt-autoitemize/internal/domain"
	"receipt-autoitemize/internal/repository"
)

type Repo struct {
	mu            sync.Mutex
	receipts      map[string]domain.Receipt
	ocr           map[string]domain.OCRResult
	ocrByReceipt  map[string][]string // insertion order; last is newest
	txns          map[string]domain.Transaction
	txnForReceipt map[string]string
}

var _ repository.Repository = (*Repo)(nil)

func New() *Repo {
	return &Repo{
		receipts:      map[string]domain.Receipt{},
		ocr:           map[string]domain.OCRResult{},
		ocrByReceipt:  map[string][]string{},
		txns:          map[string]domain.Transaction{},
		txnForReceipt: map[string]string{},
	}
}

func (r *Repo) Ping(context.Context) error { return nil }
func (r *Repo) Close() error               { return nil }

// ---------------------------------------------------------------- receipts

func (r *Repo) CreateReceipt(_ context.Context, rc domain.Receipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.receipts[rc.ID]; ok {
		return fmt.Errorf("receipt %s already exists", rc.ID)
	}
	r.receipts[rc.ID] = copyReceipt(rc)
	return nil
}

func (r *Repo) GetReceipt(_ context.Context, id string) (domain.Receipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rc, ok := r.receipts[id]
	if !ok {
		return domain.Receipt{}, repository.ErrNotFound
	}
	return copyReceipt(rc), nil
}

func (r *Repo) FindDuplicate(_ context.Context, sha256, excludeID string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var matches []domain.Receipt
	for _, rc := range r.receipts {
		if rc.SHA256 == sha256 && rc.ID != excludeID {
			matches = append(matches, rc)
		}
	}
	if len(matches) == 0 {
		return "", false, nil
	}
	sortReceipts(matches)
	return matches[0].ID, true, nil
}

func (r *Repo) MarkReceiptFailed(_ context.Context, id, reason string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rc, ok := r.receipts[id]
	if !ok {
		return nil // same as an UPDATE that matches no rows
	}
	rc.Status, rc.OCRError, rc.ProcessedAt = domain.ReceiptFailed, &reason, timePtr(at)
	r.receipts[id] = rc
	return nil
}

func (r *Repo) MarkProcessing(_ context.Context, id string) (domain.ReceiptStatus, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rc, ok := r.receipts[id]
	if !ok {
		return "", false, repository.ErrNotFound
	}
	prev := rc.Status
	if prev == domain.ReceiptProcessing {
		return prev, false, nil
	}
	rc.Status = domain.ReceiptProcessing
	r.receipts[id] = rc
	return prev, true, nil
}

func (r *Repo) RestoreStatus(_ context.Context, id string, status domain.ReceiptStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rc, ok := r.receipts[id]; ok && rc.Status == domain.ReceiptProcessing {
		rc.Status = status
		r.receipts[id] = rc
	}
	return nil
}

func (r *Repo) PendingReceipts(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var pending []domain.Receipt
	for _, rc := range r.receipts {
		if rc.Status == domain.ReceiptProcessing {
			pending = append(pending, rc)
		}
	}
	sortReceipts(pending)
	ids := make([]string, len(pending))
	for i, rc := range pending {
		ids[i] = rc.ID
	}
	return ids, nil
}

// ---------------------------------------------------------------- OCR

func (r *Repo) LatestOCR(_ context.Context, receiptID string) (domain.OCRResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := r.ocrByReceipt[receiptID]
	if len(ids) == 0 {
		return domain.OCRResult{}, repository.ErrNotFound
	}
	return r.ocr[ids[len(ids)-1]], nil
}

func (r *Repo) OCRByID(_ context.Context, id string) (domain.OCRResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.ocr[id]
	if !ok {
		return domain.OCRResult{}, repository.ErrNotFound
	}
	return o, nil
}

// ---------------------------------------------------------------- transactions

func (r *Repo) TransactionIDForReceipt(_ context.Context, receiptID string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.txnForReceipt[receiptID]
	return id, ok, nil
}

func (r *Repo) GetTransaction(_ context.Context, id string) (domain.Transaction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.txns[id]
	if !ok {
		return domain.Transaction{}, repository.ErrNotFound
	}
	return copyTxn(t), nil
}

func (r *Repo) SaveProcessed(_ context.Context, o domain.OCRResult, t domain.Transaction) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rc, ok := r.receipts[o.ReceiptID]
	if !ok {
		return "", repository.ErrNotFound
	}
	if _, dup := r.ocr[o.ID]; dup {
		return "", fmt.Errorf("ocr result %s already exists", o.ID)
	}
	if _, hasTxn := r.txnForReceipt[o.ReceiptID]; !hasTxn {
		if other, dup := r.txns[t.ID]; dup && other.ReceiptID != o.ReceiptID {
			// SQLite refuses this through the primary key; so must we.
			return "", fmt.Errorf("transaction %s already exists for another receipt", t.ID)
		}
	}
	// All checks passed; the writes below cannot fail, so the save is atomic.
	o.CreatedAt = norm(o.CreatedAt)
	r.ocr[o.ID] = o
	r.ocrByReceipt[o.ReceiptID] = append(r.ocrByReceipt[o.ReceiptID], o.ID)
	rc.Status, rc.OCRError, rc.ProcessedAt = domain.ReceiptProcessed, nil, timePtr(o.CreatedAt)
	r.receipts[rc.ID] = rc

	t = copyTxn(t)
	t.ReceiptID, t.OCRResultID = o.ReceiptID, o.ID
	if existingID, ok := r.txnForReceipt[o.ReceiptID]; ok {
		old := r.txns[existingID]
		t.ID, t.CreatedAt, t.Version = existingID, old.CreatedAt, old.Version+1
	} else {
		t.Version = 1
		r.txnForReceipt[o.ReceiptID] = t.ID
	}
	r.txns[t.ID] = t
	return t.ID, nil
}

func (r *Repo) ReplaceItems(_ context.Context, txnID string, expectedVersion int,
	items []domain.LineItem, status domain.ItemizeStatus, issues []domain.Issue, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.txns[txnID]
	if !ok {
		return repository.ErrNotFound
	}
	if t.Version != expectedVersion {
		return repository.ErrVersionConflict
	}
	t.LineItems = copyItems(items)
	t.ItemizeStatus, t.ItemizeIssues = status, copyIssues(issues)
	t.Version++
	t.UpdatedAt = norm(at)
	r.txns[txnID] = t
	return nil
}

// ---------------------------------------------------------------- copies
//
// Callers get and give values that share no memory with the repository, the same
// isolation a real database provides.

// norm matches what SQLite stores: UTC, millisecond precision (epoch ms).
func norm(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

func timePtr(t time.Time) *time.Time { t = norm(t); return &t }

func sortReceipts(rs []domain.Receipt) {
	sort.Slice(rs, func(i, j int) bool {
		if !rs[i].CreatedAt.Equal(rs[j].CreatedAt) {
			return rs[i].CreatedAt.Before(rs[j].CreatedAt)
		}
		return rs[i].ID < rs[j].ID
	})
}

func ptr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyReceipt(rc domain.Receipt) domain.Receipt {
	rc.CreatedAt = norm(rc.CreatedAt)
	rc.OCRError, rc.ProcessedAt = ptr(rc.OCRError), ptr(rc.ProcessedAt)
	return rc
}

func copyItems(in []domain.LineItem) []domain.LineItem {
	out := make([]domain.LineItem, len(in))
	for i, it := range in {
		it.Quantity, it.TaxAmount = ptr(it.Quantity), ptr(it.TaxAmount)
		out[i] = it
	}
	return out
}

func copyIssues(in []domain.Issue) []domain.Issue {
	out := make([]domain.Issue, len(in))
	for i, is := range in {
		is.ItemsTotal, is.AddedTaxes, is.ComputedTotal = ptr(is.ItemsTotal), ptr(is.AddedTaxes), ptr(is.ComputedTotal)
		is.GrandTotal, is.Subtotal, is.Difference = ptr(is.GrandTotal), ptr(is.Subtotal), ptr(is.Difference)
		out[i] = is
	}
	return out
}

func copyTxn(t domain.Transaction) domain.Transaction {
	t.Merchant, t.Date, t.Currency = ptr(t.Merchant), ptr(t.Date), ptr(t.Currency)
	t.Subtotal, t.GrandTotal = ptr(t.Subtotal), ptr(t.GrandTotal)
	taxes := make([]domain.TaxLine, len(t.Taxes))
	for i, tl := range t.Taxes {
		tl.Rate, tl.Jurisdiction = ptr(tl.Rate), ptr(tl.Jurisdiction)
		taxes[i] = tl
	}
	t.Taxes = taxes
	t.LineItems = copyItems(t.LineItems)
	t.ItemizeIssues = copyIssues(t.ItemizeIssues)
	t.CreatedAt, t.UpdatedAt = norm(t.CreatedAt), norm(t.UpdatedAt)
	return t
}
