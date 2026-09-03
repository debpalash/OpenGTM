---
title: CLI and entrypoints
description: The opengtm command, worker and scheduler processes, and maintenance scripts.
sidebar:
  order: 2
---

Installed as `opengtm` (with a legacy `yupcha` alias) by `uv sync`, or run as
`uv run python -m apps.api.cli <command>`.

| Command | Flags | What it does |
|---|---|---|
| `server` | `--port` (8000), `--reload` | Start the FastAPI server |
| `import` | | Import CSVs from the current directory into the lead store, then re-score |
| `scrape` | `--source`, `--city` (comma-separated) | Run scrapers, dedupe, upsert, score |
| `enrich` | `--limit` | Enrich leads with missing data, then re-score |
| `score` | | Re-score every lead |
| `pipeline` | `--source`, `--no-enrich` | Scrape, enrich and score in one go |
| `export` | `--format csv\|json`, `--output`, `--min-score`, `--status`, `--city`, `--tier` | Export leads (defaults to `data/leads_export.<format>`) |
| `stats` | | Database statistics |
| `dashboard` | `--port`, `--reload` | Alias for `server` |
| `collect` | `query`, `--workspace` / `-w` | Submit one collection query to the job runner; creates the workspace if missing |
| `jobs` | `--status` | List collection jobs |
| `cleanup` | | Validate leads, mark invalid ones dead with a `cleanup:<reason>` note |

Running with no subcommand prints help.

## Long-running processes

```bash
uv run python -m apps.api.worker       # durable SQL-queue worker (scale freely)
uv run python -m apps.api.scheduler    # stale-job recovery + recurring enqueue (run one)
uv run python -m apps.mcp.server       # MCP server on stdio; --sse [PORT] for HTTP
```

## Maintenance scripts

```bash
uv run python -m apps.api.scripts.migrate      # one-shot Alembic upgrade (Compose `migrate`)
uv run python -m apps.api.scripts.seed_demo    # idempotent first-run admin + demo workbook
uv run python -m apps.api.services.leadgen.enrichment.eval.cli --json      # provider accuracy ranking
uv run python -m apps.api.services.leadgen.enrichment.eval.cli --persist   # feed the planner prior
uv run python -m apps.api.services.leadgen.enrichment.eval.research_eval   # research-column eval gate
uv run python scripts/export_openapi.py        # regenerate the API reference
```

Migration commands are on the [migrations](/self-hosting/migrations/) page.
