-- Money is INTEGER minor units (cents). Tax rates are INTEGER basis points (19% = 1900).

CREATE TABLE IF NOT EXISTS receipts (
    id            TEXT PRIMARY KEY,
    filename      TEXT    NOT NULL,
    content_type  TEXT    NOT NULL,
    size_bytes    INTEGER NOT NULL,
    sha256        TEXT    NOT NULL,
    storage_path  TEXT    NOT NULL,
    status        TEXT    NOT NULL CHECK (status IN ('UPLOADED', 'PROCESSED', 'OCR_FAILED')),
    ocr_error     TEXT,
    created_at    TEXT    NOT NULL,
    processed_at  TEXT
);
CREATE INDEX IF NOT EXISTS receipts_sha256 ON receipts (sha256);

-- Every OCR run is kept; the newest row per receipt is the source for re-itemize.
CREATE TABLE IF NOT EXISTS ocr_results (
    id          TEXT PRIMARY KEY,
    receipt_id  TEXT NOT NULL REFERENCES receipts (id),
    engine      TEXT NOT NULL,
    raw_text    TEXT NOT NULL,
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS ocr_results_receipt ON ocr_results (receipt_id, created_at);

CREATE TABLE IF NOT EXISTS transactions (
    id                TEXT PRIMARY KEY,
    receipt_id        TEXT    NOT NULL UNIQUE REFERENCES receipts (id), -- one transaction per receipt
    ocr_result_id     TEXT    NOT NULL REFERENCES ocr_results (id),
    merchant          TEXT,
    txn_date          TEXT,
    currency          TEXT,
    subtotal_cents    INTEGER,
    grand_total_cents INTEGER,
    itemize_status    TEXT    NOT NULL CHECK (itemize_status IN ('COMPLETE', 'NEEDS_REVIEW', 'FAILED')),
    itemize_issues    TEXT    NOT NULL DEFAULT '[]', -- JSON array explaining NEEDS_REVIEW / FAILED
    version           INTEGER NOT NULL DEFAULT 1,    -- optimistic concurrency, exposed as ETag
    created_at        TEXT    NOT NULL,
    updated_at        TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS tax_lines (
    id              TEXT PRIMARY KEY,
    transaction_id  TEXT    NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    position        INTEGER NOT NULL,
    name            TEXT    NOT NULL,
    rate_bp         INTEGER,
    amount_cents    INTEGER NOT NULL,
    inclusive       INTEGER NOT NULL CHECK (inclusive IN (0, 1)),
    jurisdiction    TEXT
);
CREATE INDEX IF NOT EXISTS tax_lines_txn ON tax_lines (transaction_id, position);

CREATE TABLE IF NOT EXISTS line_items (
    id                TEXT PRIMARY KEY,
    transaction_id    TEXT    NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    position          INTEGER NOT NULL,
    description       TEXT    NOT NULL,
    amount_cents      INTEGER NOT NULL,
    quantity          REAL,
    tax_amount_cents  INTEGER,
    source            TEXT    NOT NULL CHECK (source IN ('AUTO', 'USER'))
);
CREATE INDEX IF NOT EXISTS line_items_txn ON line_items (transaction_id, position);
