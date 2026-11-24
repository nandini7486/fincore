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
| 2a | API foundation: config, DB pool, router, request IDs, errors, logging, `/readyz`, token auth, audit writer, `fincorectl` | 🚧 In review |
| 2b | Accounts: create and get | ⏳ |
| 2c | Money parsing, top-ups, payments, ledger | ⏳ |
| 2d | Payment details and account history (cursor pagination) | ⏳ |

V2 (Phases 7–12): hardened auth, dashboards, Python AI service, React UI with
live notifications (SSE), webhooks. See the blueprint §12.

## What exists today

**API**

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/healthz` | — | Process is alive |
| `GET` | `/readyz` | — | Database is reachable |
| `GET` | `/api/v1/me` | Bearer token | Returns the authenticated user |

- Every response carries an `X-Request-ID` (client-supplied if well-formed,
  otherwise generated). The same ID appears in logs, error bodies and audit records.
- Errors use one envelope: `{"error": {"code", "message", "request_id"}}`.
- One structured JSON log line per request; headers and bodies are never logged.
- Panics become a logged `500` without leaking details.
- Graceful shutdown lets in-flight requests finish.

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
    audit/              Append-only audit log writer
    auth/               Bearer-token auth, identity, role checks
    httpapi/            Router, middleware, handlers
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
.github/workflows/      CI
```

## Changelog

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
