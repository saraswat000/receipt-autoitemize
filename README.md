# Receipt upload, taxes, auto-itemize

A Go HTTP API for Navan take-home **Task A**. Upload a receipt and process it: the service creates **one transaction** with its **tax lines as separate rows**, **auto-itemized line items**, the **raw OCR text** and an **itemize status** (`COMPLETE` / `NEEDS_REVIEW` / `FAILED`). Users can edit, merge and split items; an edit that breaks the totals is rejected with `409`, never silently fixed.

> **OCR is stubbed** (as the brief allows). No vendor is called and no API key is needed: a fixture `.txt` upload is its own OCR text, and an image or PDF named after a fixture (e.g. `receipt-clean.jpg`) returns that fixture's text.

## Run

Needs Go 1.25+ (or Docker).

```bash
make run      # API on :8080 (or: go run ./cmd/server; or: make docker)
make test     # all tests, with the race detector
```

## Try it (run in order; needs `curl` and `jq`)

```bash
# Health
curl -s localhost:8080/health

# 1. Upload a receipt  -> 201 {"receipt_id": "rcpt_...", "status": "UPLOADED", ...}
RID=$(curl -s -F file=@fixtures/task-a/receipt-clean.txt localhost:8080/receipts | jq -r .receipt_id)

# 2. Process it  -> 200 transaction: merchant, date, currency, taxes, line_items, itemize_status
curl -s -X POST localhost:8080/receipts/$RID/process

# 3. Read the receipt (status, raw OCR text, transaction_id) and the transaction (ETag = version)
curl -s localhost:8080/receipts/$RID
TID=$(curl -s localhost:8080/receipts/$RID | jq -r .transaction_id)
curl -si localhost:8080/transactions/$TID

# 4. Re-run auto-itemize from the stored OCR  -> same transaction, items replaced
curl -s -X POST localhost:8080/transactions/$TID/itemize

# 5. Edit items: merge espresso + water, split the sandwich  -> 200, still COMPLETE
T=$(curl -s localhost:8080/transactions/$TID)
ESPRESSO=$(echo "$T" | jq -r '.line_items[0].id')
SANDWICH=$(echo "$T" | jq -r '.line_items[1].id')
WATER=$(echo "$T" | jq -r '.line_items[2].id')
V=$(echo "$T" | jq .version)
curl -s -X PATCH localhost:8080/transactions/$TID/items \
  -H 'Content-Type: application/json' -H "If-Match: \"$V\"" \
  -d '{"operations": [
        {"op": "merge", "item_ids": ["'$ESPRESSO'", "'$WATER'"], "description": "Drinks"},
        {"op": "split", "item_id": "'$SANDWICH'", "into": [
          {"description": "Bread", "amount": 4.40}, {"description": "Filling", "amount": 4.50}]}
      ]}'

# 6. An edit that breaks the total  -> 409 ITEMS_DO_NOT_RECONCILE with the numbers; nothing saved
DRINKS=$(curl -s localhost:8080/transactions/$TID | jq -r '.line_items[0].id')
curl -s -X PATCH localhost:8080/transactions/$TID/items -H 'Content-Type: application/json' \
  -d '{"operations": [{"op": "update", "item_id": "'$DRINKS'", "amount": 9.99}]}'
```

`make demo` runs the same walk over all three fixtures against a running server.

PATCH operations (applied in order, all-or-nothing): `update` (`item_id` + any of `description`, `amount`, `quantity`, `tax_amount`), `merge` (`item_ids`, `description`, optional `amount`), `split` (`item_id`, `into`: 2+ items), `add` (`description`, `amount`), `delete` (`item_id`). `If-Match` is optional on writes.

## Results on the fixtures

| Fixture | Taxes | Items | Status |
|---|---|---|---|
| `receipt-clean` | VAT 19% 2.85, added | Espresso 3.50, Sandwich 8.90, Mineral water 2.60 | `COMPLETE` (15.00 + 2.85 = 17.85) |
| `receipt-tax-only` | VAT 19% 3.83, **inclusive** | none ("Trip fare" has no price) | `NEEDS_REVIEW` (`NO_ITEMS`) |
| `receipt-mismatch` | VAT 19% 1.90, added | Water 4.00, Snacks 6.00 | `NEEDS_REVIEW` (`TOTAL_MISMATCH`: 11.90 ≠ 18.50) |

Asserted against `gold.json` at the parser level and over HTTP. The tax-only receipt is resolved by the user with `{"op": "add", "description": "Trip fare", "amount": 24.00}`.

## Key decisions

- **Reconciliation:** `sum(items) + sum(added taxes) = grand total` (±1 cent). Items are net; inclusive taxes ("incl. VAT") are already in the prices. A mismatch is reported, never fixed: no invented balancing line, no adjusted total.
- **Taxes are rows** with `name`, `rate`, `amount` and an `inclusive` flag, so one receipt can carry several rates.
- **Exact types:** money is integer cents; rates and quantities are exact decimals (19.123% is kept); timestamps are stored as epoch milliseconds and returned as ISO-8601. No floats.
- **Raw OCR is the source of truth:** every OCR run is stored, and re-itemize reads the one the transaction was built from.
- **One transaction per receipt** (database-enforced); re-processing updates it in place.
- **Concurrency:** every write bumps `version` (the `ETag`); a stale `If-Match` gets `412`.

## Status codes

| Code | When |
|---|---|
| `200` / `201` | success (`201` on upload) |
| `400` | malformed request or JSON |
| `404` | unknown receipt, transaction or route |
| `409` | PATCH result doesn't reconcile (`ITEMS_DO_NOT_RECONCILE`), or a concurrent write won |
| `412` | stale `If-Match` |
| `413` / `415` | upload too large / unsupported file type (sniffed from the bytes) |
| `422` | invalid PATCH operation, or OCR found no text (receipt marked `OCR_FAILED`) |
| `503` / `504` | OCR temporarily unavailable / timed out (receipt left unchanged, safe to retry) |

Every error has the shape `{"error": {"code", "message", "details"}, "request_id"}`.

## More

- **Architecture:** [docs/architecture.pdf](docs/architecture.pdf) (8 pages, with diagrams) or [ARCHITECTURE.md](ARCHITECTURE.md): layers, data model, async mode, design patterns, testing, tradeoffs.
- **Async mode:** `PROCESS_MODE=async make run` (or `make run-async`) returns `202` from `/process` and runs OCR on a bounded worker pool with retries and crash recovery; poll `GET /receipts/{id}`.
- **In-memory storage:** `make run-memory` (same API, no database file).

<details>
<summary>Configuration (environment variables, all optional)</summary>

| Variable | Default | Meaning |
|---|---|---|
| `ADDR` | `:8080` | listen address |
| `DATA_DIR` | `./data` | SQLite database and uploaded files |
| `STORE` | `sqlite` | `sqlite` or `memory` |
| `FIXTURES_DIR` | `./fixtures/task-a` | text used by the stub OCR |
| `MAX_UPLOAD_MB` | `10` | upload size limit |
| `LOG_FORMAT` | `text` | `json` for structured logs |
| `PROCESS_MODE` | `sync` | `async` for the worker pool |
| `OCR_TIMEOUT` | `30s` | timeout per OCR call |
| `OCR_CACHE_SIZE` | `1000` | OCR results cached by file hash (`0` = off) |
| `OCR_WORKERS` / `OCR_QUEUE_SIZE` / `OCR_MAX_ATTEMPTS` | `4` / `100` / `3` | async pool size, queue length, retries |

</details>
