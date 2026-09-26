# Receipt upload, taxes, auto-itemize

A small Go HTTP API. You upload a receipt, it is processed, and the service stores **one transaction** with its **tax lines as separate rows**, **auto-itemized line items**, the **raw OCR text**, and an **itemize status** (`COMPLETE` / `NEEDS_REVIEW` / `FAILED`). Users can edit, merge and split items. Edits that break the totals are rejected with `409` and never "fixed" silently.

> **OCR is stubbed.** No OCR or LLM vendor is called and no API key is needed. Plain-text uploads (the fixture `.txt` files) are treated as the OCR output, and an image or PDF named after a fixture (e.g. `receipt-clean.jpg`) returns that fixture's text. The engine sits behind an interface (`internal/ocr`), so a real one can be dropped in.

## Run

Requires Go 1.25+ (older Go toolchains download 1.25 automatically).

```bash
make run          # or: go run ./cmd/server
make run-async    # same, but OCR runs on a goroutine worker pool (see "Async processing")
```

The server listens on `:8080` and keeps SQLite and uploaded files in `./data`. To use Docker instead: `make docker`.

```bash
make test         # unit + golden (gold.json) + end-to-end HTTP tests, with -race
make demo         # with the server running: walks every endpoint on the 3 fixtures
```

| Env var | Default | Meaning |
|---|---|---|
| `ADDR` | `:8080` | listen address |
| `STORE` | `sqlite` | repository backend: `sqlite` or `memory` (see "Repository layer") |
| `DATA_DIR` | `./data` | SQLite DB and stored uploads |
| `FIXTURES_DIR` | `./fixtures/task-a` | text used by the stub OCR |
| `MAX_UPLOAD_MB` | `10` | upload size limit |
| `LOG_FORMAT` | `text` | `json` for structured logs |
| `PROCESS_MODE` | `sync` | `async` returns `202` from `/process` and runs OCR on a worker pool |
| `OCR_WORKERS` | `4` | async: OCR jobs running at once (caps vendor concurrency) |
| `OCR_QUEUE_SIZE` | `100` | async: jobs that may wait before `/process` answers `503` |
| `OCR_TIMEOUT` | `30s` | timeout per OCR call (and per async attempt) |
| `OCR_CACHE_SIZE` | `1000` | OCR results kept in an LRU keyed by file hash; `0` turns it off |
| `OCR_MAX_ATTEMPTS` | `3` | async: attempts per job, with exponential backoff from 1s |

## API and example curls

All errors share one shape: `{"error": {"code", "message", "details"}, "request_id"}`. Each response carries an `X-Request-ID` header, and a caller-supplied one is propagated.

### `GET /health`

```bash
curl -s localhost:8080/health
# {"ocr_engine": "stub-fixtures", "status": "ok"}
```

### `POST /receipts`: upload (multipart, field `file`)

The file type is detected from the bytes, not from the client's header. Accepted types are JPEG, PNG, WebP, GIF, PDF, and plain text (for the stub). The response is `201` with a `Location` header. If the same bytes were uploaded before, `duplicate_of` names that earlier receipt; this flags a likely duplicate expense without blocking the upload.

```bash
curl -s -F file=@fixtures/task-a/receipt-clean.txt localhost:8080/receipts
# {"receipt_id": "rcpt_b8c3...", "status": "UPLOADED", "sha256": "4ca0...", "duplicate_of": null, ...}

RID=$(curl -s -F file=@fixtures/task-a/receipt-clean.txt localhost:8080/receipts | jq -r .receipt_id)
```

Errors: `415` for an unsupported type, `413` when the file is too large, `400` for an empty file or a request that isn't multipart.

### `POST /receipts/{id}/process`: OCR, extraction and auto-itemize

This creates the receipt's transaction. Processing the same receipt again updates that transaction in place, because the database allows only one transaction per receipt. The raw OCR text is stored in `ocr_results`.

```bash
curl -s -X POST localhost:8080/receipts/$RID/process
```

```json
{
  "id": "txn_9348...", "receipt_id": "rcpt_b8c3...", "ocr_result_id": "ocr_...",
  "merchant": "Cafe Mitte", "date": "2026-03-12", "currency": "EUR",
  "subtotal": 15.00, "grand_total": 17.85,
  "taxes": [{"id": "tax_...", "name": "VAT", "rate": 0.19, "amount": 2.85, "inclusive": false, "jurisdiction": null}],
  "line_items": [
    {"id": "li_...", "description": "Espresso",      "amount": 3.50, "quantity": null, "tax_amount": null, "source": "AUTO"},
    {"id": "li_...", "description": "Sandwich",      "amount": 8.90, "quantity": null, "tax_amount": null, "source": "AUTO"},
    {"id": "li_...", "description": "Mineral water", "amount": 2.60, "quantity": null, "tax_amount": null, "source": "AUTO"}
  ],
  "itemize_status": "COMPLETE", "itemize_issues": [], "version": 1, ...
}
```

The mismatch fixture keeps its total and its two items and explains the problem:

```json
"itemize_status": "NEEDS_REVIEW",
"itemize_issues": [{"code": "TOTAL_MISMATCH", "items_total": 10.00, "added_taxes": 1.90,
                    "computed_total": 11.90, "grand_total": 18.50, "difference": 6.60, ...}]
```

Errors: `404` for an unknown receipt, and `422 OCR_FAILED` when the stub has no text for the file. In that case the receipt is marked `OCR_FAILED`.

In async mode (`PROCESS_MODE=async`) this returns **`202 Accepted`** with the receipt (`"status": "PROCESSING"`) and a `Location: /receipts/{id}` header. Poll that URL until the status is `PROCESSED` (then `transaction_id` is set) or `OCR_FAILED`. Calling it again while the receipt is `PROCESSING` returns 202 without queuing a second job. When the queue is full it returns `503 QUEUE_FULL` with `Retry-After`, and the receipt keeps its previous status.

```bash
curl -si -X POST localhost:8080/receipts/$RID/process     # async: HTTP/1.1 202 Accepted
curl -s localhost:8080/receipts/$RID | jq '{status, transaction_id}'
```

### `GET /receipts/{id}`

Returns the receipt metadata, its status, the latest raw OCR text and the transaction ID.

```bash
curl -s localhost:8080/receipts/$RID
```

### `GET /transactions/{id}`

```bash
TID=$(curl -s localhost:8080/receipts/$RID | jq -r .transaction_id)
curl -si localhost:8080/transactions/$TID      # ETag: "1"
```

### `POST /transactions/{id}/itemize`: re-run auto-itemize from the stored OCR

This replaces the line items only, including any user edits. The header, the taxes and the transaction ID stay the same. `If-Match` is optional.

```bash
curl -s -X POST localhost:8080/transactions/$TID/itemize
```

### `PATCH /transactions/{id}/items`: user override

The body holds a list of operations that are applied in order and **all-or-nothing**:

| op | fields |
|---|---|
| `update` | `item_id`, any of `description`, `amount`, `quantity`, `tax_amount` |
| `merge` | `item_ids` (2+), `description`, optional `amount` (defaults to the sum) |
| `split` | `item_id`, `into`: 2+ `{description, amount, quantity?, tax_amount?}` |
| `add` | `description`, `amount`, optional `quantity`, `tax_amount` |
| `delete` | `item_id` |

Amounts may be JSON numbers or strings, and anything finer than a cent is rejected. Send `If-Match: "<version>"` from the ETag to guard against lost updates.

```bash
IDS=($(curl -s localhost:8080/transactions/$TID | jq -r '.line_items[].id'))

# Merge espresso + water, split the sandwich: still reconciles -> 200, version 2
curl -s -X PATCH localhost:8080/transactions/$TID/items \
  -H 'Content-Type: application/json' -H 'If-Match: "1"' \
  -d '{"operations": [
        {"op": "merge", "item_ids": ["'${IDS[0]}'", "'${IDS[2]}'"], "description": "Drinks"},
        {"op": "split", "item_id": "'${IDS[1]}'", "into": [
          {"description": "Bread", "amount": 4.40}, {"description": "Filling", "amount": 4.50}]}
      ]}'

# Breaks the total -> 409, nothing saved
curl -s -X PATCH localhost:8080/transactions/$TID/items -H 'Content-Type: application/json' \
  -d '{"operations": [{"op": "update", "item_id": "'${IDS[0]}'", "amount": 9.99}]}'
```

```json
{"error": {"code": "ITEMS_DO_NOT_RECONCILE",
  "message": "edited line items do not reconcile with the transaction total and stored taxes; nothing was saved",
  "details": {"issues": [{"code": "TOTAL_MISMATCH", "items_total": 18.89, "added_taxes": 2.85,
                          "computed_total": 21.74, "grand_total": 17.85, "difference": -3.89}],
              "proposed_line_items": [...]}}}
```

The status codes are `200` when saved (itemize status becomes `COMPLETE`), `409` when the items don't reconcile, `412` when `If-Match` is stale, `422` for an invalid operation such as an unknown `item_id`, `400` for malformed JSON or unknown fields, and `404` for an unknown transaction.

A tax-only receipt can be resolved by the user, for example:
`{"operations": [{"op": "add", "description": "Trip fare", "amount": 24.00}]}` moves it from `NEEDS_REVIEW` to `COMPLETE`.

## Behaviour on the fixtures

| Fixture | Taxes | Items | Status | Why |
|---|---|---|---|---|
| `receipt-clean` | VAT 19%, 2.85, added | Espresso 3.50, Sandwich 8.90, Mineral water 2.60 | `COMPLETE` | 15.00 + 2.85 = 17.85 |
| `receipt-tax-only` | VAT 19%, 3.83, **inclusive** | none | `NEEDS_REVIEW` (`NO_ITEMS`) | "Trip fare" has no price, so it is not a reliable item |
| `receipt-mismatch` | VAT 19%, 1.90, added | Water 4.00, Snacks 6.00 | `NEEDS_REVIEW` (`TOTAL_MISMATCH`) | 10.00 + 1.90 = 11.90 ≠ 18.50; no balancing line is added |

These results are asserted against `gold.json` in `internal/extract/extract_test.go` (parser level) and `internal/api/api_test.go` (over HTTP).

## Repository layer

The service depends only on `repository.Repository`, an interface grouping `Receipts`, `OCRResults` and `Transactions`. It never imports a database package. Two implementations ship:

- `repository/sqlite`, the default, with foreign keys, a UNIQUE constraint, CHECK constraints and a SQL transaction per write.
- `repository/memory`, a mutex-guarded in-memory store (`make run-memory`). It proves the boundary is real, and it's handy for tests.

`repository/repotest` is the **contract suite**: 12 tests covering round trips, one transaction per receipt, in-place re-process, optimistic locking, atomic claim, crash-recovery ordering, isolation of returned values and concurrent writers. Both implementations run it, and `make test` also runs the whole HTTP suite against each backend. To add Postgres or DynamoDB, write a package that passes `repotest.Run` and add a case to `openRepository` in `cmd/server`. Nothing else changes.

Transaction is treated as an aggregate: its header, taxes and items are always read and written together (`SaveProcessed`, `ReplaceItems`), so a backend can't leave half a transaction behind.

## Async processing

With the stub, OCR is instant, so sync is the default and the brief's curls return the transaction directly. A real OCR or vision-model call takes seconds and sometimes fails, so `PROCESS_MODE=async` moves it off the request path:

- `/process` atomically marks the receipt `PROCESSING` in the database and hands its ID to a buffered channel. A **fixed pool** of `OCR_WORKERS` goroutines reads the channel, which caps concurrent vendor calls.
- Each attempt runs with a timeout. Transient errors are retried with exponential backoff; permanent ones (the file has no readable text) fail at once and mark the receipt `OCR_FAILED`.
- **The database is the durable queue, and the channel is only a hand-off.** On startup, every receipt still in `PROCESSING` (left by a crash or an unfinished shutdown) is re-queued.
- **The save is idempotent** (an upsert on `receipt_id`), so a job that runs twice still yields one transaction. A duplicate `/process` call doesn't queue a second job, because claiming the receipt is a single conditional update.
- **Backpressure:** a full queue returns `503` with `Retry-After` instead of blocking or starting unbounded goroutines.
- **Graceful shutdown:** HTTP stops first, then workers finish the jobs they are running within the deadline. Queued jobs stay `PROCESSING` and resume on the next start.

This scales within one process. With several API replicas, each would have its own in-memory channel, so the next step is an external queue (SQS or Kafka) with separate worker processes. The worker code doesn't change, because it already assumes at-least-once delivery.

## Design patterns

Each pattern is here because it solves a problem in this code, not for show.

| Pattern | Where | What it buys |
|---|---|---|
| Repository | `internal/repository` | The service never sees a database; backends are swappable and share one contract suite |
| Strategy | `ocr.Engine`, `repository.Repository` | Stub OCR today, a vendor or VLM tomorrow; SQLite or memory chosen at start |
| Decorator | `ocr.Chain` with `WithLogging`, `WithCache`, `WithTimeout` (`internal/ocr/middleware.go`) | Cross-cutting concerns around the vendor call without touching the engine or the service |
| Command | PATCH operations (`internal/itemize/ops.go`) | Each op is its own type with `Execute`; a registry decodes them, so a new op is a new type, not a longer switch |
| Factory | `openRepository` in `cmd/server` | The only place that knows which database is used |
| Adapter | `Service.WorkerHandler()` | Plugs service use cases into the generic `worker.Pool` without the pool importing the service |
| Producer–consumer / worker pool | `internal/worker` | Bounded concurrency, backpressure, retries |
| Chain of responsibility | HTTP middleware (`internal/api/middleware.go`) | Request ID, panic recovery and access log wrap every handler |
| Value object | `domain.Money`, `domain.Rate` | Exact integer arithmetic; parsing and JSON in one place |
| Optimistic locking | `version` column, `ETag`/`If-Match` | No lost updates without holding locks |

Considered and left out: a state machine for receipt status (almost every transition is legal, since a receipt can be re-processed from any state, so a table would add code and catch nothing), and Specification objects for reconciliation (three short rules read better as one function).

## Decisions and assumptions

- **Items are net.** An item amount excludes added-on tax, which matches `gold.json`. The reconciliation rule is `sum(items) + sum(non-inclusive taxes) == grand_total` with a tolerance of 1 cent. Inclusive taxes such as "incl. VAT" are already inside the prices, so they are not added.
- **The tax-only receipt gets no fallback item.** Gold allows one item equal to the total, but an invented line is exactly what the brief warns against. It stays `NEEDS_REVIEW` with empty items, and the user adds the line through PATCH.
- **The printed subtotal is checked at process time** (`SUBTOTAL_MISMATCH`) to catch OCR that dropped a line. It is not enforced on user edits, because the grand total is the invariant there.
- **Re-itemize replaces user edits.** The brief says "replace line items". Every item carries `source` (`AUTO` or `USER`), so a client can warn before calling it.
- **Money is int64 cents and rates are int64 basis points.** No floats touch amounts, and anything finer than a cent is rejected rather than rounded. Two-decimal currencies are assumed.
- **Dates** accept ISO and day-first `DD.MM.YYYY`. Ambiguous slash dates are left `null` rather than guessed.

## Layout

```
cmd/server          wiring, config, graceful shutdown
internal/domain     Money, Rate, Receipt, Transaction, TaxLine, LineItem, statuses
internal/ocr        Engine interface, StubEngine, decorators (timeout, cache, logging)
internal/extract    OCR text -> header, taxes, proposed items (pure)
internal/itemize    Reconcile rules + PATCH operations as commands (pure)
internal/service    use cases: upload, process, re-itemize, patch
internal/repository persistence contract (interfaces + errors) the service depends on
  ├─ sqlite         SQLite implementation (schema.sql embedded), transactional writes
  ├─ memory         in-memory implementation
  └─ repotest       contract test suite every implementation must pass
internal/worker     bounded goroutine pool: retries, timeouts, graceful shutdown
internal/api        HTTP handlers, error mapping, middleware
fixtures/task-a     brief fixtures + gold.json
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for the data model, the tradeoffs, and what would change for production.
