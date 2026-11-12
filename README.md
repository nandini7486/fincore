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
| 1 | PostgreSQL schema and migrations | 🚧 In progress — migrations written, under review |
| 2 | Go API: auth, accounts, top-ups, payments, ledger, audit log | ⏳ Next |
| 3 | Idempotency and concurrency | ⏳ |
| 4 | Failure injection and test hardening | ⏳ |
| 5 | Outbox and background worker | ⏳ |
| 6 | Docker image, metrics, load test, V1 release | ⏳ |

V2 (Phases 7–12): hardened auth, dashboards, Python AI service, React UI. See the blueprint §12.

## What exists today

- `GET /healthz` — liveness endpoint with structured JSON logs and graceful shutdown.
- **Database schema** (9 migrations): `users`, `accounts`, `payments`,
  `ledger_entries`, `idempotency_records`, `audit_log`, `outbox_events`,
  `notifications`, plus the INR treasury account.
- **Database-enforced safety nets:** non-negative user balances, one treasury
  per currency, no self-payments, `FAILED` ⇔ failure reason, at most one
  debit and one credit per payment, unique idempotency key per user.
- **Append-only ledger and audit log:** a trigger rejects `UPDATE`, `DELETE`
  and `TRUNCATE`, and the `fincore_app` role is never granted those privileges.

## Prerequisites

- Go (see `backend/go.mod` for the minimum version)
- Docker Desktop (WSL2 backend on Windows)

## Run locally

```sh
cp .env.example .env                 # adjust values if needed
docker compose up -d postgres        # start PostgreSQL

cd backend
export DATABASE_URL=postgres://fincore:fincore_local_dev@localhost:5432/fincore?sslmode=disable
go run ./cmd/migrate up              # apply migrations
go run ./cmd/migrate status          # show applied migrations

go run ./cmd/api                     # API on http://localhost:8080
curl http://localhost:8080/healthz
```

PowerShell: use `$env:DATABASE_URL = "postgres://..."` instead of `export`.

Migration commands: `up` (apply all), `down` (roll back one), `reset` (roll back all), `status`.

## Test

```sh
cd backend
go vet ./...
go test ./...                        # unit tests; database tests are skipped
```

Database tests **reset the schema**, so point them at a disposable database:

```sh
docker compose exec postgres createdb -U fincore fincore_test
export FINCORE_TEST_DATABASE_URL=postgres://fincore:fincore_local_dev@localhost:5432/fincore_test?sslmode=disable
go test ./...
```

CI runs everything, including the database tests and a migrate CLI round trip,
on every push and pull request.

## Repository layout

```text
backend/
  cmd/api/        HTTP API server
  cmd/migrate/    Migration runner (up, down, reset, status)
  migrations/     goose SQL migrations (embedded) + schema tests
docs/
  database/       Schema design
.github/          CI workflow
```

## Changelog

- **Phase 1 — migrations:** 9 goose migrations, `cmd/migrate` CLI, schema
  tests for constraints, append-only triggers and role grants; CI now runs
  against a PostgreSQL service.
- **Phase 1 — schema design:** ER diagram and constraints agreed
  ([PR #1](https://github.com/nandini7486/fincore/pull/1)); account history
  served from two `payments` indexes.
- **Phase 0:** repository, Go module, `/healthz`, Docker Compose PostgreSQL, CI.
