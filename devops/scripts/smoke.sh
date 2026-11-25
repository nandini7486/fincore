#!/usr/bin/env bash
# End-to-end smoke test: builds the real binaries, creates a fresh database,
# starts the API and the worker, exercises every endpoint over HTTP
# (including notifications delivered by the worker), then reconciles.
#
# It is the gate before starting a new phase: everything built so far must
# still work through the real server, not only in unit tests.
#
# Usage:
#   FINCORE_ADMIN_DATABASE_URL=postgres://user:pass@localhost:5432/postgres devops/scripts/smoke.sh
#
# The admin URL must allow CREATE DATABASE. A throwaway database is created
# and dropped. Requires: go, curl, jq, uuidgen.
set -euo pipefail

: "${FINCORE_ADMIN_DATABASE_URL:?set FINCORE_ADMIN_DATABASE_URL (a server where you may CREATE DATABASE)}"
for tool in go curl jq uuidgen psql; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 2; }
done

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d)
DB="fincore_smoke_$(date +%s)_$$"
PORT=${FINCORE_SMOKE_PORT:-18181}
BASE="http://127.0.0.1:$PORT"
API_PID=""
WORKER_PID=""

# Swap the database name in the admin URL.
DATABASE_URL=$(python3 -c 'import sys,urllib.parse as u; p=u.urlsplit(sys.argv[1]); print(u.urlunsplit(p._replace(path="/"+sys.argv[2])))' "$FINCORE_ADMIN_DATABASE_URL" "$DB")
export DATABASE_URL HTTP_ADDR="127.0.0.1:$PORT"
# Metrics are covered by compose-check.sh; use free ports here so a busy
# 9090/9091 on this machine cannot break the smoke test.
export METRICS_ADDR="127.0.0.1:0"

cleanup() {
  [ -n "$API_PID" ] && kill "$API_PID" 2>/dev/null && wait "$API_PID" 2>/dev/null || true
  [ -n "$WORKER_PID" ] && kill "$WORKER_PID" 2>/dev/null && wait "$WORKER_PID" 2>/dev/null || true
  psql "$FINCORE_ADMIN_DATABASE_URL" -qc "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); printf '  ok    %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL  %s\n' "$1"; }

# req METHOD PATH TOKEN [BODY] [IDEMPOTENCY_KEY] -> sets CODE, BODY, HDRS
req() {
  local method=$1 path=$2 token=$3 body=${4:-} key=${5:-}
  local args=(-s -X "$method" -o "$WORK/body" -D "$WORK/hdrs" -w '%{http_code}')
  [ -n "$token" ] && args+=(-H "Authorization: Bearer $token")
  [ -n "$key" ] && args+=(-H "Idempotency-Key: $key")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  CODE=$(curl "${args[@]}" "$BASE$path")
  BODY=$(cat "$WORK/body")
  HDRS=$(tr -d '\r' <"$WORK/hdrs")
}

# expect DESCRIPTION WANT_CODE [JQ_FILTER EXPECTED]
expect() {
  local desc=$1 want=$2
  if [ "$CODE" != "$want" ]; then
    fail "$desc (HTTP $CODE, want $want): $BODY"
    return
  fi
  if [ $# -ge 4 ]; then
    local got
    got=$(jq -r "$3" <<<"$BODY" 2>/dev/null || echo "<invalid json>")
    if [ "$got" != "$4" ]; then
      fail "$desc ($3 = '$got', want '$4')"
      return
    fi
  fi
  pass "$desc"
}

echo "== build"
(cd "$ROOT/backend" && go build -o "$WORK/api" ./cmd/api && go build -o "$WORK/migrate" ./cmd/migrate && go build -o "$WORK/fincorectl" ./cmd/fincorectl && go build -o "$WORK/worker" ./cmd/worker)

echo "== fresh database $DB"
psql "$FINCORE_ADMIN_DATABASE_URL" -qc "CREATE DATABASE $DB" >/dev/null
"$WORK/migrate" up >/dev/null

token() { "$WORK/fincorectl" user create -name "$1" -email "$2" -role "$3" | sed -n 's/^API token: //p'; }
ADMIN=$(token Admin admin@smoke.test ADMIN)
ALICE=$(token Alice alice@smoke.test USER)
BOB=$(token Bob bob@smoke.test USER)
MALLORY=$(token Mallory mallory@smoke.test USER)

echo "== start API on :$PORT"
"$WORK/api" >"$WORK/api.log" 2>&1 &
API_PID=$!
for _ in $(seq 1 50); do curl -sf "$BASE/readyz" >/dev/null && break; sleep 0.1; done

echo "== start worker"
WORKER_POLL_INTERVAL=200ms "$WORK/worker" >"$WORK/worker.log" 2>&1 &
WORKER_PID=$!

echo "== health"
req GET /healthz ""; expect "GET /healthz" 200 .status ok
req GET /readyz ""; expect "GET /readyz" 200 .status ready

echo "== authentication"
req GET /api/v1/me ""; expect "no token -> 401" 401 .error.code UNAUTHENTICATED
req GET /api/v1/me "fct_forged"; expect "forged token -> 401" 401
req GET /api/v1/me "$ALICE"; expect "valid token -> 200" 200 .email alice@smoke.test

echo "== accounts"
req POST /api/v1/accounts "$ALICE"; expect "create account" 201 .balance 0.00
A=$(jq -r .account_id <<<"$BODY")
req POST /api/v1/accounts "$BOB" '{"currency":"INR"}'; expect "create account with currency" 201 .currency INR
B=$(jq -r .account_id <<<"$BODY")
req POST /api/v1/accounts "$ALICE" '{"currency":"USD"}'; expect "unsupported currency -> 400" 400 .error.code UNSUPPORTED_CURRENCY
req POST /api/v1/accounts "$ALICE" '{"balance":"1000000.00"}'; expect "client-chosen balance rejected" 400 .error.code INVALID_REQUEST
req GET "/api/v1/accounts/$A" "$ALICE"; expect "owner reads account" 200 .account_id "$A"
req GET "/api/v1/accounts/$A" "$MALLORY"; expect "other user -> 404" 404 .error.code ACCOUNT_NOT_FOUND

echo "== top-ups"
req POST "/api/v1/accounts/$A/topups" "$ADMIN" '{"amount":"1000.00","currency":"INR"}' "$(uuidgen)"; expect "admin top-up" 201 .type TOPUP
req POST "/api/v1/accounts/$A/topups" "$ALICE" '{"amount":"1.00","currency":"INR"}' "$(uuidgen)"; expect "non-admin top-up -> 403" 403 .error.code FORBIDDEN

echo "== payments"
pay() { printf '{"sender_account_id":"%s","receiver_account_id":"%s","amount":"%s","currency":"INR"}' "$1" "$2" "$3"; }
req POST /api/v1/payments "$ALICE" "$(pay "$A" "$B" 250.50)" "$(uuidgen)"; expect "transfer" 201 .amount 250.50
P1=$(jq -r .payment_id <<<"$BODY")
req POST /api/v1/payments "$ALICE" "$(pay "$A" "$B" 5000.00)" "$(uuidgen)"; expect "insufficient funds -> 422" 422 .error.code INSUFFICIENT_FUNDS
req POST /api/v1/payments "$MALLORY" "$(pay "$A" "$B" 1.00)" "$(uuidgen)"; expect "spending someone else's account -> 404" 404
req POST /api/v1/payments "$ALICE" "$(pay "$A" "$B" 1.005)" "$(uuidgen)"; expect "three decimals -> 400" 400 .error.code INVALID_AMOUNT
req POST /api/v1/payments "$ALICE" "$(pay "$A" "$B" 1.00)"; expect "missing Idempotency-Key -> 400" 400 .error.code IDEMPOTENCY_KEY_REQUIRED

echo "== idempotency"
KEY=$(uuidgen)
for i in 1 2 3 4 5; do
  curl -s -o "$WORK/dup$i" -D "$WORK/duph$i" -X POST "$BASE/api/v1/payments" \
    -H "Authorization: Bearer $ALICE" -H "Idempotency-Key: $KEY" -H 'Content-Type: application/json' \
    -d "$(pay "$A" "$B" 10.00)" &
  DUP_PIDS[$i]=$!
done
for i in 1 2 3 4 5; do wait "${DUP_PIDS[$i]}"; done
ids=$(for i in 1 2 3 4 5; do jq -r .payment_id "$WORK/dup$i"; done | sort -u | wc -l)
fresh=$(for i in 1 2 3 4 5; do grep -qi '^Idempotent-Replayed: true' "$WORK/duph$i" || echo x; done | wc -l)
if [ "$ids" = 1 ] && [ "$fresh" = 1 ]; then pass "5 simultaneous duplicates -> 1 payment, 4 replays"; else fail "duplicates: $ids distinct payments, $fresh processed"; fi
req POST /api/v1/payments "$ALICE" "$(pay "$A" "$B" 20.00)" "$KEY"; expect "same key, different amount -> 409" 409 .error.code IDEMPOTENCY_KEY_REUSED
req GET "/api/v1/accounts/$A" "$ALICE"; expect "alice charged once: 1000 - 250.50 - 10" 200 .balance 739.50

echo "== reading payments"
req GET "/api/v1/payments/$P1" "$BOB"; expect "receiver reads payment" 200 .payment_id "$P1"
req GET "/api/v1/payments/$P1" "$MALLORY"; expect "stranger -> 404" 404 .error.code PAYMENT_NOT_FOUND
req GET "/api/v1/accounts/$A/payments?limit=2" "$ALICE"; expect "history page 1" 200 '.payments | length' 2
CURSOR=$(jq -r .next_cursor <<<"$BODY")
SEEN=$(jq -r '.payments[].payment_id' <<<"$BODY")
while [ "$CURSOR" != "null" ]; do
  req GET "/api/v1/accounts/$A/payments?limit=2&cursor=$CURSOR" "$ALICE"
  [ "$CODE" = 200 ] || { fail "history next page: HTTP $CODE"; break; }
  SEEN+=$'\n'$(jq -r '.payments[].payment_id' <<<"$BODY")
  CURSOR=$(jq -r .next_cursor <<<"$BODY")
done
total=$(grep -c . <<<"$SEEN"); unique=$(sort -u <<<"$SEEN" | grep -c .)
# top-up, transfer, failed transfer, idempotent transfer
if [ "$total" = 4 ] && [ "$unique" = 4 ]; then pass "history: 4 payments across pages, no duplicates"; else fail "history: $total seen, $unique unique, want 4"; fi
req GET "/api/v1/accounts/$A/payments?cursor=tampered" "$ALICE"; expect "tampered cursor -> 400" 400 .error.code INVALID_CURSOR

echo "== notifications (delivered by the worker)"
# bob received 250.50 and 10.00 (the duplicate-tested payment, once); the
# failed 5000.00 payment produced none. alice's top-up produced one.
for _ in $(seq 1 50); do
  req GET /api/v1/notifications "$BOB"
  [ "$(jq '.notifications | length' <<<"$BODY")" = 2 ] && break
  sleep 0.1
done
expect "bob has 2 notifications (worker delivered them)" 200 '.notifications | length' 2
expect "newest: the idempotent 10.00 payment" 200 '.notifications[0].message' "You received ₹10.00 from Alice."
expect "older: the 250.50 payment" 200 '.notifications[1].payment_id' "$P1"
req GET /api/v1/notifications "$ALICE"; expect "alice: top-up notification" 200 '.notifications[0].message' "₹1,000.00 was added to your account."
req GET "/api/v1/notifications?limit=1" "$BOB"; expect "notifications page 1 of 2" 200 '.notifications | length' 1
req GET /api/v1/notifications ""; expect "notifications without token -> 401" 401
pending=$(psql "$DATABASE_URL" -Atc "SELECT count(*) FROM outbox_events WHERE processed_at IS NULL")
if [ "$pending" = 0 ]; then pass "no undelivered outbox events"; else fail "$pending outbox events still pending"; fi

echo "== reconciliation"
if "$WORK/fincorectl" reconcile >"$WORK/reconcile.txt"; then pass "fincorectl reconcile: books balance"; else fail "fincorectl reconcile: $(cat "$WORK/reconcile.txt")"; fi

echo "== hygiene"
if grep -q 'fct_' "$WORK/api.log" "$WORK/worker.log"; then fail "a token appeared in a server log"; else pass "no tokens in server or worker logs"; fi

echo
echo "smoke: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
