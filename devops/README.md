# FinCore — DevOps

Everything needed to run and operate FinCore locally.

| Path | Purpose | Since |
|---|---|---|
| `docker-compose.yml` | Local stack: PostgreSQL 16 | Phase 0 |
| `.env.example` | Template for `devops/.env` (git-ignored) | Phase 0 |
| `docker/` | Dockerfiles for the API and worker | Phase 6 |
| `monitoring/` | Prometheus and Grafana configuration | V2 |
| `scripts/` | Operational helper scripts | as needed |

CI lives in [`/.github/workflows`](../.github/workflows) at the repository root,
because GitHub only reads workflows from there.

## Start PostgreSQL

From the repository root:

```sh
cp devops/.env.example devops/.env      # adjust if needed
docker compose -f devops/docker-compose.yml up -d postgres
docker compose -f devops/docker-compose.yml ps
```

Compose reads `devops/.env` automatically because it sits next to the
compose file.

## Stop / reset

```sh
docker compose -f devops/docker-compose.yml down        # stop, keep data
docker compose -f devops/docker-compose.yml down -v     # stop and DELETE all data
```
