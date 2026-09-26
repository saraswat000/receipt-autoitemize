# Architecture

## Layers

```
HTTP (internal/api) ──> service (use cases) ──> store (SQLite)
                          ▲       │
  worker.Pool (async) ────┘       ├──> ocr.Engine        (stub today, vendor later)
  bounded goroutines              ├──> extract           (pure: text -> header, taxes, items)
                                  └──> itemize           (pure: reconcile rules, PATCH operations)
```

The domain rules live in `extract` and `itemize`. Both are pure functions with no I/O, clock or IDs, so they are tested directly against `gold.json`. The service layer orchestrates, the store owns atomicity, and the HTTP layer only decodes requests and maps errors.

## Data model

```
receipts 1 ──< ocr_results           every OCR run is kept (engine, raw_text)
receipts 1 ── 1 transactions         UNIQUE(receipt_id): one transaction per receipt
                 │  ocr_result_id -> the OCR run it was built from
                 ├──< tax_lines      name, rate_bp, amount_cents, inclusive, jurisdiction
                 └──< line_items     description, amount_cents, quantity, tax_amount_cents, source AUTO|USER
```

- **Taxes are rows, not a header field.** A receipt can carry several rates, such as 7% food and 19% drinks in Germany. The `inclusive` flag separates "VAT 19%" added on top of the items from "incl. VAT 19%" already inside the total. Without it, the taxi receipt could never reconcile.
- **Money is integer cents and rates are integer basis points**, so reconciliation is exact.
- **Raw OCR is the source of truth.** Re-itemize reads the OCR row the transaction points at. Re-processing adds a new OCR row and moves the pointer, so the history is kept.
- **`itemize_issues`** is a JSON list on the transaction that explains `NEEDS_REVIEW` or `FAILED` with the numbers (`TOTAL_MISMATCH`, `SUBTOTAL_MISMATCH`, `NO_ITEMS`, `NO_TOTAL`), so a review UI can say exactly what is wrong.
- **`version`** increases on every write and is exposed as the `ETag`. It backs optimistic concurrency.

## Invariants and where they are enforced

| Invariant | Where |
|---|---|
| One transaction per receipt, even with concurrent `process` calls | `UNIQUE(receipt_id)` plus `INSERT … ON CONFLICT DO UPDATE` |
| Process is atomic (OCR row, header, taxes, items) | a single SQL transaction in `store.SaveProcessed` |
| Re-itemize and PATCH change items only, never header or taxes | `store.ReplaceItems` only touches `line_items` and the itemize status |
| The total is never adjusted and no balancing line is invented | `itemize.Reconcile` only reports; PATCH returns 409 and writes nothing |
| A crash never loses an accepted async job | `PROCESSING` is written before the hand-off; recovery re-queues on start |
| Duplicate `/process` calls run OCR once | a conditional claim (`MarkProcessing`) inside one SQL transaction |
| No lost updates | `UPDATE … WHERE version = ?`, plus an optional `If-Match` that returns 412 |
| File type is trusted from the bytes, not the header | `http.DetectContentType` together with an allow-list |

## Tradeoffs made for scope

- **Processing is sync by default and async on request.** `PROCESS_MODE=async` returns `202` and runs OCR on a bounded goroutine pool. It includes per-attempt timeouts, retries with backoff for transient errors, and `503` backpressure when the queue is full. The database is the durable queue: receipts are marked `PROCESSING` before they are handed to the channel, and startup recovery re-queues any left behind. The pool is per process, so with several replicas the channel becomes SQS or Kafka and the workers become their own deployment. The job code stays the same because it is already idempotent.
- **Local disk and SQLite.** One connection serialises writes. Production would use object storage (S3 with content-addressed keys) and Postgres. The store package is the only code that would change.
- **A regex parser over labelled text.** It is deliberately simple and deterministic. A real pipeline would put a VLM that returns structured JSON with a confidence score per field behind `ocr.Engine`. Low confidence would map to `NEEDS_REVIEW` just like a total mismatch does today. The reconciliation rules would stay the same and act as the guardrail on model output.
- **Re-itemize overwrites user edits** because the brief asks for that. Items carry `source`, so a future `?preserve_user_edits=true` would be simple to add.

## Production follow-ups

- Allocate tax per line (`tax_amount`) with largest-remainder rounding so line taxes sum exactly to the tax rows, and support a separate rate per line.
- Store currency exponents (ISO 4217) for zero-decimal and three-decimal currencies such as JPY and KWD, and store the tax jurisdiction from the merchant's country.
- Add auth and tenancy (`user_id` on every row), an audit log of item edits, and duplicate detection beyond identical bytes (same merchant, date and total).
- Add metrics (latency and error rate, NEEDS_REVIEW rate per OCR engine) and tracing using the request ID that is already propagated.
