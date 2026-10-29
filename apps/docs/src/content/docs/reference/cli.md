---
title: CLI and entrypoints
description: The Go binaries, what each one runs, and the maintenance commands.
sidebar:
  order: 2
---

`make build` produces every binary in `bin/`. In Compose each one is its own
service or image target (see [Docker Compose](/self-hosting/docker/)).

| Binary | Compose service | What it does |
|---|---|---|
| `opengtm-api` | `api-go` | REST API, auth, SSE/WebSocket and the embedded web UI. `-healthcheck` probes `/healthz` |
| `opengtm-worker` | `worker-go` | Durable SQL-queue worker (scale freely) |
| `opengtm-scheduler` | `scheduler-go` | Recurring jobs; needs `GO_SCHEDULER_ENABLED=true`; one active instance (advisory lock) |
| `opengtm-mcp` | `mcp` | MCP server on stdio; `--sse [PORT]` for HTTP |
| `opengtm-migrate` | `migrate` | `up` (default), `status`, `version`: owns the schema; safe to run concurrently |
| `opengtm-seed` | `seed-go` | Opt-in demo workbook in the workspace `/setup` created; never creates a user |
| `suppressnorm` | (in the worker image) | One-shot normalization of outreach suppressions; `--dry-run` first |

## Maintenance

```bash
DATABASE_URL=postgresql://owner@host/db bin/opengtm-migrate status
docker compose run --rm migrate status
docker compose run --rm --entrypoint /suppressnorm worker-go --dry-run
docker compose --profile demo up seed-go
```

Migration details are on the [migrations](/self-hosting/migrations/) page.
