// Package store persists receipts, OCR results and transactions in SQLite.
// Every multi-row write runs in one database transaction.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"receipt-autoitemize/internal/domain"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so `go run` and Docker just work
)

//go:embed schema.sql
var schema string

var (
	ErrNotFound        = errors.New("not found")
	ErrVersionConflict = errors.New("version conflict")
)

type Store struct{ db *sql.DB }

// Open opens (or creates) the database at path and applies the schema.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer at a time; a single connection serialises writes
	// cleanly instead of surfacing SQLITE_BUSY under concurrent requests.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- receipts

func (s *Store) CreateReceipt(ctx context.Context, r domain.Receipt) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO receipts (id, filename, content_type, size_bytes, sha256, storage_path, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Filename, r.ContentType, r.SizeBytes, r.SHA256, r.StoragePath, r.Status, ts(r.CreatedAt))
	return err
}

func (s *Store) GetReceipt(ctx context.Context, id string) (domain.Receipt, error) {
	var (
		r         domain.Receipt
		created   string
		processed sql.NullString
		ocrErr    sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, filename, content_type, size_bytes, sha256, storage_path, status, ocr_error, created_at, processed_at
		FROM receipts WHERE id = ?`, id).
		Scan(&r.ID, &r.Filename, &r.ContentType, &r.SizeBytes, &r.SHA256, &r.StoragePath, &r.Status, &ocrErr, &created, &processed)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.CreatedAt = parseTS(created)
	if processed.Valid {
		t := parseTS(processed.String)
		r.ProcessedAt = &t
	}
	if ocrErr.Valid {
		r.OCRError = &ocrErr.String
	}
	return r, nil
}

// FindDuplicate returns the oldest other receipt with the same file hash, if any.
func (s *Store) FindDuplicate(ctx context.Context, sha256, excludeID string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM receipts WHERE sha256 = ? AND id <> ? ORDER BY created_at, id LIMIT 1`,
		sha256, excludeID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

func (s *Store) MarkReceiptFailed(ctx context.Context, id, reason string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE receipts SET status = 'OCR_FAILED', ocr_error = ?, processed_at = ? WHERE id = ?`,
		reason, ts(at), id)
	return err
}

// MarkProcessing atomically moves a receipt to PROCESSING unless it already is, and
// returns the status it had before. claimed is false when another request already
// queued it, which is how duplicate process calls are collapsed into one job.
func (s *Store) MarkProcessing(ctx context.Context, id string) (prev domain.ReceiptStatus, claimed bool, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT status FROM receipts WHERE id = ?`, id).Scan(&prev); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if prev == domain.ReceiptProcessing {
			return nil
		}
		claimed = true
		_, err := tx.ExecContext(ctx,
			`UPDATE receipts SET status = 'PROCESSING', ocr_error = NULL WHERE id = ?`, id)
		return err
	})
	return prev, claimed, err
}

// RestoreStatus undoes MarkProcessing when the job could not be queued.
func (s *Store) RestoreStatus(ctx context.Context, id string, status domain.ReceiptStatus) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE receipts SET status = ? WHERE id = ? AND status = 'PROCESSING'`, status, id)
	return err
}

// PendingReceipts lists receipts left in PROCESSING, e.g. by a crash or shutdown.
func (s *Store) PendingReceipts(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM receipts WHERE status = 'PROCESSING' ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LatestOCR returns the newest OCR result for a receipt.
func (s *Store) LatestOCR(ctx context.Context, receiptID string) (domain.OCRResult, error) {
	return s.ocrWhere(ctx, `receipt_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, receiptID)
}

// OCRByID returns one OCR result.
func (s *Store) OCRByID(ctx context.Context, id string) (domain.OCRResult, error) {
	return s.ocrWhere(ctx, `id = ?`, id)
}

func (s *Store) ocrWhere(ctx context.Context, where string, arg any) (domain.OCRResult, error) {
	var (
		o       domain.OCRResult
		created string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, receipt_id, engine, raw_text, created_at FROM ocr_results WHERE `+where, arg).
		Scan(&o.ID, &o.ReceiptID, &o.Engine, &o.Text, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	o.CreatedAt = parseTS(created)
	return o, err
}

func (s *Store) TransactionIDForReceipt(ctx context.Context, receiptID string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM transactions WHERE receipt_id = ?`, receiptID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// ---------------------------------------------------------------- transactions

// SaveProcessed stores an OCR run and creates or updates the receipt's transaction
// with its header, taxes and auto-itemized lines, all atomically. The receipt_id
// UNIQUE constraint guarantees one transaction per receipt even under concurrent
// processing. It returns the transaction ID (the existing one on re-process).
func (s *Store) SaveProcessed(ctx context.Context, ocr domain.OCRResult, t domain.Transaction) (string, error) {
	var txnID string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ocr_results (id, receipt_id, engine, raw_text, created_at) VALUES (?, ?, ?, ?, ?)`,
			ocr.ID, ocr.ReceiptID, ocr.Engine, ocr.Text, ts(ocr.CreatedAt)); err != nil {
			return fmt.Errorf("insert ocr: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE receipts SET status = 'PROCESSED', ocr_error = NULL, processed_at = ? WHERE id = ?`,
			ts(ocr.CreatedAt), ocr.ReceiptID); err != nil {
			return fmt.Errorf("update receipt: %w", err)
		}
		issues, err := json.Marshal(t.ItemizeIssues)
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `
			INSERT INTO transactions (id, receipt_id, ocr_result_id, merchant, txn_date, currency,
				subtotal_cents, grand_total_cents, itemize_status, itemize_issues, version, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
			ON CONFLICT (receipt_id) DO UPDATE SET
				ocr_result_id = excluded.ocr_result_id, merchant = excluded.merchant,
				txn_date = excluded.txn_date, currency = excluded.currency,
				subtotal_cents = excluded.subtotal_cents, grand_total_cents = excluded.grand_total_cents,
				itemize_status = excluded.itemize_status, itemize_issues = excluded.itemize_issues,
				version = transactions.version + 1, updated_at = excluded.updated_at
			RETURNING id`,
			t.ID, t.ReceiptID, ocr.ID, t.Merchant, t.Date, t.Currency,
			moneyArg(t.Subtotal), moneyArg(t.GrandTotal), t.ItemizeStatus, string(issues),
			ts(t.CreatedAt), ts(t.UpdatedAt)).Scan(&txnID)
		if err != nil {
			return fmt.Errorf("upsert transaction: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tax_lines WHERE transaction_id = ?`, txnID); err != nil {
			return err
		}
		for i, tl := range t.Taxes {
			var rate any
			if tl.Rate != nil {
				rate = int64(*tl.Rate)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO tax_lines (id, transaction_id, position, name, rate_bp, amount_cents, inclusive, jurisdiction)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				tl.ID, txnID, i, tl.Name, rate, int64(tl.Amount), boolInt(tl.Inclusive), tl.Jurisdiction); err != nil {
				return fmt.Errorf("insert tax: %w", err)
			}
		}
		return replaceItems(ctx, tx, txnID, t.LineItems)
	})
	return txnID, err
}

// ReplaceItems swaps the transaction's line items and itemize status, but only if the
// stored version still equals expectedVersion (optimistic concurrency). Header and
// taxes are never touched.
func (s *Store) ReplaceItems(ctx context.Context, txnID string, expectedVersion int,
	items []domain.LineItem, status domain.ItemizeStatus, issues []domain.Issue, at time.Time) error {
	issuesJSON, err := json.Marshal(issues)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE transactions SET itemize_status = ?, itemize_issues = ?, version = version + 1, updated_at = ?
			WHERE id = ? AND version = ?`,
			status, string(issuesJSON), ts(at), txnID, expectedVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM transactions WHERE id = ?`, txnID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return ErrVersionConflict
		}
		return replaceItems(ctx, tx, txnID, items)
	})
}

func replaceItems(ctx context.Context, tx *sql.Tx, txnID string, items []domain.LineItem) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM line_items WHERE transaction_id = ?`, txnID); err != nil {
		return err
	}
	for i, it := range items {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO line_items (id, transaction_id, position, description, amount_cents, quantity, tax_amount_cents, source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			it.ID, txnID, i, it.Description, int64(it.Amount), it.Quantity, moneyArg(it.TaxAmount), it.Source); err != nil {
			return fmt.Errorf("insert line item: %w", err)
		}
	}
	return nil
}

// GetTransaction loads a transaction with its taxes and line items.
func (s *Store) GetTransaction(ctx context.Context, id string) (domain.Transaction, error) {
	var (
		t                  domain.Transaction
		subtotal, total    sql.NullInt64
		issues             string
		created, updated   string
		merchant, date, cc sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, receipt_id, ocr_result_id, merchant, txn_date, currency, subtotal_cents, grand_total_cents,
			itemize_status, itemize_issues, version, created_at, updated_at
		FROM transactions WHERE id = ?`, id).
		Scan(&t.ID, &t.ReceiptID, &t.OCRResultID, &merchant, &date, &cc, &subtotal, &total,
			&t.ItemizeStatus, &issues, &t.Version, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	t.Merchant, t.Date, t.Currency = strPtr(merchant), strPtr(date), strPtr(cc)
	t.Subtotal, t.GrandTotal = moneyPtr(subtotal), moneyPtr(total)
	t.CreatedAt, t.UpdatedAt = parseTS(created), parseTS(updated)
	if err := json.Unmarshal([]byte(issues), &t.ItemizeIssues); err != nil {
		return t, fmt.Errorf("decode itemize_issues: %w", err)
	}

	if t.Taxes, err = s.taxes(ctx, id); err != nil {
		return t, err
	}
	t.LineItems, err = s.items(ctx, id)
	return t, err
}

func (s *Store) taxes(ctx context.Context, txnID string) ([]domain.TaxLine, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, rate_bp, amount_cents, inclusive, jurisdiction
		FROM tax_lines WHERE transaction_id = ? ORDER BY position`, txnID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.TaxLine{}
	for rows.Next() {
		var (
			tl        domain.TaxLine
			rate      sql.NullInt64
			amount    int64
			inclusive int
			juris     sql.NullString
		)
		if err := rows.Scan(&tl.ID, &tl.Name, &rate, &amount, &inclusive, &juris); err != nil {
			return nil, err
		}
		if rate.Valid {
			r := domain.Rate(rate.Int64)
			tl.Rate = &r
		}
		tl.Amount, tl.Inclusive, tl.Jurisdiction = domain.Money(amount), inclusive == 1, strPtr(juris)
		out = append(out, tl)
	}
	return out, rows.Err()
}

func (s *Store) items(ctx context.Context, txnID string) ([]domain.LineItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, description, amount_cents, quantity, tax_amount_cents, source
		FROM line_items WHERE transaction_id = ? ORDER BY position`, txnID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.LineItem{}
	for rows.Next() {
		var (
			it     domain.LineItem
			amount int64
			qty    sql.NullFloat64
			tax    sql.NullInt64
		)
		if err := rows.Scan(&it.ID, &it.Description, &amount, &qty, &tax, &it.Source); err != nil {
			return nil, err
		}
		it.Amount, it.TaxAmount = domain.Money(amount), moneyPtr(tax)
		if qty.Valid {
			it.Quantity = &qty.Float64
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- helpers

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func moneyArg(m *domain.Money) any {
	if m == nil {
		return nil
	}
	return int64(*m)
}

func moneyPtr(v sql.NullInt64) *domain.Money {
	if !v.Valid {
		return nil
	}
	m := domain.Money(v.Int64)
	return &m
}

func strPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
