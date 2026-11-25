# FinCore — DevOps

Everything needed to run and operate FinCore locally.

| Path | Purpose | Since |
|---|---|---|
| `docker-compose.yml` | Full local stack: PostgreSQL, migrate, API, worker, fincorectl | Phase 0 (DB), Phase 6 (app) |
| `.env.example` | Template for `devops/.env` (git-ignored) | Phase 0 |
| `docker/Dockerfile` | Multi-stage build: one distroless, non-root image with all four binaries | Phase 6 |
| `docker/postgres-init/` | First-boot script: the least-privilege database login | Phase 6 |
| `scripts/smoke.sh` | End-to-end gate: real binaries over HTTP | Phase 2 |
| `scripts/compose-check.sh` | Deployment gate: the Compose stack checked as an operator would | Phase 6 |
| `monitoring/` | Prometheus and Grafana configuration | V2 |

CI lives in [`/.github/workflows`](../.github/workflows) at the repository root,
because GitHub only reads workflows from there.

## Run everything

From the repository root:

```sh
cp devops/.env.example devops/.env      # optional: every value has a default
docker compose -f devops/docker-compose.yml up -d --build --wait
docker compose -f devops/docker-compose.yml ps
```

Start order is enforced by Compose:

```
postgres (healthy) ──► migrate (runs once, exit 0 required) ──► api + worker
```

If a migration fails, the API and worker never start against a half-migrated
schema.

> **Volume from before Phase 6?** The `fincore_api` login is created only when
> the database volume is first initialised. Recreate it with
> `docker compose -f devops/docker-compose.yml down -v` (deletes local data).

| Service | Port (127.0.0.1 only) | Database login |
|---|---|---|
| `api` | `8080` API, `9090` `/metrics` | `fincore_api` (member of `fincore_app`) |
| `worker` | `9091` `/metrics` | `fincore_api` |
| `migrate` | — (exits) | `fincore` (schema owner) |
| `postgres` | `5432` | — |

Create users and check the books with the admin CLI:

```sh
docker compose -f devops/docker-compose.yml run --rm fincorectl user create -name Alice -email alice@example.com
docker compose -f devops/docker-compose.yml run --rm fincorectl reconcile
```

## The image

`docker/Dockerfile` builds all four binaries (`api`, `worker`, `migrate`,
`fincorectl`) as static Go binaries and copies them into
`gcr.io/distroless/static-debian12:nonroot`. The image has no shell or
package manager, runs as uid 65532 and is about 70 MB on disk.
Because there is no `curl`, the container healthcheck is the binary itself:
`/app/api healthcheck` (asks `/readyz`), or `/app/api healthcheck <url>`
(used for the worker's metrics port).

```sh
docker build -f devops/docker/Dockerfile --build-arg VERSION=v1.0.0 -t fincore:v1.0.0 backend
```

## Least-privilege database login

The API and worker never connect as the schema owner. On the volume's first
start, `docker/postgres-init/01-app-login-role.sh` creates `fincore_api`, a
login with no privileges of its own, and makes it a member of `fincore_app`,
whose grants come from migration `00008`. So the running application
**cannot** `DELETE` or `UPDATE` ledger entries or audit records, or change
the schema. `compose-check.sh` verifies each of these; the checks were also
run against the owner login to confirm they fail when the rule is broken.

## Metrics

`/metrics` (Prometheus text format) is served on a separate internal port,
never on the public API port. Main series:

| Metric | What it tells you |
|---|---|
| `fincore_http_requests_total{method,route,status}` | Traffic and error rate per route. `route` is the route *pattern*, so random URLs cannot create new series |
| `fincore_http_request_duration_seconds{method,route}` | Latency histogram (p50/p95/p99 via `histogram_quantile`) |
| `fincore_http_requests_in_flight` | Concurrent requests |
| `fincore_db_pool_*` | Connections in use, and how often and how long requests waited for one |
| `fincore_outbox_pending_events`, `fincore_outbox_oldest_pending_age_seconds` | Worker backlog and lag (read from the database at scrape time) |
| `fincore_outbox_failing_events`, `fincore_outbox_events_handled_total{event_type,result}` | Delivery failures |
| `fincore_build_info{version,component}` | Which build is running |

Route label meanings: `/api/v1/*` is a request that stopped in the API
sub-router before a specific route matched (a `401`, or an unknown path under
`/api/v1`); `unmatched` is no route at all (unknown path or `405`).

## Check the deployment

```sh
devops/scripts/compose-check.sh
```

Builds the image, starts its own Compose project on separate ports, runs 30
checks (containers, privileges, API, metrics, reconcile, graceful stop and
restart) and deletes everything on exit. CI runs it on every push.

## Stop / reset

```sh
docker compose -f devops/docker-compose.yml down        # stop, keep data
docker compose -f devops/docker-compose.yml down -v     # stop and DELETE all data
```
