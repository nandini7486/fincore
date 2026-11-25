# Testing FinCore

The money path is never mocked. Every database test runs against a real
PostgreSQL, in its own throwaway database, as the restricted `fincore_app`
role (the API's real privileges). CI runs everything on every push.

## How to run

```sh
cd backend
export FINCORE_TEST_DATABASE_URL=postgres://fincore:fincore_local_dev@localhost:5432/fincore?sslmode=disable
go vet ./...
go test -race ./...            # unit, integration, concurrency, failure tests

cd ..
FINCORE_ADMIN_DATABASE_URL=$FINCORE_TEST_DATABASE_URL scripts/smoke.sh   # end-to-end gate
```

## The gate before every new phase

Before work on a new phase starts, all of the following must pass on `main`:

1. `gofmt`, `go vet`, `go mod tidy -diff`
2. `go test -race ./...`
3. Migration round trip (`migrate reset`, `up`, `status`) on an empty database
4. `scripts/smoke.sh`: builds the real binaries, creates a fresh database,
   starts the server and exercises every endpoint over HTTP (auth, accounts,
   top-ups, payments, simultaneous duplicate requests, history pagination,
   notifications delivered by the real worker), then runs `fincorectl
   reconcile` and checks that no token reached the API or worker logs

CI runs 1-4 on every push and pull request.

## Test layers

| Layer | Where | What it proves |
|---|---|---|
| Unit | `money`, `idempotency`, `ids`, `payments` (error mapping), `auth` | Parsing, hashing, cursor and ID validation; fuzzed money round trip |
| Integration | every package with a `testdb` | Each rule against real PostgreSQL, constraints and grants |
| HTTP | `internal/httpapi` | Every endpoint, status code and response shape through the real router |
| Security | `httpapi/security_test.go` | Every registered route requires a bearer token; admin routes return 403 to users |
| Concurrency | `payments`, `httpapi/idempotency_test.go`, `httpapi/stress_test.go` | No overdraft, no deadlock, duplicates move money once |
| Failure | `httpapi/failure_test.go`, `payments` lock-timeout test | Nothing partial survives a failure; retries are safe |
| Outbox and worker | `outbox`, `notifications`, `payments/events_test.go` | Events exist iff money moved; delivery, retries, competing workers, crash redelivery, idempotent consumer |
| Process | `cmd/api/process_test.go`, `cmd/worker/process_test.go` | The real binaries killed or shut down mid-work behave correctly |
| Reconciliation | `reconcile`, plus after every payments test | The books balance; five corruption types are caught |
| End-to-end | `scripts/smoke.sh` | The real binaries work together |

## Failure scenarios

Each scenario is injected in the middle of a real payment. After it, the test
checks that no transfer, ledger entry, idempotency record or audit record
survived, the balance is unchanged and reconciliation passes. Then it retries
with the same `Idempotency-Key` and expects a fresh `201`, charged once.

| Scenario | How it is injected | Result |
|---|---|---|
| Audit write fails | `REVOKE INSERT ON audit_log` | `500`, everything rolled back |
| Ledger write fails | `REVOKE INSERT ON ledger_entries` | `500`, everything rolled back |
| Business-rule failure with the ledger broken | `REVOKE INSERT ON ledger_entries` | `422` still recorded (a FAILED payment needs no ledger) |
| Database connection lost mid-transaction | `pg_terminate_backend` while the payment waits on a lock | `500`; PostgreSQL rolls back; pool recovers |
| Client disconnects mid-payment | request context cancelled while waiting on a lock | `499` logged as a warning, not an error; rolled back |
| Lock held too long | another session holds the account row | `503` + `Retry-After` after ~2s; nothing committed |
| Rollback past the treasury seed with payments present | `migrate reset` | refused with a clear message; nothing changed |
| **Server process killed** (`kill -9`) mid-payment | real binary, `SIGKILL` while the payment waits on a lock | client gets a connection error; PostgreSQL rolls back (no `COMMIT` was sent); after restart the same key charges once |
| Worker dies mid-handler | `pg_terminate_backend` during the handler | handler's write and the processed mark both rolled back; no failure recorded; redelivered exactly once |
| Event redelivered | `processed_at` reset to NULL, twice | still exactly one notification (`UNIQUE (event_id)`) |
| Handler fails / panics / unknown event type | test handlers | writes rolled back via savepoint; `attempts`, `last_error`, backoff recorded; other events still delivered |
| **Graceful shutdown** (`SIGTERM`) mid-payment | real binary, `SIGTERM` while the payment waits on a lock | new connections refused at once; the in-flight payment commits (`201`); exit code 0 |

## Every status code is tested

| Status | Meaning | Tested in |
|---|---|---|
| `200` | Read succeeded | `accounts_test`, `history_test`, `router_test` |
| `201` | Created | `accounts_test`, `payments_test`, `idempotency_test` |
| `201` + `Idempotent-Replayed` | Replay of a completed request | `idempotency_test` |
| `400` | Invalid input or missing `Idempotency-Key` | `accounts_test`, `payments_test`, `history_test`, `idempotency_test` |
| `401` | Missing or invalid token | `router_test`, `security_test` (every route × 6 bad credentials) |
| `403` | Authenticated, wrong role | `payments_test`, `security_test` |
| `404` | Not found or not yours (same response) | `accounts_test`, `payments_test`, `history_test`, `router_test` |
| `405` | Wrong method | `router_test` |
| `409` | `Idempotency-Key` reused for a different request | `idempotency_test` |
| `422` | Business rule failed, stored as FAILED | `payments_test`, `idempotency_test` |
| `499` | Client disconnected (logged only) | `failure_test` |
| `500` | Unexpected failure, rolled back | `failure_test`, `middleware_test` (panic) |
| `503` | Lock or statement timeout, safe to retry | `idempotency_test`, `payments` (`ErrBusy`) |

## How we know the tests work

A test that cannot fail proves nothing. Key tests were checked by breaking the
code on purpose and confirming they fail (mutation testing):

| Deliberate break | Caught by |
|---|---|
| Remove `FOR UPDATE` from the account lock | concurrent overdraft test (the DB `CHECK` then also blocks it) |
| Credit the receiver 1 paisa too much | reconciliation after every payment test |
| Drop the `id` tie-breaker from the history cursor | timestamp-tie pagination test (returned 1 of 5) |
| Naive "check then insert" idempotency | deterministic in-flight duplicate test (10/10 runs) |
| Remove the auth middleware / add an unprotected route | route-walking security test |
| Drop the receiver-account match in reconciliation | "credit posted to the wrong account" test |
| Ignore a failed audit write | audit-failure rollback test (got `201`, want `500`) |
| `srv.Close()` instead of graceful `srv.Shutdown()` | graceful-shutdown test (in-flight payment cut off) |
| Commit the idempotency claim in its own transaction first | kill test (orphaned claim would block every retry) |
| Notification insert without `ON CONFLICT DO NOTHING` | redelivery test (consumer not idempotent) |
| No rollback of a failed handler's writes | retry test (the failed attempt's write survived) |
| Outbox event written for failed payments too | event test (an event for money that never moved) |
| Notifications query without `WHERE user_id = $1` | notifications endpoint test (users would see each other's notifications) |
| Notifications cursor without the `id` tie-breaker | notifications tie test (returned 1 of 5) |
| Worker query without `SKIP LOCKED` | competing-workers test (workers queue behind one lock instead of taking different events) |

One lesson is recorded in [ADR 0003](decisions/0003-idempotency-claim-inside-the-transaction.md):
a concurrency test that relies on random timing caught the naive idempotency
bug only 4 times in 30, so concurrency tests here force the interleaving
(by holding a lock) instead of hoping for it.
The competing-workers test was first written the hopeful way and still passed
with `SKIP LOCKED` removed; it now holds every worker at a barrier until all of
them have an event at once, which only `SKIP LOCKED` makes possible.
