---
title: Quickstart (Docker)
description: Bring up the full OpenGTM stack with Docker Compose in a few minutes.
sidebar:
  order: 2
---

One command brings up the API, the enrichment worker, PostgreSQL, Redis and
nginx with sane defaults.

## Requirements

- Docker Engine 24+ with the Compose plugin (`docker compose version`)
- 2 CPU / 4 GB RAM for a comfortable single-node install
- At least one LLM provider key for AI, research and agent features. OpenRouter,
  Google AI, Groq, Cerebras, NVIDIA, Mistral and GitHub Models all have free
  tiers; enrichment vendors and CRM destinations are optional and can be added
  later from **Settings → API Keys**.

## Install

```bash
git clone https://github.com/debpalash/opengtm.git
cd opengtm
cp .env.example .env     # add at least one LLM key; everything else is optional
docker compose up        # API + worker + Postgres + Redis + nginx
```

Then open **http://localhost:3000**.

On first boot the `seed` service creates an admin login and a demo workbook
wired to zero-key (free) providers. The defaults are `admin` / `admin`; change
`SEED_ADMIN_PASSWORD` in `.env` for anything that is not a laptop.

## What just started

| Service | Purpose |
|---|---|
| `postgres` | Primary data plane: users, workbooks, jobs, cells. RLS-protected. |
| `redis` | Live progress channels for the UI and bounded reconnect history. |
| `migrate` | One-shot Alembic `upgrade head` with the schema-owner role. |
| `api` | FastAPI: enqueue and query only; it never runs user work inline. |
| `worker` | Claims jobs from the durable PostgreSQL queue and runs enrichment, imports, refreshes. |
| `scheduler` | Recovers stale jobs and enqueues recurring work. Run exactly one. |
| `nginx` | Serves the React bundle and proxies `/api` on port 3000. |

Scale enrichment throughput with more workers:

```bash
docker compose up --scale worker=4
```

## Using an existing database

Set `DATABASE_URL` in `.env` to point at your own PostgreSQL (the URL must use
the `postgresql+psycopg://` scheme). Compose also supports a separate,
non-superuser `APP_DATABASE_URL` for API and worker traffic so forced
row-level security is effective. SQLite is supported for local development
only; it has no concurrent writers.

## Next

- [Your first workbook](/getting-started/first-workbook/)
- [Configuration reference](/self-hosting/configuration/) for every environment
  variable
- [Production checklist](/self-hosting/production/) before exposing the install
  to a network
