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

## Status

| Phase | Focus | State |
|---|---|---|
| 0 | Repo, Go module, Compose, CI | ✅ Done |
| 1 | PostgreSQL schema and migrations | ✅ Done |
| 2 | Go API: auth, accounts, top-ups, payments, ledger, audit log | 🚧 In progress |
| 3 | Idempotency and concurrency | ⏳ |
| 4 | Failure injection and test hardening | ⏳ |
| 5 | Outbox, background worker, in-app notifications | ⏳ |
| 6 | Docker image, metrics, load test, V1 release | ⏳ |

Phase 2 is delivered in four PRs:

| Step | Contents | State |
|---|---|---|
| 2a | API foundation: config, DB pool, router, request IDs, errors, logging, `/readyz`, token auth, audit writer, `fincorectl` | ✅ Done |
| 2b | Accounts: create and get | 🚧 In review |
| 2c | Money parsing, top-ups, payments, ledger | ⏳ |
| 2d | Payment details and account history (cursor pagination) | ⏳ |

V2 (Phases 7–12): hardened auth, dashboards, Python AI service, React UI with
live notifications (SSE), webhooks. See the blueprint §12.

## Problems this project tackles

These are well-known, expensive problems in real payment systems. Large
fintechs solve them with dedicated teams and years of hardening. FinCore
applies the same proven techniques at small scale, and **tests each one**,
so every design choice can be explained and demonstrated.

| Problem in real payment systems | What goes wrong | FinCore's approach | Status |
|---|---|---|---|
| **Duplicate charges on retries** | A mobile client times out and retries; the server processes the payment twice | Required `Idempotency-Key`; the key is claimed with `INSERT … ON CONFLICT DO NOTHING` **inside** the payment transaction, and retries replay the stored response ([blueprint §6.3](docs/FINCORE_PROJECT_BLUEPRINT.md#63-why-the-claim-is-inside-the-transaction)) | Schema ✅ · logic Phase 2c/3 |
| **Double-spend under concurrency** | Two simultaneous payments both pass the balance check and overdraw the account | Both account rows locked in one statement, in ID order, and funds checked while locked; a database `CHECK` makes negative user balances impossible even if code is wrong | `CHECK` ✅ · locking Phase 2c/3 |
| **Deadlocks between opposite transfers** | A→B and B→A lock rows in opposite orders and block each other | Deterministic lock order (`ORDER BY id FOR UPDATE`), plus `lock_timeout` returned as a retryable `503` | Phase 3 |
| **Ledger drift** | Stored balances slowly disagree with transaction history, and nobody can say which is right | Balance is only a projection; the **append-only double-entry ledger** is the source of truth; a reconciliation check verifies every balance and that the global ledger sums to zero | Append-only ✅ · reconciliation Phase 2c+ |
| **Lost or phantom events (dual writes)** | The payment commits but the notification event is lost, or the event is sent for a payment that rolled back | **Transactional outbox**: the event row commits in the same transaction as the money; a worker delivers it afterwards | Table ✅ · worker Phase 5 |
| **Money moves without an audit trail** | Ops and compliance cannot answer "who did what, when, from where" | Audit record written **in the same transaction** as each change, so it cannot be missing; every record carries the request ID, which also appears in logs and errors | ✅ users, auth, accounts |
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

- Every response carries an `X-Request-ID` (client-supplied if well-formed,
  otherwise generated). The same ID appears in logs, error bodies and audit records.
- Errors use one envelope: `{"error": {"code", "message", "request_id"}}`.
- One structured JSON log line per request; headers and bodies are never logged.
- Panics become a logged `500` without leaking details.
- Graceful shutdown lets in-flight requests finish.
- Money is returned as a decimal string (`"0.00"`), never a JSON number.
- Accounts you may not see (someone else's, the treasury, missing or malformed
  IDs) all return the same `404 ACCOUNT_NOT_FOUND`.

**Authentication (V1)**

- Users are created by an operator with `fincorectl`, which prints a random
  API token **once**. Only its SHA-256 hash is stored.
- Requests send `Authorization: Bearer <token>`. Missing or invalid tokens get
  `401` and an `auth.failure` audit record (the token is never recorded).
- Identity always comes from the token, never from the request body.

**Database**

- 9 migrations: `users`, `accounts`, `payments`, `ledger_entries`,
  `idempotency_records`, `audit_log`, `outbox_events`, `notifications`, plus
  the INR treasury account.
- Database-enforced safety nets: non-negative user balances, one treasury per
  currency, no self-payments, `FAILED` ⇔ failure reason, at most one debit and
  one credit per payment, unique idempotency key per user.
- Append-only ledger and audit log: a trigger rejects `UPDATE`, `DELETE` and
  `TRUNCATE`, and the `fincore_app` role is never granted those privileges.

## Notifications

No external or paid service. A payment writes an event to the PostgreSQL
**outbox** in the same transaction; a Go worker turns it into an in-app
notification (`GET /api/v1/notifications`). V2 adds Server-Sent Events,
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
curl http://localhost:8080/readyz
curl -H "Authorization: Bearer <token>" http://localhost:8080/api/v1/me
curl -X POST -H "Authorization: Bearer <token>" http://localhost:8080/api/v1/accounts
curl -H "Authorization: Bearer <token>" http://localhost:8080/api/v1/accounts/<account_id>
```

PowerShell: use `$env:DATABASE_URL = "postgres://..."` instead of `export`.

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | — (required) | PostgreSQL connection string |
| `HTTP_ADDR` | `:8080` | Listen address |
| `DB_MAX_CONNS` | `10` | Connection pool size |

Commands:

| Command | Purpose |
|---|---|
| `go run ./cmd/api` | Run the API server |
| `go run ./cmd/migrate up\|down\|reset\|status` | Manage migrations |
| `go run ./cmd/fincorectl user create -name N -email E [-role ADMIN]` | Create a user and print its token |

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

CI runs everything, including the database tests and a migrate CLI round trip,
on every push and pull request.

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
  cmd/migrate/          Migration runner (up, down, reset, status)
  cmd/fincorectl/       Operator CLI (create users)
  internal/
    accounts/           Account creation and owner-only reads
    audit/              Append-only audit log writer
    auth/               Bearer-token auth, identity, role checks
    httpapi/            Router, middleware, handlers
    money/              Minor units <-> decimal strings (no floats)
    users/              User creation
    platform/           config, db (pool, WithTx), httpx (JSON/errors), reqctx
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
