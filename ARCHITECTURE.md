# Architecture

A fuller version with diagrams, failure handling and the testing strategy is in [docs/architecture.pdf](docs/architecture.pdf).

## Layers

```
HTTP (internal/api) ──> api.Service (interface) ◁── service (use cases) ──> repository (interface) ──> sqlite | memory
                                                     ▲       │
  worker.Pool (async) ── adapter in cmd/server ──────┘       ├──> FileStore         (storage.Disk; S3 later)
  bounded goroutines                                         ├──> ocr.Engine        (logging -> cache -> timeout -> stub; vendor later)
                                                             ├──> extract           (pure: text -> header, taxes, items)
                                                             └──> itemize           (pure: reconcile rules, PATCH operations)
```

The domain rules live in `extract` and `itemize`. Both are pure functions with no I/O, clock or IDs, so they are tested directly against `gold.json`. The service layer orchestrates, and it depends on the `repository.Repository` interface, not a database. Implementations own atomicity and must pass the shared `repotest` contract suite. The HTTP layer only decodes requests and maps errors, and it depends on a small `api.Service` interface declared where it is used, not on the concrete service. The service imports no infrastructure: uploaded bytes go through a `FileStore` interface (local disk today), OCR engines receive bytes rather than a path, and the service does not know the worker pool exists. `cmd/server` is the composition root that wires the pool to `Service.RunJob`/`FailJob`.

`repository` holds its interfaces in its own package rather than in `service`, which is a deliberate exception to "declare interfaces where they are consumed": the contract has two implementations and a shared test suite (`repotest`), and all three need to import it without importing the service.

Two extension points are built as patterns so they grow without edits to the core. The OCR engine is wrapped by decorators (`ocr.Chain`): logging, an LRU cache keyed by file hash, and a timeout. A real vendor engine drops in underneath and inherits all three, and a circuit breaker or rate limiter would be one more decorator. PATCH operations are commands: each op type implements `Execute` on a working copy, and a registry maps the wire name to the type, so `Apply` stays a short loop that keeps the all-or-nothing guarantee.

## Data model

```
receipts 1 ──< ocr_results           every OCR run is kept (engine, raw_text)
receipts 1 ── 1 transactions         UNIQUE(receipt_id): one transaction per receipt
                 │  ocr_result_id -> the OCR run it was built from
                 ├──< tax_lines      name, rate_value + rate_scale, amount_cents, inclusive, jurisdiction
                 └──< line_items     description, amount_cents, quantity_value + quantity_scale, tax_amount_cents, source AUTO|USER
```

- **Taxes are rows, not a header field.** A receipt can carry several rates, such as 7% food and 19% drinks in Germany. The `inclusive` flag separates "VAT 19%" added on top of the items from "incl. VAT 19%" already inside the total. Without it, the taxi receipt could never reconcile.
- **Every column has an exact type, and tables are `STRICT`** (SQLite rejects a wrongly typed value). Money is `INTEGER` cents, so reconciliation is exact. Instants (`created_at`, `updated_at`, `processed_at`) are `INTEGER` Unix epoch milliseconds, UTC; the API still returns ISO-8601. `txn_date` stays `TEXT 'YYYY-MM-DD'`, because a printed calendar date has no time or zone. Tax rates and quantities are exact decimals stored as a value and a power of ten (`rate% = rate_value / 10^rate_scale`, so 19.123% is `(19123, 3)`); nothing is `REAL`. CHECK constraints guard the rest (ISO currency, date format, version ≥ 1, valid JSON issues, decimal pairs set together).
- **Raw OCR is the source of truth.** Re-itemize reads the OCR row the transaction points at. Re-processing adds a new OCR row and moves the pointer, so the history is kept.
- **`itemize_issues`** is a JSON list on the transaction that explains `NEEDS_REVIEW` or `FAILED` with the numbers (`TOTAL_MISMATCH`, `SUBTOTAL_MISMATCH`, `NO_ITEMS`, `NO_TOTAL`), so a review UI can say exactly what is wrong.
- **`version`** increases on every write and is exposed as the `ETag`. It backs optimistic concurrency.

## Invariants and where they are enforced

| Invariant | Where |
|---|---|
| One transaction per receipt, even with concurrent `process` calls | `UNIQUE(receipt_id)` plus `INSERT … ON CONFLICT DO UPDATE` |
| Process is atomic (OCR row, header, taxes, items) | `Repository.SaveProcessed` (one SQL transaction in sqlite; one critical section in memory) |
| Re-itemize and PATCH change items only, never header or taxes | `Repository.ReplaceItems` only touches `line_items` and the itemize status |
| The total is never adjusted and no balancing line is invented | `itemize.Reconcile` only reports; PATCH returns 409 and writes nothing |
| A crash never loses an accepted async job | `PROCESSING` is written before the hand-off; recovery re-queues on start |
| Duplicate async `/process` calls run OCR once | a conditional `UPDATE … WHERE status <> 'PROCESSING'` (`MarkProcessing`). Sync mode does not claim, so two concurrent sync calls both run OCR (the save is still one transaction) |
| A schema change reaches existing databases | ordered, embedded migrations tracked in `PRAGMA user_version`; a database newer than the binary is refused |
| No lost updates | `UPDATE … WHERE version = ?`; a stale `If-Match` returns 412, and a lost race without one returns 409 |
| File type is trusted from the bytes, not the header | `http.DetectContentType` together with an allow-list |

## Tradeoffs made for scope

- **Processing is sync by default and async on request.** `PROCESS_MODE=async` returns `202` and runs OCR on a bounded goroutine pool. It includes per-attempt timeouts, retries with backoff for transient errors, and `503` backpressure when the queue is full. The database is the durable queue: receipts are marked `PROCESSING` before they are handed to the channel, and startup recovery re-queues any left behind. The pool is per process, so with several replicas the channel becomes SQS or Kafka and the workers become their own deployment. The job code stays the same because it is already idempotent.
- **Local disk and SQLite.** One connection serialises writes, and it serialises reads too: a slow read waits behind OCR-triggered writes. That is the ceiling of this backend, and it is fine for a single-process take-home. Timestamps are integers, so `ORDER BY created_at` is numeric and exact, and a transaction (header, taxes, items) is read inside one SQL transaction so its version always matches its items. Production would use object storage (S3 with content-addressed keys) and Postgres. That means a new `repository` implementation that passes `repotest`, plus one line in `cmd/server`. The service and HTTP layers don't change.
- **A regex parser over labelled text.** It is deliberately simple and deterministic. A real pipeline would put a VLM that returns structured JSON with a confidence score per field behind `ocr.Engine`. Low confidence would map to `NEEDS_REVIEW` just like a total mismatch does today. The reconciliation rules would stay the same and act as the guardrail on model output.
- **Re-itemize overwrites user edits** because the brief asks for that. Items carry `source`, so a future `?preserve_user_edits=true` would be simple to add.

## Production follow-ups

- Allocate tax per line (`tax_amount`) with largest-remainder rounding so line taxes sum exactly to the tax rows, and support a separate rate per line.
- Store currency exponents (ISO 4217) for zero-decimal and three-decimal currencies such as JPY and KWD, and store the tax jurisdiction from the merchant's country.
- Add auth and tenancy (`user_id` on every row), an audit log of item edits, and duplicate detection beyond identical bytes (same merchant, date and total).
- Add metrics (latency and error rate, NEEDS_REVIEW rate per OCR engine) and tracing using the request ID that is already propagated.
