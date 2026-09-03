---
title: Local development
description: Run the API, worker and frontend directly on your machine without Docker.
sidebar:
  order: 3
---

## Prerequisites

- Python 3.11+ and [uv](https://docs.astral.sh/uv/)
- [Bun](https://bun.sh) 1.x (the monorepo uses Bun workspaces + Turborepo)
- PostgreSQL 15+ recommended; SQLite works for single-process development

## Backend

```bash
uv sync                                                 # install Python deps from uv.lock
cp .env.example .env
uv run uvicorn apps.api.main:app --reload --port 8000  # API on :8000
```

Background enrichment and source imports run on the durable SQL-queue worker.
Start one in a second terminal:

```bash
uv run python -m apps.api.worker
```

Optional extras:

```bash
uv run python -m apps.api.scheduler   # stale-job recovery + recurring refreshes
uv sync --extra browser               # patchright + stealth-requests for browser-tier fetches
uv run patchright install chromium
```

Redis is optional for single-process development; without it live progress
falls back to in-process queues.

## Frontend

```bash
bun install                # once, at the repo root
bun run --cwd apps/web dev # https://opengtm.localhost via Portless
bun run --cwd apps/web dev:app   # plain Vite on http://localhost:5173
```

`bun run dev` at the root starts every workspace app in parallel through
Turborepo.

## Tests

```bash
uv run pytest                        # hermetic backend suite (SQLite)
uv run pytest -m postgres            # RLS integration tests; needs a disposable Postgres
uv run alembic check                 # model ↔ migration drift
bun run lint && bun run build        # every TypeScript package, including this docs site
uv run python scripts/export_openapi.py --check   # API reference is current
```

Tests marked `live` need real network services or credentials and are opt-in.

## Docs site

```bash
bun run --cwd apps/docs dev          # http://localhost:4321
bun run --cwd apps/docs build
bun run --cwd apps/docs preview --ignore-lock   # Astro 7.3: several previews side by side
```

See the [contributing guide](/community/contributing/) for the pull-request
checklist and commit conventions.
