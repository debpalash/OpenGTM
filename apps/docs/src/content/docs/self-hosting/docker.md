---
title: Docker Compose
description: Every service in the bundled stack, the ports they use, the image, and how to scale.
sidebar:
  order: 1
---

`docker compose up` starts the whole stack from one immutable application
image plus PostgreSQL, Redis and nginx.

## Services

| Service | Image | Host port | Purpose |
|---|---|---|---|
| `postgres` | `postgres:16-alpine` | `127.0.0.1:5432` | Primary store; volume `pgdata` |
| `redis` | `redis:7-alpine` | `127.0.0.1:6379` | Live progress pub/sub; volume `redisdata`. No auth: never expose beyond loopback |
| `migrate` | app image, one-shot | | `python -m apps.api.scripts.migrate` as the schema owner; provisions the runtime role; api/worker wait for it |
| `api` | app image | `127.0.0.1:8000` | FastAPI with `RUN_INLINE_WORKER=0`: it enqueues, never executes user work |
| `worker` | app image | | `python -m apps.api.worker`; no fixed container name so `--scale` works |
| `scheduler` | app image | | `python -m apps.api.scheduler`; single reconciliation owner |
| `seed` | app image, one-shot | | First-run admin and zero-key demo workbook; idempotent |
| `frontend_assets` | app image, one-shot | | Copies the built React bundle into the `frontend_dist` volume on every deploy |
| `nginx` | `nginx:alpine` | `${PORT:-3000}` | Serves the SPA, proxies `/api`, `/auth` and `/health` |
| `reacher` | `reacherhq/backend:v0.10.0` | none | Optional email verification, `--profile reacher` |

The application image is `${YUPCHA_IMAGE:-lead-data-app:local}`. Set
`YUPCHA_IMAGE=ghcr.io/debpalash/opengtm:<version>` to run a published image
instead of building locally.

## Database roles

`migrate` and `seed` connect as the schema owner (`DATABASE_URL`). `api`,
`worker` and `scheduler` connect as `yupcha_runtime` via `APP_DATABASE_URL`,
a role without superuser or `BYPASSRLS`, so forced row-level security applies
to all runtime traffic. Change `POSTGRES_PASSWORD` and
`YUPCHA_RUNTIME_DB_PASSWORD` for any shared install.

## nginx

Inside the container nginx listens on port 80 and resolves the `api` upstream
dynamically, so a long-lived proxy never pins a dead container IP after a
redeploy. `/api/` is proxied with WebSocket upgrade, 300-second timeouts and
buffering off for server-sent events. Static assets get one-year immutable
caching. Auth routes are at `/auth/`, not `/api/auth/`.

## Scaling

```bash
docker compose up --scale worker=4
```

Workers claim jobs with `SELECT … FOR UPDATE SKIP LOCKED`, so replicas never
double-claim or double-charge. Keep one scheduler. Tune per-worker concurrency
with `WORKER_CONCURRENCY`.

## Building the image

```bash
docker build -t opengtm:local .
```

The Dockerfile builds the frontend from the Bun workspace (root `bun.lock`),
then installs the Python API on `python:3.13-slim` and copies `alembic.ini`
and `migrations/` so the image can apply migrations and RLS policies. Tagged
releases publish a multi-arch image to `ghcr.io/debpalash/opengtm`.

## Upgrading

```bash
git pull
docker compose build
docker compose up -d      # migrate runs first, then api/worker restart
```
