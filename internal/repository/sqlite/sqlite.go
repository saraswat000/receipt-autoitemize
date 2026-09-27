// Package sqlite implements repository.Repository on SQLite.
// Every multi-row write runs in one database transaction.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"receipt-autoitemize/internal/domain"
	"receipt-autoitemize/internal/repository"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so `go run` and Docker just work
)

// Migrations are applied in file-name order, each in its own transaction, and the
// applied count is kept in PRAGMA user_version. Never edit a released file; add one.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// Aliases so callers can match errors from either package.
var (
	ErrNotFound        = repository.ErrNotFound
	ErrVersionConflict = repository.ErrVersionConflict
)

// Store is the SQLite repository.
type Store struct{ db *sql.DB }

var _ repository.Repository = (*Store)(nil)

// Open opens (or creates) the database at path and applies the schema.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer at a time; a single connection serialises writes
	// cleanly instead of surfacing SQLITE_BUSY under concurrent requests. It also
	// serialises reads, which is the scalability ceiling of this backend (see
	// ARCHITECTURE.md); Postgres is the answer, not more SQLite connections.
	db.SetMaxOpenConns(1)
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(names) {
		return fmt.Errorf("database schema version %d is newer than this binary (%d migrations)", version, len(names))
	}
	for i := version; i < len(names); i++ {
		body, err := migrationFS.ReadFile(names[i])
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, string(body))
		if err == nil {
			// PRAGMA takes no bind parameters; i+1 is an int we control.
			_, err = tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1))
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			return fmt.Errorf("migration %s: %w", names[i], err)
		}
	}
	return nil
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
		created   int64
		processed sql.NullInt64
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
		t := parseTS(processed.Int64)
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

// MarkProcessing claims a receipt for processing. The conditional UPDATE is what
// makes the claim atomic (on any database, not only because SQLite has one
// connection): exactly one caller sees a row affected. claimed is false when the
// receipt is already PROCESSING, which collapses duplicate process calls into one job.
func (s *Store) MarkProcessing(ctx context.Context, id string) (prev domain.ReceiptStatus, claimed bool, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		// prev is only used to undo the claim (RestoreStatus), never to decide it.
		if err := tx.QueryRowContext(ctx, `SELECT status FROM receipts WHERE id = ?`, id).Scan(&prev); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE receipts SET status = 'PROCESSING' WHERE id = ? AND status <> 'PROCESSING'`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		claimed = n == 1
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
		created int64
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
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM receipts WHERE id = ?`, ocr.ReceiptID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
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
			var rate *domain.Decimal
			if tl.Rate != nil {
				rate = &tl.Rate.Percent
			}
			rateValue, rateScale := decimalArgs(rate)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO tax_lines (id, transaction_id, position, name, rate_value, rate_scale, amount_cents, inclusive, jurisdiction)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				tl.ID, txnID, i, tl.Name, rateValue, rateScale, int64(tl.Amount), boolInt(tl.Inclusive), tl.Jurisdiction); err != nil {
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
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
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
		qtyValue, qtyScale := decimalArgs(it.Quantity)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO line_items (id, transaction_id, position, description, amount_cents,
				quantity_value, quantity_scale, tax_amount_cents, source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			it.ID, txnID, i, it.Description, int64(it.Amount), qtyValue, qtyScale, moneyArg(it.TaxAmount), it.Source); err != nil {
			return fmt.Errorf("insert line item: %w", err)
		}
	}
	return nil
}

// queryer is what the read helpers need; both *sql.DB and *sql.Tx satisfy it.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// GetTransaction loads a transaction with its taxes and line items. The three reads
// run in one SQL transaction, so a concurrent ReplaceItems can never pair version N's
// header (and ETag) with version N+1's items.
func (s *Store) GetTransaction(ctx context.Context, id string) (t domain.Transaction, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		t, err = getTransaction(ctx, tx, id)
		return err
	})
	return t, err
}

func getTransaction(ctx context.Context, q queryer, id string) (domain.Transaction, error) {
	var (
		t                  domain.Transaction
		subtotal, total    sql.NullInt64
		issues             string
		created, updated   int64
		merchant, date, cc sql.NullString
	)
	err := q.QueryRowContext(ctx, `
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
	if t.ItemizeIssues == nil {
		t.ItemizeIssues = []domain.Issue{}
	}

	if t.Taxes, err = taxes(ctx, q, id); err != nil {
		return t, err
	}
	t.LineItems, err = items(ctx, q, id)
	return t, err
}

func taxes(ctx context.Context, q queryer, txnID string) ([]domain.TaxLine, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, name, rate_value, rate_scale, amount_cents, inclusive, jurisdiction
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
			scale     sql.NullInt64
			amount    int64
			inclusive int
			juris     sql.NullString
		)
		if err := rows.Scan(&tl.ID, &tl.Name, &rate, &scale, &amount, &inclusive, &juris); err != nil {
			return nil, err
		}
		if d := decimalPtr(rate, scale); d != nil {
			tl.Rate = &domain.Rate{Percent: *d}
		}
		tl.Amount, tl.Inclusive, tl.Jurisdiction = domain.Money(amount), inclusive == 1, strPtr(juris)
		out = append(out, tl)
	}
	return out, rows.Err()
}

func items(ctx context.Context, q queryer, txnID string) ([]domain.LineItem, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, description, amount_cents, quantity_value, quantity_scale, tax_amount_cents, source
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
			qty    sql.NullInt64
			scale  sql.NullInt64
			tax    sql.NullInt64
		)
		if err := rows.Scan(&it.ID, &it.Description, &amount, &qty, &scale, &tax, &it.Source); err != nil {
			return nil, err
		}
		it.Amount, it.TaxAmount, it.Quantity = domain.Money(amount), moneyPtr(tax), decimalPtr(qty, scale)
		out = append(out, it)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- helpers

// Instants are stored as INTEGER Unix epoch milliseconds (UTC); they sort and
// compare as numbers. Sub-millisecond precision is dropped (the memory backend
// truncates the same way, so both backends return identical values).
func ts(t time.Time) int64 { return t.UnixMilli() }

func parseTS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// decimalArgs splits an optional exact decimal into its value and scale columns.
func decimalArgs(d *domain.Decimal) (value, scale any) {
	if d == nil {
		return nil, nil
	}
	return d.Value, int64(d.Scale)
}

func decimalPtr(value, scale sql.NullInt64) *domain.Decimal {
	if !value.Valid || !scale.Valid || scale.Int64 < 0 || scale.Int64 > domain.MaxDecimalScale {
		return nil
	}
	d := domain.NewDecimal(value.Int64, uint8(scale.Int64))
	return &d
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
