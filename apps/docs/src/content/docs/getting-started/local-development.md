---
title: Local development
description: Run the API, worker and frontend directly on your machine without Docker.
sidebar:
  order: 3
---

## Prerequisites

- Go (the version in `apps/backend-go/go.mod`)
- [Bun](https://bun.sh) 1.x (each app under `apps/` is installed on its own; there is no root workspace)
- PostgreSQL 16+ and Redis (`make up` starts them with the rest of the Compose stack)
- Optional: Python 3.13 and [uv](https://docs.astral.sh/uv/) for the browser service

## Backend

```bash
cp .env.example .env              # set SECRET_KEY, INTERNAL_SERVICE_TOKEN, DATABASE_URL, ...
make migrate                      # apply the schema (DATABASE_URL = owner credentials)
make run                          # build web UI + Go binaries, start api + worker (.run/*.log)
```

`make run` uses the shell's environment. The API listens on `API_ADDR`
(default `:8000`) and serves the embedded UI; set `SERVE_UI=false` to serve the
API only. Recurring jobs need the scheduler:

```bash
GO_SCHEDULER_ENABLED=true bin/opengtm-scheduler
```

Set `GO_ALL_NATIVE=true` to turn on every Go route and executor flag, as
Compose does. `make kill` stops what `make run` and `make dev` started.

## Browser service

Browser-backed work (rendering, Google Maps, scraping) calls the internal
browser service:

```bash
cd apps/browser-service
uv sync && uv run playwright install --only-shell chromium
BROWSER_SERVICE_TOKEN=$(openssl rand -hex 32) uv run python -m browser_service
```

Point the Go processes at it with `BROWSER_SERVICE_URL` and the same
`BROWSER_SERVICE_TOKEN`.

## Frontend

```bash
bun install --cwd apps/web  # once
bun run --cwd apps/web dev  # https://opengtm.localhost via Portless
bun run --cwd apps/web dev:app   # plain Vite on http://localhost:4099 (or `make dev` from the repo root)
```

## Tests

```bash
make test                                              # go vet + go test, web lint + build
cd apps/backend-go && go test -tags integration ./...  # needs TEST_DATABASE_URL / TEST_REDIS_URL
cd apps/browser-service && uv run pytest               # browser service
```

## Docs site

```bash
bun run --cwd apps/docs dev          # http://localhost:4321
bun run --cwd apps/docs build
bun run --cwd apps/docs preview --ignore-lock   # Astro 7.3: several previews side by side
```

See the [contributing guide](/community/contributing/) for the pull-request
checklist and commit conventions.
