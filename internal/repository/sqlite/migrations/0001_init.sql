-- Types, and why:
--   * Money is INTEGER minor units (cents): exact, and sums never drift.
--   * Instants (created_at, updated_at, processed_at) are INTEGER Unix epoch
--     milliseconds, UTC: compact, and they sort and compare as numbers.
--   * txn_date is TEXT 'YYYY-MM-DD': a calendar date printed on a receipt has no
--     time or zone, so an epoch would invent one.
--   * Tax rates and quantities are exact decimals, stored as a value and a power of
--     ten: rate% = rate_value / 10^rate_scale, so 19.123% is (19123, 3). No REAL.
--   * Tables are STRICT, so SQLite itself rejects a value of the wrong type.

CREATE TABLE IF NOT EXISTS receipts (
    id            TEXT    PRIMARY KEY,
    filename      TEXT    NOT NULL,
    content_type  TEXT    NOT NULL,
    size_bytes    INTEGER NOT NULL CHECK (size_bytes > 0),
    sha256        TEXT    NOT NULL CHECK (length(sha256) = 64),
    storage_path  TEXT    NOT NULL,
    status        TEXT    NOT NULL CHECK (status IN ('UPLOADED', 'PROCESSING', 'PROCESSED', 'OCR_FAILED')),
    ocr_error     TEXT,
    created_at    INTEGER NOT NULL,                  -- epoch ms, UTC
    processed_at  INTEGER                            -- epoch ms, UTC
) STRICT;
CREATE INDEX IF NOT EXISTS receipts_sha256 ON receipts (sha256);
CREATE INDEX IF NOT EXISTS receipts_status ON receipts (status); -- startup recovery scans PROCESSING

-- Every OCR run is kept; the transaction points at the run it was built from.
CREATE TABLE IF NOT EXISTS ocr_results (
    id          TEXT    PRIMARY KEY,
    receipt_id  TEXT    NOT NULL REFERENCES receipts (id),
    engine      TEXT    NOT NULL,
    raw_text    TEXT    NOT NULL,
    created_at  INTEGER NOT NULL                     -- epoch ms, UTC
) STRICT;
CREATE INDEX IF NOT EXISTS ocr_results_receipt ON ocr_results (receipt_id, created_at);

CREATE TABLE IF NOT EXISTS transactions (
    id                TEXT    PRIMARY KEY,
    receipt_id        TEXT    NOT NULL UNIQUE REFERENCES receipts (id), -- one transaction per receipt
    ocr_result_id     TEXT    NOT NULL REFERENCES ocr_results (id),
    merchant          TEXT,
    txn_date          TEXT    CHECK (txn_date GLOB '[0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]'),
    currency          TEXT    CHECK (currency GLOB '[A-Z][A-Z][A-Z]'),  -- ISO 4217
    subtotal_cents    INTEGER,
    grand_total_cents INTEGER,
    itemize_status    TEXT    NOT NULL CHECK (itemize_status IN ('COMPLETE', 'NEEDS_REVIEW', 'FAILED')),
    itemize_issues    TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(itemize_issues)), -- explains NEEDS_REVIEW / FAILED
    version           INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),  -- optimistic concurrency, exposed as ETag
    created_at        INTEGER NOT NULL,              -- epoch ms, UTC
    updated_at        INTEGER NOT NULL               -- epoch ms, UTC
) STRICT;

CREATE TABLE IF NOT EXISTS tax_lines (
    id              TEXT    PRIMARY KEY,
    transaction_id  TEXT    NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    position        INTEGER NOT NULL,
    name            TEXT    NOT NULL,
    rate_value      INTEGER CHECK (rate_value >= 0),  -- rate% = rate_value / 10^rate_scale
    rate_scale      INTEGER CHECK (rate_scale BETWEEN 0 AND 7),
    amount_cents    INTEGER NOT NULL,
    inclusive       INTEGER NOT NULL CHECK (inclusive IN (0, 1)),
    jurisdiction    TEXT,
    CHECK ((rate_value IS NULL) = (rate_scale IS NULL))
) STRICT;
CREATE INDEX IF NOT EXISTS tax_lines_txn ON tax_lines (transaction_id, position);

CREATE TABLE IF NOT EXISTS line_items (
    id                TEXT    PRIMARY KEY,
    transaction_id    TEXT    NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    position          INTEGER NOT NULL,
    description       TEXT    NOT NULL,
    amount_cents      INTEGER NOT NULL,
    quantity_value    INTEGER CHECK (quantity_value > 0), -- quantity = quantity_value / 10^quantity_scale
    quantity_scale    INTEGER CHECK (quantity_scale BETWEEN 0 AND 9),
    tax_amount_cents  INTEGER,
    source            TEXT    NOT NULL CHECK (source IN ('AUTO', 'USER')),
    CHECK ((quantity_value IS NULL) = (quantity_scale IS NULL))
) STRICT;
CREATE INDEX IF NOT EXISTS line_items_txn ON line_items (transaction_id, position);
