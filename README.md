# FinCore

[![CI](https://github.com/nandini7486/fincore/actions/workflows/ci.yml/badge.svg)](https://github.com/nandini7486/fincore/actions/workflows/ci.yml)

A learning-focused payment-processing platform: atomic account-to-account
transfers, a double-entry ledger, idempotent retries, concurrency-safe
balance updates, and an append-only audit log — built in Go on PostgreSQL.

> FinCore is a **simulation and portfolio project**. It does not move real
> money or connect to real banking rails.

## Documentation

| Document | What it covers |
|---|---|
| [Project blueprint](docs/FINCORE_PROJECT_BLUEPRINT.md) | Requirements, architecture, API, roadmap, confirmed decisions |
| [Database schema design](docs/database/schema-design.md) | ER diagram, constraints, invariants, roles, migration plan |
| [Decision records](docs/decisions/) | Why key design choices were made |
| [Testing](docs/TESTING.md) | Test layers, failure scenarios, status-code coverage, mutation checks |

## Status

| Phase | Focus | State |
|---|---|---|
| 0 | Repo, Go module, Compose, CI | ✅ Done |
| 1 | PostgreSQL schema and migrations | ✅ Done |
| 2 | Go API: auth, accounts, top-ups, payments, ledger, audit log | ✅ Done |
| 3 | Idempotency and concurrency | ✅ Done |
| 4 | Failure injection and test hardening | ✅ Done |
| 5 | Outbox, background worker, in-app notifications | ✅ Done |
| 6 | Docker image, metrics, load test, V1 release | ⏳ |

Phase 2 is delivered in four PRs:

| Step | Contents | State |
|---|---|---|
| 2a | API foundation: config, DB pool, router, request IDs, errors, logging, `/readyz`, token auth, audit writer, `fincorectl` | ✅ Done |
| 2b | Accounts: create and get | ✅ Done |
| 2c | Money parsing, top-ups, payments, ledger | ✅ Done |
| 2d | Payment details and account history (cursor pagination) | ✅ Done |

V2 (Phases 7–12): hardened auth, dashboards, Python AI service, React UI with
live notifications (SSE), webhooks. See the blueprint §12.

## Problems this project tackles

These are well-known, expensive problems in real payment systems. Large
fintechs solve them with dedicated teams and years of hardening. FinCore
applies the same proven techniques at small scale, and **tests each one**,
so every design choice can be explained and demonstrated.

| Problem in real payment systems | What goes wrong | FinCore's approach | Status |
|---|---|---|---|
| **Duplicate charges on retries** | A mobile client times out and retries; the server processes the payment twice | Required `Idempotency-Key`; the key is claimed with `INSERT … ON CONFLICT DO NOTHING` **inside** the payment transaction, and retries replay the stored response ([blueprint §6.3](docs/FINCORE_PROJECT_BLUEPRINT.md#63-why-the-claim-is-inside-the-transaction)) | ✅ in-transaction claim, replay, `409`; double-charge race tested ([ADR 0003](docs/decisions/0003-idempotency-claim-inside-the-transaction.md)) |
| **Double-spend under concurrency** | Two simultaneous payments both pass the balance check and overdraw the account | Both account rows locked in one statement, in ID order, and funds checked while locked; a database `CHECK` makes negative user balances impossible even if code is wrong | ✅ locking, `CHECK`, concurrency test |
| **Deadlocks between opposite transfers** | A→B and B→A lock rows in opposite orders and block each other | Deterministic lock order (`ORDER BY id FOR UPDATE`), plus `lock_timeout` returned as a retryable `503` | ✅ tested; more scenarios Phase 3 |
| **Ledger drift** | Stored balances slowly disagree with transaction history, and nobody can say which is right | Balance is only a projection; the **append-only double-entry ledger** is the source of truth; a reconciliation check verifies every balance and that the global ledger sums to zero | ✅ ledger · `fincorectl reconcile` (one snapshot, exit 1 on any problem), run after every payment test |
| **Lost or phantom events (dual writes)** | The payment commits but the notification event is lost, or the event is sent for a payment that rolled back | **Transactional outbox**: the event row commits in the same transaction as the money; a worker delivers it afterwards | ✅ event written in the payment transaction; `cmd/worker` delivers with `SKIP LOCKED`, retries, idempotent consumer ([ADR 0004](docs/decisions/0004-transactional-outbox-and-worker.md)) |
| **Money moves without an audit trail** | Ops and compliance cannot answer "who did what, when, from where" | Audit record written **in the same transaction** as each change, so it cannot be missing; every record carries the request ID, which also appears in logs and errors | ✅ users, auth, accounts, payments, top-ups |
| **Floating-point money bugs** | `0.1 + 0.2 ≠ 0.3`; rounding errors accumulate in balances | Money is `BIGINT` paise in the database and `int64` in Go; the API uses decimal strings like `"2000.00"`, never JSON numbers | ✅ |
| **Broken object-level authorization (IDOR)** | Changing an ID in the URL exposes another customer's account (OWASP API Top 10 #1) | Identity comes only from the token; any account you may not see returns the **same 404**, and probing is audited ([ADR 0001](docs/decisions/0001-not-found-for-inaccessible-accounts.md)) | ✅ |
| **Over-privileged application database user** | A bug or injection can rewrite history | The API runs as `fincore_app`, which has no `UPDATE`/`DELETE` on the ledger or audit log; a trigger blocks them anyway; tests run with exactly these privileges | ✅ |

## AI in FinCore (V2, planned)

AI is planned for V2 as a separate Python service. It is **advisory**: it
helps people find problems faster, but it never moves money.

### Why add AI to a payment system

Rule-based checks ("block payments over ₹X", "flag more than N payments per
hour") are predictable, but they have two well-known weaknesses in fintech:

- **False positives:** static rules block legitimate customers, who then
  contact support or leave.
- **Unknown patterns:** rules only catch the patterns someone thought of in
  advance.

Machine learning helps with both, but only if its output is **explainable**
(risk teams and regulators must know *why* something was flagged) and if it
learns from **clean, trustworthy data**. Messy, mutable data is one of the main
reasons fintech ML projects fail.

### What the AI service will do

| Capability | What it does | Who it helps |
|---|---|---|
| **Anomaly detection** | Scores each payment against the account's own history (amount, velocity, new counterparty, time of day) using an unsupervised model such as Isolation Forest | Risk team: reviews unusual payments first |
| **Explainable risk scores** | Every flag lists the features that drove it, e.g. "amount 12× this account's median" | Reviewers and auditors: no black-box decisions |
| **Failure analysis** | Groups `FAILED` payments by reason, time window and account features to spot systemic issues early | Ops: finds incidents before support tickets do |
| **Natural-language analytics** | Turns a plain-English question into one of a fixed set of **pre-approved** queries, never free-form SQL | Non-engineers: answers without database access |

### How the V1 backend makes this possible

The V1 design is what makes the AI trustworthy:

- **Immutable history.** The ledger and audit log are append-only, so models
  train on data that cannot be silently edited.
- **Failures are kept.** Business-rule failures are stored as `FAILED`
  payments with a `failure_reason`, not discarded, so there is labeled data
  for failure analysis.
- **Correlated records.** One request ID links logs, audit records and
  payments, so every flag can be traced to the exact request.
- **Events without coupling.** The outbox lets the AI service consume
  payment events without slowing down or risking the payment path.

### Safety boundary

- The Go payment service is the only authority on balances and payment state.
- AI cannot debit, credit, approve, reject or refund. Its database role is
  **read-only**.
- AI output is advisory, explained, and recorded in the audit log when shown
  to a user.
- Experiments use synthetic or anonymized data.

See [blueprint §9](docs/FINCORE_PROJECT_BLUEPRINT.md#9-ai-intelligence-service-----v2).

## What exists today

**API**

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/healthz` | — | Process is alive |
| `GET` | `/readyz` | — | Database is reachable |
| `GET` | `/api/v1/me` | Bearer token | Returns the authenticated user |
| `POST` | `/api/v1/accounts` | Bearer token | Open an INR account (balance `0.00`, `ACTIVE`) |
| `GET` | `/api/v1/accounts/{account_id}` | Bearer token, owner only | Account details and balance |
| `POST` | `/api/v1/accounts/{account_id}/topups` | Bearer token, **admin** | Fund an account from the treasury |
| `POST` | `/api/v1/payments` | Bearer token, sender owner | Transfer between accounts |
| `GET` | `/api/v1/payments/{payment_id}` | Bearer token, sender or receiver owner | Payment details (incl. `failure_reason`) |
| `GET` | `/api/v1/accounts/{account_id}/payments?limit=&cursor=` | Bearer token, owner only | Payment history, newest first |
| `GET` | `/api/v1/notifications?limit=&cursor=` | Bearer token | Your notifications, newest first (delivered by the worker) |

- Every response carries an `X-Request-ID` (client-supplied if well-formed,
  otherwise generated). The same ID appears in logs, error bodies and audit records.
- Errors use one envelope: `{"error": {"code", "message", "request_id"}}`.
- One structured JSON log line per request; headers and bodies are never logged.
- Panics become a logged `500` without leaking details.
- Graceful shutdown lets in-flight requests finish.
- Money is returned as a decimal string (`"0.00"`), never a JSON number.
- Accounts you may not see (someone else's, the treasury, missing or malformed
  IDs) all return the same `404 ACCOUNT_NOT_FOUND`.

**Payments and top-ups**

- Amounts are decimal strings (`"2000.00"`): greater than zero, at most two
  decimal places, up to ₹1,00,00,000.00. No floats, signs or exponents.
- One transaction per payment: lock both accounts **in ID order**, check the
  rules while locked, then write the payment, both balance changes, one DEBIT
  and one CREDIT ledger entry, and the audit record.
- Business-rule failures (`INSUFFICIENT_FUNDS`, `ACCOUNT_INACTIVE`,
  `CURRENCY_MISMATCH`) are stored as `FAILED` payments and return `422` with
  the `payment_id`; no money moves.
- Lock or statement timeouts return `503 SERVICE_BUSY` with `Retry-After`;
  nothing is committed, so retrying is safe.
- Top-ups move money from the treasury, the only account allowed to go
  negative. Its balance equals the money issued.
- **`Idempotency-Key` is required.** Send a new UUID per payment and the same
  one on every retry. A retry of a completed request returns the stored
  response with `Idempotent-Replayed: true` and moves no money; the same key
  with a different request is `409 IDEMPOTENCY_KEY_REUSED`. Keys are per user
  and expire after 24 hours. See [ADR 0003](docs/decisions/0003-idempotency-claim-inside-the-transaction.md).

**History**

- Sent and received payments, including `FAILED` ones, newest first.
- **Keyset pagination** on `(created_at, id)`: follow `next_cursor` until it is
  `null`. Pages stay correct while new payments arrive, and a deep page costs
  the same as the first (two index scans, ~0.3 ms on 200k payments). See
  [ADR 0002](docs/decisions/0002-keyset-pagination-for-history.md).
- `limit` 1-100 (default 20); invalid `limit` or `cursor` returns `400`.

**Authentication (V1)**

- Users are created by an operator with `fincorectl`, which prints a random
  API token **once**. Only its SHA-256 hash is stored.
- Requests send `Authorization: Bearer <token>`. Missing or invalid tokens get
  `401` and an `auth.failure` audit record (the token is never recorded).
- Identity always comes from the token, never from the request body.

**Database**

- 10 migrations: `users`, `accounts`, `payments`, `ledger_entries`,
  `idempotency_records`, `audit_log`, `outbox_events`, `notifications`, plus
  the INR treasury account.
- Database-enforced safety nets: non-negative user balances, one treasury per
  currency, no self-payments, `FAILED` ⇔ failure reason, at most one debit and
  one credit per payment, unique idempotency key per user.
- Append-only ledger and audit log: a trigger rejects `UPDATE`, `DELETE` and
  `TRUNCATE`, and the `fincore_app` role is never granted those privileges.

## Reconciliation

`fincorectl reconcile` proves the books balance. In one `REPEATABLE READ`
snapshot (so payments committing meanwhile cannot cause false alarms) it
checks that:

1. every account balance equals the sum of its ledger entries,
2. the whole ledger sums to zero (money is never created or destroyed),
3. every successful payment has exactly one DEBIT on the sender and one
   CREDIT on the receiver, for the payment's amount,
4. failed payments have no ledger entries.

```text
$ fincorectl reconcile
Checked 3 accounts, 2 payments, 4 ledger entries.
Ledger sum: 0 (must be 0)
Accounts whose balance disagrees with the ledger: 1
  646782bc-… (USER): stored 59000, ledger 9000, difference 50000
Payments with wrong ledger entries: 0
PROBLEMS FOUND: investigate before moving more money.
```

Exit codes (0 OK, 1 problems, 2 error) make it usable from cron or CI. Each
run is audited as `reconciliation.run`. The payment tests run the same
reconciler after every test, and its own tests corrupt the books five
different ways (including a credit posted to the wrong account, which leaves
every total looking correct) to prove each is caught.

## Notifications

No external or paid service. A successful payment writes an event to the
PostgreSQL **outbox** in the same transaction; the worker (`cmd/worker`)
turns it into an in-app notification for the receiver, such as
"You received ₹2,000.00 from Alice." Several workers can run at once
(`FOR UPDATE SKIP LOCKED`); failed events retry with backoff; redelivery
never duplicates a notification. See [ADR 0004](docs/decisions/0004-transactional-outbox-and-worker.md).
Read them with `GET /api/v1/notifications?limit=&cursor=` (newest first, the
same keyset pagination as payment history, each with its `payment_id`). V2 adds Server-Sent Events,
webhooks and optional Mailpit email on top of the same worker.
See [blueprint §8.6](docs/FINCORE_PROJECT_BLUEPRINT.md#86-notifications).

## Prerequisites

- Go (see `backend/go.mod` for the minimum version)
- Docker Desktop (WSL2 backend on Windows)

## Run locally

```sh
cp devops/.env.example devops/.env                          # adjust values if needed
docker compose -f devops/docker-compose.yml up -d postgres  # start PostgreSQL

cd backend
export DATABASE_URL=postgres://fincore:fincore_local_dev@localhost:5432/fincore?sslmode=disable
go run ./cmd/migrate up              # apply migrations

# create a user and get an API token (shown once)
go run ./cmd/fincorectl user create -name "Alice" -email alice@example.com

go run ./cmd/api                     # API on http://localhost:8080
go run ./cmd/worker                  # in another terminal: notifications + cleanup
curl http://localhost:8080/readyz
curl -H "Authorization: Bearer <token>" http://localhost:8080/api/v1/me
curl -X POST -H "Authorization: Bearer <token>" http://localhost:8080/api/v1/accounts
curl -H "Authorization: Bearer <token>" http://localhost:8080/api/v1/accounts/<account_id>
# admin token: fund an account
curl -X POST -H "Authorization: Bearer <admin_token>" -H 'Content-Type: application/json' \
     -H "Idempotency-Key: $(uuidgen)" \
     -d '{"amount":"5000.00","currency":"INR"}' http://localhost:8080/api/v1/accounts/<account_id>/topups
# transfer
curl -X POST -H "Authorization: Bearer <token>" -H 'Content-Type: application/json' \
     -H "Idempotency-Key: $(uuidgen)" \
     -d '{"sender_account_id":"<from>","receiver_account_id":"<to>","amount":"2000.00","currency":"INR"}' \
     http://localhost:8080/api/v1/payments
# history (then add &cursor=<next_cursor> for the next page)
curl -H "Authorization: Bearer <token>" "http://localhost:8080/api/v1/accounts/<account_id>/payments?limit=20"
```

PowerShell: use `$env:DATABASE_URL = "postgres://..."` instead of `export`.

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | — (required) | PostgreSQL connection string |
| `HTTP_ADDR` | `:8080` | Listen address |
| `DB_MAX_CONNS` | `10` | Connection pool size |
| `WORKER_POLL_INTERVAL` | `1s` | Worker: how often to look for outbox events |
| `IDEMPOTENCY_CLEANUP_INTERVAL` | `1m` | Worker: how often to delete expired idempotency keys |

Commands:

| Command | Purpose |
|---|---|
| `go run ./cmd/api` | Run the API server |
| `go run ./cmd/worker` | Run the outbox worker (notifications, key cleanup) |
| `go run ./cmd/migrate up\|down\|reset\|status` | Manage migrations |
| `go run ./cmd/fincorectl user create -name N -email E [-role ADMIN]` | Create a user and print its token |
| `go run ./cmd/fincorectl reconcile [-json]` | Check the books balance; exit 0 OK, 1 problems found, 2 error |

## Test

```sh
cd backend
go vet ./...
go test ./...                        # unit tests; database tests are skipped
```

Database tests create (and drop) their **own throwaway database per test
package**, and run the code under test as the restricted `fincore_app` role —
the same privileges the API has. Point them at a server where the user may
create databases:

```sh
export FINCORE_TEST_DATABASE_URL=postgres://fincore:fincore_local_dev@localhost:5432/fincore?sslmode=disable
go test ./...
```

End-to-end gate (builds the real binaries, fresh database, every endpoint
over HTTP, then reconciliation):

```sh
FINCORE_ADMIN_DATABASE_URL=$FINCORE_TEST_DATABASE_URL scripts/smoke.sh
```

CI runs everything, including the database tests, a migrate CLI round trip,
`fincorectl reconcile` and the smoke test, on every push and pull request.
See [docs/TESTING.md](docs/TESTING.md) for failure scenarios and coverage.

## Repository layout

The repo has three areas — **backend**, **frontend**, **devops** — plus
shared docs. `.github/` stays at the root because GitHub only reads
workflows from there.

| Folder | Contents | State |
|---|---|---|
| [`backend/`](backend) | Go API, CLIs, migrations, tests | Active |
| [`frontend/`](frontend) | React + TypeScript dashboard | Planned (V2) |
| [`devops/`](devops) | Docker Compose, env template; later Dockerfiles, monitoring | Active |
| [`docs/`](docs) | Blueprint, schema design, decisions | Active |
| [`.github/`](.github) | CI workflow | Active |

```text
backend/
  cmd/api/              HTTP API server
  cmd/worker/           Outbox worker (notifications, idempotency cleanup)
  cmd/migrate/          Migration runner (up, down, reset, status)
  cmd/fincorectl/       Operator CLI (create users, reconcile)
  internal/
    accounts/           Account creation and owner-only reads
    audit/              Append-only audit log writer
    auth/               Bearer-token auth, identity, role checks
    httpapi/            Router, middleware, handlers
    idempotency/        Idempotency-Key claim, fingerprint, stored responses
    money/              Minor units <-> decimal strings (no floats), ₹ display
    notifications/      Outbox consumer: in-app notifications
    outbox/             Transactional outbox: Write + worker (SKIP LOCKED, retries)
    payments/           Transfers, top-ups, ledger entries, history (keyset pagination)
    reconcile/          Books check: balances vs ledger, zero sum, per-payment entries
    users/              User creation
    platform/           config, db (pool, WithTx), cursor, httpx (JSON/errors), ids, reqctx
    testutil/testdb/    Per-package throwaway test databases
  migrations/           goose SQL migrations (embedded) + schema tests
frontend/               React + TypeScript dashboard (V2, placeholder)
devops/
  docker-compose.yml    Local PostgreSQL
  .env.example          Template for devops/.env (git-ignored)
docs/
  database/             Schema design
  decisions/            Architecture decision records (ADRs)
.github/workflows/      CI
```

## Changelog

- **Phase 5b — notifications API:** `GET /api/v1/notifications` (caller's own,
  newest first, keyset pagination, `payment_id`); shared `platform/cursor`
  and `pageParams`; migration 10 adds `(user_id, created_at DESC, id DESC)`
  (0.25 ms per page on 200k notifications); the smoke test now runs the
  worker and checks notifications over HTTP (35 checks). Phase 5 complete.
- **Phase 5a — outbox and worker:** successful payments write
  `payment.succeeded` / `topup.succeeded` in the same transaction;
  `cmd/worker` delivers them (`SKIP LOCKED`, savepoint per handler, retries
  with backoff, panic-safe) to an idempotent notifications consumer, and
  deletes expired idempotency keys in batches; `money.FormatINR`
  ([ADR 0004](docs/decisions/0004-transactional-outbox-and-worker.md)).
- **Phase 4b — process failures:** tests that build and run the real API
  binary: `kill -9` mid-payment (rolled back; after restart the same key
  charges once) and `SIGTERM` mid-payment (new connections refused, the
  in-flight payment commits, exit 0). Phase 4 complete.
- **Phase 4a — failure injection:** tests that break audit and ledger writes,
  kill the database connection and disconnect the client mid-payment, each
  proving nothing partial survives and the retry charges once; client
  disconnects logged as `499` warnings instead of `500` errors; migration 9
  refuses rollback once money has moved; `scripts/smoke.sh` end-to-end gate
  in CI; `docs/TESTING.md`.
- **Phase 3b — reconciliation and stress:** `internal/reconcile` and
  `fincorectl reconcile [-json]` (snapshot isolation, audited, exit codes);
  payment tests now use the real reconciler; five corruption scenarios
  tested; HTTP stress test with random concurrent payments, concurrent
  top-ups and double-sent requests ends with balanced books and balances
  matching an independent model. Phase 3 complete.
- **Phase 3a — idempotency:** required `Idempotency-Key` on payments and
  top-ups; claim inside the payment transaction, replay of `201`/`422`,
  `409` on reuse, per-user scope, 24-hour expiry; `idempotency.conflict`
  audited ([ADR 0003](docs/decisions/0003-idempotency-claim-inside-the-transaction.md)).
  A deterministic in-flight duplicate test catches a naive check-then-insert
  10/10 times.
- **Phase 2d — reading payments:** `GET /api/v1/payments/{id}` (sender or
  receiver owner) and `GET /api/v1/accounts/{id}/payments` with keyset
  pagination and an opaque cursor ([ADR 0002](docs/decisions/0002-keyset-pagination-for-history.md));
  `failure_reason` in payment responses; shared `ids.IsUUID`,
  `ownedAccount` and `auditDenied` helpers. Tests cover gaps, duplicates,
  timestamp ties and payments arriving mid-scroll. Phase 2 complete.
- **Phase 2c — money movement:** strict `money.Parse` (fuzz-tested round trip),
  `POST /api/v1/payments` and admin `POST /api/v1/accounts/{id}/topups`;
  ordered row locks, rules checked under lock, double-entry ledger, `FAILED`
  payments for rule failures (`422`), `lock_timeout`/`statement_timeout` as
  `503`, validation failures audited. Tests check ledger invariants after
  every scenario, concurrency (no overdraft, no deadlock) and lock timeouts.
- **Phase 2b — accounts:** `POST /api/v1/accounts` and owner-only
  `GET /api/v1/accounts/{account_id}`; `account.create` audited in the same
  transaction; uniform `404` plus `authz.denied` audit for other users'
  accounts ([ADR 0001](docs/decisions/0001-not-found-for-inaccessible-accounts.md));
  `money.Format` for decimal-string balances. README: problems FinCore
  tackles and the V2 AI plan.
- **Repo layout:** split into `backend/`, `frontend/` and `devops/`;
  `docker-compose.yml` and `.env.example` moved to `devops/`; CI and docs updated.
- **Phase 2a — API foundation:** config, pgx pool and `WithTx`, chi router,
  request IDs, JSON error envelope, access logging, panic recovery, `/readyz`,
  bearer-token auth with audited failures, `RequireRole`, audit writer,
  `fincorectl user create`, `GET /api/v1/me`. Tests use per-package
  throwaway databases and the `fincore_app` role.
- **Spec — notifications:** outbox → worker → in-app notifications in V1;
  SSE, webhooks, optional Mailpit email in V2 (blueprint §8.6, decision #11).
- **Phase 1 — migrations:** 9 goose migrations, `cmd/migrate` CLI, schema
  tests for constraints, append-only triggers and role grants; CI now runs
  against a PostgreSQL service.
- **Phase 1 — schema design:** ER diagram and constraints agreed
  ([PR #1](https://github.com/nandini7486/fincore/pull/1)); account history
  served from two `payments` indexes.
- **Phase 0:** repository, Go module, `/healthz`, Docker Compose PostgreSQL, CI.
