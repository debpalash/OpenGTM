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

## Low-memory install

The default stack is small except for the browser service (Playwright and
Chromium). `docker-compose.lite.yml` is an opt-in override that does not start
it. The default `docker compose up` is unchanged.

```bash
docker compose -f docker-compose.yml -f docker-compose.lite.yml up -d --build
# or put COMPOSE_FILE=docker-compose.yml:docker-compose.lite.yml in .env
```

Measured on Docker Compose v5.8.0 (Linux, amd64) with `docker stats --no-stream`,
three samples 10 to 15 seconds apart after every service was healthy, empty
database, nothing running. Memory is idle working set, not a peak: under load the
browser service can use up to its `BROWSER_MEM_LIMIT` (default 2 GB).

| Service | Default (MiB) | Lite (MiB) | Image size |
|---|---|---|---|
| `browser` | 285.7 | not started | 1.91 GB |
| `postgres` | 48.4 | 52.4 | 294 MB |
| `nginx` | 16.1 | 15.8 | 63 MB |
| `worker-go` | 13.8 | 12.5 | 37.9 MB |
| `scheduler-go` | 12.5 | 8.3 | 28 MB |
| `api-go` | 11.7 | 15.6 | 48.8 MB |
| `redis` | 8.2 | 8.4 | 39.1 MB |
| `migrate` (exits after start) | 0 | 0 | 13.1 MB |
| **Total** | **396** | **113** | **2.43 GB** vs **0.52 GB** |

Image sizes are the uncompressed sizes `docker images` reports. Idle memory
grows with data and traffic, so treat these as a floor.

What you give up without the browser service (verified by running the override):

- `collect`, `bulk_enrich`, `source_health_check` and workbook collection-query
  jobs. The worker logs `collect pipeline not wired` at start, and each such job
  ends `failed` with `collect cannot run: BROWSER_SERVICE_URL / BROWSER_SERVICE_TOKEN not set`.
  `POST /api/collect` still answers 200 and enqueues the job; it is refused later,
  not up front.
- `POST /api/v2/scraper/scrape` answers `browser service is not available`.
  Google Maps and LinkedIn roster discovery and the browser tier of web fetch use
  the same service.

Everything that does not render pages keeps working: the UI, CSV import,
workbooks, API-backed providers, LLM columns and outreach. This was not
exercised with real provider keys. `GET /api/system-stats` has no field that says
the browser service is absent; it simply omits its `browser` counters.

To add it back, drop the second `-f` (or `COMPOSE_FILE`) and run
`docker compose up -d`.

## Running two stacks on one machine

Compose names containers `<project>-<service>-1`, so give each copy its own
project name, host ports and network subnet:

```bash
docker compose -p gtm-a --env-file a.env up -d   # a.env: PORT=3000 API_GO_PORT=8001 POSTGRES_PORT=5432 REDIS_PORT=6379
docker compose -p gtm-b --env-file b.env up -d   # b.env: PORT=3100 API_GO_PORT=8101 POSTGRES_PORT=5532 REDIS_PORT=6479 LEAD_NET_SUBNET=172.29.251.0/24
```

`lead_net` has a fixed subnet (`LEAD_NET_SUBNET`, default `172.29.250.0/24`)
because `TRUST_PROXY_CIDRS` trusts nginx's `X-Real-IP` by that range. Two stacks
with the same subnet fail with `Pool overlaps with other one on this address
space`. `scripts/install.sh` writes a free `172.29.25x.0/24` into a new `.env`
when the default is taken. Changing the subnet of an existing install only takes
effect when its network is recreated (`docker compose down`, then `up`); data
volumes are kept.

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
