---
title: Docker Compose
description: Every service in the bundled stack, the ports they use, the images, and how to scale.
sidebar:
  order: 1
---

`docker compose up` builds the Go images and the internal browser service and
starts them with PostgreSQL, Redis and nginx. There is no Python API, worker or
scheduler: Python runs only inside the browser service.

## Services

| Service | Image | Host port | Purpose |
|---|---|---|---|
| `postgres` | `postgres:16-alpine` | `127.0.0.1:5432` | Primary store; volume `pgdata` |
| `redis` | `redis:7-alpine` | `127.0.0.1:6379` | Live progress pub/sub; volume `redisdata`. No auth: never expose beyond loopback |
| `migrate` | Go, one-shot | | `opengtm-migrate up` as the schema owner; provisions the runtime role; everything else waits for it |
| `api-go` | Go | `127.0.0.1:8001` | REST API, auth, SSE/WebSocket and the embedded web UI |
| `worker-go` | Go | | Durable SQL-queue worker; scale with `--scale` |
| `scheduler-go` | Go | | Recurring jobs; a Postgres advisory lock keeps one instance active |
| `browser` | `apps/browser-service` | none | Internal browser/scraper service (Playwright Chromium). No database, no secrets besides its token, internal network only |
| `nginx` | `nginx:alpine` | `${PORT:-3000}` | Public entrypoint; proxies everything to `api-go` |
| `seed-go` | Go, one-shot | | Optional demo workbook, `--profile demo` (after `/setup`) |
| `mcp` | Go | none | Optional MCP server over SSE, `--profile mcp` |
| `reacher` | `reacherhq/backend:v0.10.0` | none | Optional email verification, `--profile reacher` |

`.env` must hold `SECRET_KEY`, `INTERNAL_SERVICE_TOKEN` and
`BROWSER_SERVICE_TOKEN`; `scripts/install.sh` generates them.

## Database roles

`migrate` connects as the schema owner (`DATABASE_URL`). The Go services
connect as `yupcha_runtime`, a role without superuser or `BYPASSRLS`, so forced
row-level security applies to all runtime traffic. Change `POSTGRES_PASSWORD`
and `YUPCHA_RUNTIME_DB_PASSWORD` for any shared install.

## nginx

Inside the container nginx listens on port 80 and resolves the `api-go`
upstream dynamically, so a long-lived proxy never pins a dead container IP
after a redeploy. `/api/` is proxied with WebSocket upgrade, 300-second
timeouts and buffering off for server-sent events; `/api/events` and the
workbook WebSocket get one-hour reads and never log their `?token=`. Auth
routes are at `/auth/`, not `/api/auth/`. `/internal/`, `/admin` and `/scim`
are not exposed.

## Browser service

The browser service is reachable only from the Go services on an internal
network, with a bearer token. Its own internet path goes through an in-process
egress proxy that refuses private and metadata addresses. Chromium runs with its
sandbox on, which needs the seccomp and AppArmor options Compose sets.

## Scaling

```bash
docker compose up -d --scale worker-go=4
```

Workers claim jobs with `SELECT … FOR UPDATE SKIP LOCKED`, so replicas never
double-claim or double-charge. Extra scheduler replicas idle. Tune per-worker
concurrency with `GO_WORKER_CONCURRENCY`.

## Images

All Go images come from `apps/backend-go/Dockerfile`: `--target api` (with
`--build-context web=apps/web`, so the UI is built with Bun and embedded),
`worker`, `mcp`, `scheduler` (also carries the demo seed) and `migrate`. The
browser service builds from `apps/browser-service`. Tagged releases publish
multi-arch images to `ghcr.io/debpalash/opengtm-api`, `-worker`, `-mcp`,
`-scheduler`, `-migrate` and `-browser`.

## Upgrading

```bash
git pull
docker compose up -d --build   # migrate runs first, then the services restart
```
