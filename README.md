# FinCore

A learning-focused payment-processing platform: atomic account-to-account
transfers, a double-entry ledger, idempotent retries, concurrency-safe
balance updates, and an append-only audit log — built in Go on PostgreSQL.

> FinCore is a **simulation and portfolio project**. It does not move real
> money or connect to real banking rails.

See the full specification: [docs/FINCORE_PROJECT_BLUEPRINT.md](docs/FINCORE_PROJECT_BLUEPRINT.md).

## Status

**Phase 0** — repository, Go module, local PostgreSQL via Docker Compose, CI.

## Prerequisites

- Go (see `backend/go.mod` for the minimum version)
- Docker Desktop (WSL2 backend on Windows)

## Run locally

```sh
cp .env.example .env          # adjust values if needed
docker compose up -d postgres # start PostgreSQL
cd backend
go run ./cmd/api              # API on http://localhost:8080
curl http://localhost:8080/healthz
```

## Test

```sh
cd backend
go vet ./...
go test ./...
```

## Repository layout

```text
backend/     Go API (modular monolith)
docs/        Specification, architecture, decisions (ADRs)
.github/     CI workflows
```
