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

## Repository layer

The service depends only on `repository.Repository`, an interface grouping `Receipts`, `OCRResults` and `Transactions`. It never imports a database package. Two implementations ship:

- `repository/sqlite`, the default, with foreign keys, a UNIQUE constraint, CHECK constraints and a SQL transaction per write.
- `repository/memory`, a mutex-guarded in-memory store (`make run-memory`). It proves the boundary is real, and it's handy for tests.

`repository/repotest` is the **contract suite**: 15 tests covering round trips, one transaction per receipt, in-place re-process, optimistic locking, atomic claim, crash-recovery ordering, sub-second timestamp ordering, reads during concurrent writes (no torn aggregates), transaction-ID collisions, isolation of returned values and concurrent writers. Both implementations run it, and `make test` also runs the whole HTTP suite against each backend. To add Postgres or DynamoDB, write a package that passes `repotest.Run` and add a case to `openRepository` in `cmd/server`. Nothing else changes.

Transaction is treated as an aggregate: its header, taxes and items are always read and written together (`SaveProcessed`, `ReplaceItems`), so a backend can't leave half a transaction behind.

## Async processing

With the stub, OCR is instant, so sync is the default and the brief's curls return the transaction directly. A real OCR or vision-model call takes seconds and sometimes fails, so `PROCESS_MODE=async` moves it off the request path:

- `/process` atomically marks the receipt `PROCESSING` in the database and hands its ID to a buffered channel. A **fixed pool** of `OCR_WORKERS` goroutines reads the channel, which caps concurrent vendor calls.
- Each attempt runs with a timeout. Transient errors are retried with exponential backoff; permanent ones (the file has no readable text) fail at once and mark the receipt `OCR_FAILED`.
- **The database is the durable queue, and the channel is only a hand-off.** On startup, every receipt still in `PROCESSING` (left by a crash or an unfinished shutdown) is re-queued.
- **The save is idempotent** (an upsert on `receipt_id`), so a job that runs twice still yields one transaction. A duplicate `/process` call doesn't queue a second job, because claiming the receipt is a conditional `UPDATE … WHERE status <> 'PROCESSING'` (exactly one caller sees a row change, on any database). If recording a failed job itself fails (the database is down), the receipt stays `PROCESSING` until the next start, when recovery re-queues it. Claims have no lease on purpose: an expiring claim needs a fencing token carried with the job, or a stale job can overwrite newer results (a production follow-up).
- **Backpressure:** a full queue returns `503` with `Retry-After` instead of blocking or starting unbounded goroutines.
- **Graceful shutdown:** HTTP stops first, then workers finish the jobs they are running within the deadline. Queued jobs stay `PROCESSING` and resume on the next start.

This scales within one process. With several API replicas, each would have its own in-memory channel, so the next step is an external queue (SQS or Kafka) with separate worker processes. The worker code doesn't change, because it already assumes at-least-once delivery.

## Design patterns

Each pattern is here because it solves a problem in this code, not for show.

| Pattern | Where | What it buys |
|---|---|---|
| Repository | `internal/repository` | The service never sees a database; backends are swappable and share one contract suite |
| Strategy (interfaces chosen at startup) | `ocr.Engine`, `repository.Repository`, `service.FileStore`; picked in `cmd/server` (`openRepository`) | Stub OCR today, a vendor or VLM tomorrow; SQLite or memory; local disk or S3 |
| Decorator | `ocr.Chain` with `WithLogging`, `WithCache`, `WithTimeout` (`internal/ocr/middleware.go`) | Cross-cutting concerns around the vendor call without touching the engine or the service |
| Command | PATCH operations (`internal/itemize/ops.go`) | Each op is its own type with `Execute` and its own accepted fields; a registry maps the wire name to it, so a new op is a new type, not a longer switch. The ops share one wire struct for decoding, so this is a light version of the pattern |
| Adapter | `jobHandler` in `cmd/server` | Plugs `Service.RunJob`/`FailJob` into the generic `worker.Pool`; neither package imports the other |
| Producer–consumer / worker pool | `internal/worker` | Bounded concurrency, backpressure, retries |
| Chain of responsibility | HTTP middleware (`internal/api/middleware.go`) | Request ID, access log and panic recovery wrap every handler; the access log sits outside recovery so a panicking request is still logged |
| Value object | `domain.Money`, `domain.Rate` | Exact integer arithmetic; parsing and JSON in one place |
| Optimistic locking | `version` column, `ETag`/`If-Match` | No lost updates without holding locks |

Considered and left out: a state machine for receipt status (almost every transition is legal, since a receipt can be re-processed from any state, so a table would add code and catch nothing), and Specification objects for reconciliation (three short rules read better as one function).

## Decisions and assumptions

- **Items are net.** An item amount excludes added-on tax, which matches `gold.json`. The reconciliation rule is `sum(items) + sum(non-inclusive taxes) == grand_total` with a tolerance of 1 cent. Inclusive taxes such as "incl. VAT" are already inside the prices, so they are not added.
- **The tax-only receipt gets no fallback item.** Gold allows one item equal to the total, but an invented line is exactly what the brief warns against. It stays `NEEDS_REVIEW` with empty items, and the user adds the line through PATCH.
- **The printed subtotal is checked at process time** (`SUBTOTAL_MISMATCH`) to catch OCR that dropped a line. It is not enforced on user edits, because the grand total is the invariant there.
- **Re-itemize replaces user edits.** The brief says "replace line items". Every item carries `source` (`AUTO` or `USER`), so a client can warn before calling it, and can send `If-Match` so it never clobbers an edit it has not seen.
- **Re-processing rebuilds the transaction too**, from a fresh OCR run, so it also replaces edited items. That is the point of re-processing (the OCR input changed), and the old OCR row is kept. A production version would require `If-Match` on `/process` once a transaction has `USER` items; it is left out here to keep `/process` a plain retryable command. If a re-process fails permanently, the receipt shows `OCR_FAILED` while its previous transaction stays readable and unchanged.
- **No floats anywhere.** Money is int64 cents; anything finer than a cent is rejected rather than rounded, and two-decimal currencies are assumed (0- and 3-decimal currencies such as JPY and KWD would take their exponent from ISO 4217). Tax rates and quantities are exact decimals (value and power of ten), so 19.123% or 1.125 kg are kept as printed. Instants are stored as epoch milliseconds (UTC) and returned as ISO-8601; the receipt date is a plain `YYYY-MM-DD`. SQLite tables are `STRICT`.
- **Dates** accept ISO and day-first `DD.MM.YYYY`. Ambiguous slash dates are left `null` rather than guessed.
- **Beyond the fixtures, the parser also handles** tax lines written "Total VAT" or "19% VAT", German summary and payment lines (Summe, Zwischensumme, MwSt, Bar, Rückgeld), and tip or rounding lines, which count toward the total but not the printed subtotal. Amount sums are overflow-checked.

## Layout

```
cmd/server          wiring, config, graceful shutdown
internal/domain     Money, Rate, Receipt, Transaction, TaxLine, LineItem, statuses
internal/ocr        Engine interface, StubEngine, decorators (timeout, cache, logging)
internal/extract    OCR text -> header, taxes, proposed items (pure)
internal/itemize    Reconcile rules + PATCH operations as commands (pure)
internal/service    use cases: upload, process, re-itemize, patch (no HTTP, SQL, disk or pool imports)
internal/storage    FileStore implementation: local disk (S3 would sit beside it)
internal/id         prefixed random IDs
internal/reqid      request ID in the context + slog handler that stamps it on every log line
internal/repository persistence contract (interfaces + errors) the service depends on
  ├─ sqlite         SQLite implementation, transactional writes, versioned migrations (migrations/*.sql)
  ├─ memory         in-memory implementation
  └─ repotest       contract test suite every implementation must pass
internal/worker     bounded goroutine pool: retries, timeouts, graceful shutdown
internal/api        HTTP handlers, error mapping, middleware; depends on the api.Service interface
fixtures/task-a     brief fixtures + gold.json
docs                architecture.pdf (for review) and its HTML source
```

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
