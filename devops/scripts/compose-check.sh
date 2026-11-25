#!/usr/bin/env bash
# Deployment check: builds the Docker image, starts the full Compose stack
# (postgres -> migrate -> api + worker) and verifies it the way an operator
# would. smoke.sh proves the application logic; this proves the packaging:
# image, release-step migrations, least-privilege database login, internal
# metrics, healthchecks, graceful stop and restart.
#
# Usage: devops/scripts/compose-check.sh
# Requires: docker (with compose), curl, jq, uuidgen.
# Uses its own Compose project and ports, and deletes its data on exit.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
export COMPOSE_PROJECT_NAME=fincore-check
export POSTGRES_PORT=${CHECK_POSTGRES_PORT:-55499} API_PORT=${CHECK_API_PORT:-18280}
export API_METRICS_PORT=${CHECK_API_METRICS_PORT:-19290} WORKER_METRICS_PORT=${CHECK_WORKER_METRICS_PORT:-19291}
export FINCORE_VERSION=${FINCORE_VERSION:-compose-check}
BASE="http://127.0.0.1:$API_PORT"
dc() { docker compose -f "$ROOT/devops/docker-compose.yml" "$@"; }
WORK=$(mktemp -d)

cleanup() {
  if [ "$FAIL" -gt 0 ]; then dc logs --no-color --tail 50 api worker migrate >&2 || true; fi
  dc --profile tools down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
PASS=0
FAIL=0
trap cleanup EXIT
pass() { PASS=$((PASS + 1)); printf '  ok    %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL  %s\n' "$1"; }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }

# req METHOD PATH TOKEN [BODY] [IDEMPOTENCY_KEY] -> sets CODE, BODY
req() {
  local args=(-s -X "$1" -o "$WORK/body" -w '%{http_code}')
  [ -n "${3:-}" ] && args+=(-H "Authorization: Bearer $3")
  [ -n "${5:-}" ] && args+=(-H "Idempotency-Key: $5")
  [ -n "${4:-}" ] && args+=(-H 'Content-Type: application/json' -d "$4")
  CODE=$(curl "${args[@]}" "$BASE$2" || echo 000)
  BODY=$(cat "$WORK/body" 2>/dev/null || true)
}
# metric PORT SERIES -> the value of that exact series (comment lines skipped)
metric() { curl -s "http://127.0.0.1:$1/metrics" | grep -v '^#' | grep -F -- "$2 " | awk '{print $NF}' | head -1 || true; }
token() { dc run --rm -T fincorectl user create -name "$1" -email "$2" -role "$3" 2>/dev/null | sed -n 's/^API token: //p'; }

echo "== build and start ($FINCORE_VERSION)"
dc --profile tools down -v --remove-orphans >/dev/null 2>&1 || true
dc build --quiet
dc up -d --wait --wait-timeout 120 postgres api worker

echo "== containers"
check "migrate ran once and exited 0" '[ "$(docker inspect -f "{{.State.ExitCode}}" "$(dc ps -aq migrate)")" = 0 ]'
check "api runs as non-root" '[ "$(docker inspect -f "{{.Config.User}}" "$(dc ps -q api)")" = "nonroot:nonroot" ]'
check "api container healthy (built-in HEALTHCHECK)" '[ "$(docker inspect -f "{{.State.Health.Status}}" "$(dc ps -q api)")" = healthy ]'
check "worker container healthy" '[ "$(docker inspect -f "{{.State.Health.Status}}" "$(dc ps -q worker)")" = healthy ]'
check "image has no shell (distroless)" '! docker run --rm --entrypoint /bin/sh "fincore:$FINCORE_VERSION" -c true >/dev/null 2>&1'
check "migrate is idempotent on rerun" 'dc run --rm -T migrate >/dev/null 2>&1'

echo "== least-privilege database login"
# psql as the app login; never fails, so callers inspect the output (the
# expected outcome here is an error, and pipefail would hide a match).
DB_LOGIN=fincore_api
psql_app() { dc exec -T postgres psql -U "$DB_LOGIN" -d fincore -tAc "$1" 2>&1 || true; }
check "app login is not the table owner" '[ "$(psql_app "SELECT tableowner = current_user FROM pg_tables WHERE tablename = '"'"'payments'"'"'")" = f ]'
check "app login cannot DELETE ledger entries" 'psql_app "DELETE FROM ledger_entries" | grep -q "permission denied"'
check "app login cannot UPDATE audit log" 'psql_app "UPDATE audit_log SET action = action" | grep -q "permission denied"'
check "app login cannot DROP tables" 'psql_app "DROP TABLE payments" | grep -q "must be owner"'

echo "== API through the published port"
req GET /healthz ""; check "healthz 200" '[ "$CODE" = 200 ]'
req GET /readyz ""; check "readyz 200 (database reachable as app login)" '[ "$CODE" = 200 ]'
req GET /metrics ""; check "/metrics NOT on the public port" '[ "$CODE" = 404 ]'
req GET /api/v1/me ""; check "no token -> 401" '[ "$CODE" = 401 ]'

ADMIN=$(token Admin admin@compose.test ADMIN)
ALICE=$(token Alice alice@compose.test USER)
BOB=$(token Bob bob@compose.test USER)
check "fincorectl creates users as the app login" '[ -n "$ADMIN" ] && [ -n "$ALICE" ] && [ -n "$BOB" ]'

req POST /api/v1/accounts "$ALICE"; A=$(jq -r .account_id <<<"$BODY")
req POST /api/v1/accounts "$BOB"; B=$(jq -r .account_id <<<"$BODY")
req POST "/api/v1/accounts/$A/topups" "$ADMIN" '{"amount":"500.00","currency":"INR"}' "$(uuidgen)"
check "top-up 201" '[ "$CODE" = 201 ]'
KEY=$(uuidgen)
PAY=$(printf '{"sender_account_id":"%s","receiver_account_id":"%s","amount":"%s","currency":"INR"}' "$A" "$B" 120.00)
req POST /api/v1/payments "$ALICE" "$PAY" "$KEY"; check "payment 201" '[ "$CODE" = 201 ]'
req POST /api/v1/payments "$ALICE" "$PAY" "$KEY"; check "same key replays 201" '[ "$CODE" = 201 ]'
req GET "/api/v1/accounts/$A" "$ALICE"; check "charged once (500 - 120 = 380.00)" '[ "$(jq -r .balance <<<"$BODY")" = 380.00 ]'

for _ in $(seq 1 50); do
  req GET /api/v1/notifications "$BOB"
  [ "$(jq '.notifications | length' <<<"$BODY")" = 1 ] && break
  sleep 0.2
done
check "worker container delivered bob's notification" '[ "$(jq -r ".notifications[0].message" <<<"$BODY")" = "You received ₹120.00 from Alice." ]'

echo "== metrics (internal ports)"
check "api build_info carries the image version" '[ "$(metric "$API_METRICS_PORT" "fincore_build_info{component=\"api\",version=\"$FINCORE_VERSION\"}")" = 1 ]'
check "api counted 2 payments with 201" '[ "$(metric "$API_METRICS_PORT" "fincore_http_requests_total{method=\"POST\",route=\"/api/v1/payments\",status=\"201\"}")" = 2 ]'
check "api pool metrics present" '[ "$(metric "$API_METRICS_PORT" "fincore_db_pool_max_conns")" = 10 ]'
check "worker handled the transfer event" '[ "$(metric "$WORKER_METRICS_PORT" "fincore_outbox_events_handled_total{event_type=\"payment.succeeded\",result=\"ok\"}")" = 1 ]'
check "worker reports an empty backlog" '[ "$(metric "$WORKER_METRICS_PORT" "fincore_outbox_pending_events")" = 0 ]'

echo "== operations"
check "fincorectl reconcile: books balance" 'dc run --rm -T fincorectl reconcile >/dev/null 2>&1'
dc stop -t 15 api >/dev/null 2>&1
check "docker stop -> api exits 0 (graceful SIGTERM)" '[ "$(docker inspect -f "{{.State.ExitCode}}" "$(dc ps -aq api)")" = 0 ]'
dc start api >/dev/null 2>&1
for _ in $(seq 1 50); do req GET /readyz ""; [ "$CODE" = 200 ] && break; sleep 0.2; done
check "api restarts and is ready" '[ "$CODE" = 200 ]'
req GET "/api/v1/accounts/$A" "$ALICE"; check "data survived the restart" '[ "$(jq -r .balance <<<"$BODY")" = 380.00 ]'
check "no bearer token in api or worker logs" '! dc logs --no-color api worker 2>&1 | grep -q "fct_"'

echo
echo "compose-check: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
