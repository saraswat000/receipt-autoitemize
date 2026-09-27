// Package repository defines the persistence contract the service depends on.
// Any database can be plugged in by implementing Repository; the service never
// sees SQL. Implementations live in subpackages (sqlite, memory), and every
// implementation must pass the shared contract suite in repotest.
package repository

import (
	"context"
	"errors"
	"time"

	"receipt-autoitemize/internal/domain"
)

var (
	// ErrNotFound is returned when the requested row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrVersionConflict is returned when an optimistic-concurrency check fails.
	ErrVersionConflict = errors.New("version conflict")
)

// Receipts stores uploaded files' metadata and their processing status.
type Receipts interface {
	CreateReceipt(ctx context.Context, r domain.Receipt) error
	GetReceipt(ctx context.Context, id string) (domain.Receipt, error)
	// FindDuplicate returns the oldest other receipt with the same file hash.
	FindDuplicate(ctx context.Context, sha256, excludeID string) (id string, found bool, err error)
	MarkReceiptFailed(ctx context.Context, id, reason string, at time.Time) error
	// MarkProcessing atomically claims a receipt for processing. claimed is false
	// when it was already PROCESSING, so duplicate requests collapse into one job.
	// It leaves ocr_error alone, so RestoreStatus fully undoes a claim.
	MarkProcessing(ctx context.Context, id string) (prev domain.ReceiptStatus, claimed bool, err error)
	// RestoreStatus undoes MarkProcessing when the job could not be queued.
	RestoreStatus(ctx context.Context, id string, status domain.ReceiptStatus) error
	// PendingReceipts lists receipts still PROCESSING, oldest first (crash recovery).
	PendingReceipts(ctx context.Context) ([]string, error)
}

// OCRResults stores every OCR run; they are never updated.
type OCRResults interface {
	LatestOCR(ctx context.Context, receiptID string) (domain.OCRResult, error)
	OCRByID(ctx context.Context, id string) (domain.OCRResult, error)
}

// Transactions stores the Transaction aggregate: header, tax lines and line items
// are always read and written together.
type Transactions interface {
	GetTransaction(ctx context.Context, id string) (domain.Transaction, error)
	TransactionIDForReceipt(ctx context.Context, receiptID string) (id string, found bool, err error)

	// SaveProcessed atomically records an OCR run, marks the receipt PROCESSED and
	// creates or replaces the receipt's transaction (header, taxes, items). There is
	// at most one transaction per receipt: on re-process the existing ID is kept, its
	// version is bumped, and t.ID is ignored. Returns the transaction ID.
	SaveProcessed(ctx context.Context, ocr domain.OCRResult, t domain.Transaction) (string, error)

	// ReplaceItems swaps line items and itemize status only if the stored version still
	// equals expectedVersion; otherwise ErrVersionConflict. Header and taxes are untouched.
	ReplaceItems(ctx context.Context, txnID string, expectedVersion int,
		items []domain.LineItem, status domain.ItemizeStatus, issues []domain.Issue, at time.Time) error
}

// Repository is everything the service needs from persistence.
type Repository interface {
	Receipts
	OCRResults
	Transactions
	Ping(ctx context.Context) error
	Close() error
}
