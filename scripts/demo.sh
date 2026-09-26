#!/usr/bin/env bash
# Walks every endpoint against a running server using the three fixtures.
# Usage: make run (in another terminal), then ./scripts/demo.sh [base_url]
set -euo pipefail
BASE=${1:-http://localhost:8080}
FIX="$(cd "$(dirname "$0")/.." && pwd)/fixtures/task-a"
command -v jq >/dev/null || { echo "jq is required"; exit 1; }

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

step "GET /health"
curl -sS "$BASE/health" | jq .

declare -A TXN
for name in receipt-clean receipt-tax-only receipt-mismatch; do
  step "POST /receipts ($name.txt)"
  RID=$(curl -sS -F "file=@$FIX/$name.txt" "$BASE/receipts" | tee /dev/stderr | jq -r .receipt_id)

  step "POST /receipts/$RID/process"
  OUT=$(curl -sS -X POST "$BASE/receipts/$RID/process")
  if [ "$(jq -r 'has("receipt_id") and (has("line_items") | not)' <<<"$OUT")" = true ]; then
    # Async mode: 202 with the receipt; poll until a worker has processed it.
    echo "202 Accepted, status $(jq -r .status <<<"$OUT"); polling GET /receipts/$RID ..." >&2
    until [ "$(curl -sS "$BASE/receipts/$RID" | jq -r .status)" != PROCESSING ]; do sleep 0.2; done
    OUT=$(curl -sS "$BASE/transactions/$(curl -sS "$BASE/receipts/$RID" | jq -r .transaction_id)")
  fi
  TXN[$name]=$(echo "$OUT" \
    | jq -c '{id, merchant, date, currency, grand_total, taxes: [.taxes[] | {name, rate, amount, inclusive}],
              line_items: [.line_items[] | {description, amount}], itemize_status, itemize_issues}' \
    | tee /dev/stderr | jq -r .id)
done

T=${TXN[receipt-clean]}
step "GET /transactions/$T"
curl -sS -i "$BASE/transactions/$T" | sed -n '1p;/^Etag/Ip'

ITEMS=($(curl -sS "$BASE/transactions/$T" | jq -r '.line_items[].id'))

step "PATCH items: merge espresso + water, split sandwich (reconciles -> 200)"
curl -sS -X PATCH "$BASE/transactions/$T/items" -H 'If-Match: "1"' -H 'Content-Type: application/json' -d '{
  "operations": [
    {"op": "merge", "item_ids": ["'"${ITEMS[0]}"'", "'"${ITEMS[2]}"'"], "description": "Drinks"},
    {"op": "split", "item_id": "'"${ITEMS[1]}"'", "into": [
      {"description": "Bread", "amount": 4.40}, {"description": "Filling", "amount": 4.50}]}
  ]}' | jq -c '{version, itemize_status, line_items: [.line_items[] | {description, amount, source}]}'

step "PATCH items: change an amount so totals break (-> 409, nothing saved)"
FIRST=$(curl -sS "$BASE/transactions/$T" | jq -r '.line_items[0].id')
curl -sS -w '\nHTTP %{http_code}\n' -X PATCH "$BASE/transactions/$T/items" -H 'Content-Type: application/json' \
  -d '{"operations": [{"op": "update", "item_id": "'"$FIRST"'", "amount": 9.99}]}'

step "PATCH with a stale If-Match (-> 412)"
curl -sS -o /dev/null -w 'HTTP %{http_code}\n' -X PATCH "$BASE/transactions/$T/items" -H 'If-Match: "1"' \
  -H 'Content-Type: application/json' -d '{"operations": [{"op": "update", "item_id": "'"$FIRST"'", "description": "x"}]}'

step "POST /transactions/$T/itemize (re-run from stored OCR; same transaction, items replaced)"
curl -sS -X POST "$BASE/transactions/$T/itemize" | jq -c '{id, version, itemize_status, line_items: [.line_items[] | {description, amount, source}]}'

T=${TXN[receipt-tax-only]}
step "PATCH tax-only receipt: add the fare the receipt implies (NEEDS_REVIEW -> COMPLETE)"
curl -sS -X PATCH "$BASE/transactions/$T/items" -H 'Content-Type: application/json' \
  -d '{"operations": [{"op": "add", "description": "Trip fare", "amount": 24.00}]}' | jq -c '{itemize_status, line_items: [.line_items[] | {description, amount}]}'
