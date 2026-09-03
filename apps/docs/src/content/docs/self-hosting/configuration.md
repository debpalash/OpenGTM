---
title: Configuration
description: Every environment variable, grouped, with defaults. Copy .env.example and change what you need.
sidebar:
  order: 2
---

Settings load from `.env` then `.env.local` (the latter wins) and from the
environment. Provider keys can also be set per workspace in
**Settings → API Keys**, which overrides these values.

## Core

| Variable | Default | Meaning |
|---|---|---|
| `SECRET_KEY` | insecure default | JWT signing key. Boot fails on the default outside dev |
| `APP_ENV` | `dev` | `dev`, `test`, `local` tolerate the default key; anything else fails closed |
| `ACCESS_TOKEN_EXPIRE_MINUTES` | 30 | Access-token TTL |
| `REFRESH_TOKEN_EXPIRE_MINUTES` | 20160 | Refresh-token TTL (14 days) |
| `API_HOST`, `API_PORT` | `0.0.0.0`, 8000 | Bind address for uvicorn |
| `CORS_ORIGINS` | `*` | Allowed origins |
| `LOG_LEVEL` | `info` | Logging |
| `PORT` | 3000 | Host port for nginx in Compose |

## Database and tenancy

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | `postgresql+psycopg://localhost:5432/yupcha` | Schema-owner connection (migrations). SQLite `sqlite:///data/data.db` works for single-process dev |
| `APP_DATABASE_URL` | runtime role URL in Compose | Non-superuser connection for API, worker, scheduler |
| `APP_DB_ROLE` | `yupcha_app` | Expected runtime role |
| `PG_LEAD_STORE` | on | Use the shared RLS-protected lead tables on PostgreSQL |
| `PG_RLS_REQUIRE_SAFE_ROLE` | on | Refuse to boot if the role bypasses RLS |
| `YUPCHA_DB_INIT` | `alembic` | `alembic`, `create_all` (dev only) or `skip` |
| `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB`, `POSTGRES_PORT` | `yupcha` / `yupcha` / `yupcha` / 5432 | Bundled Postgres container |
| `YUPCHA_RUNTIME_DB_USER`, `YUPCHA_RUNTIME_DB_PASSWORD` | `yupcha_runtime` | Runtime role provisioned by `migrate` |
| `REDIS_URL` | unset | `redis://redis:6379` in Compose; optional for single-process dev |
| `RUN_INLINE_WORKER` | 0 in Compose | Set to 1 only for single-process development |
| `WORKER_CONCURRENCY` | 10 | Concurrent jobs per worker |
| `SCHEDULER_RECONCILE_SECONDS` | 60 | Stale-job reconciliation interval |

## Secrets and billing

| Variable | Default | Meaning |
|---|---|---|
| `SECRETS_MASTER_KEY` | derived from `SECRET_KEY` | Fernet key for per-workspace secrets |
| `BILLING_ENABLED` | off | Credit ledger and 402 gate; off means runs are never blocked |
| `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET` | | Stripe top-ups |
| `OUTREACH_SEND_COST_USD`, `POLLER_*_COST_USD` | 0.0 | Platform-metered costs |

## First run

| Variable | Default | Meaning |
|---|---|---|
| `SEED_ADMIN_USERNAME`, `SEED_ADMIN_PASSWORD` | `admin` / `admin` | Created by the `seed` service; change the password |
| `SEED_FORCE_RUN` | | Re-run the demo enrichment on next boot |

## LLM providers

`<PROVIDER>_API_KEY`, `<PROVIDER>_MODEL` and `<PROVIDER>_BASE_URL` for
`ANTHROPIC`, `CEREBRAS`, `GROQ`, `SAMBANOVA`, `NVIDIA`, `MISTRAL`,
`OPENROUTER`, `GITHUB_MODELS`, `SILICONFLOW`, plus `GOOGLE_AI_API_KEY`.
`LLM_DEFAULT_PROVIDER`, `YUPCHA_CLOUD`, `LLM_PROMPT_CACHE`,
`LLM_HTTP_TIMEOUT`, `CHAT_MAX_TOOL_ROUNDS`. See
[LLM providers](/guides/llm-providers/).

## Enrichment and destination keys

`HUNTER_API_KEY`, `APOLLO_API_KEY`, `SNOVIO_CLIENT_ID`, `SNOVIO_CLIENT_SECRET`,
`PROSPEO_API_KEY`, `PDL_API_KEY`, `ABSTRACT_API_KEY`, `DEBOUNCE_API_KEY`,
`NUMVERIFY_API_KEY`, `IPINFO_TOKEN`, `GOOGLE_MAPS_API_KEY`,
`LEADMAGIC_API_KEY`, `COMPANIES_HOUSE_API_KEY`, `DATA_GOV_IN_KEY`,
`SEC_EDGAR_USER_AGENT`, `STAFFSPY_ENABLED`, `STAFFSPY_SESSION_FILE`,
`GOOGLE_API_KEY`, `GOOGLE_CSE_ID`, `GOOGLE_PLACES_API_KEY`.

`HUBSPOT_TOKEN`, `SALESFORCE_INSTANCE_URL`, `SALESFORCE_ACCESS_TOKEN`,
`AIRTABLE_TOKEN`, `GOOGLE_SHEETS_TOKEN`, `INSTANTLY_API_KEY`,
`SMARTLEAD_API_KEY`. See [Providers](/guides/providers/) and
[Outputs](/guides/outputs/).

`REACHER_ENABLED`, `REACHER_URL`, `REACHER_API_KEY`, `REACHER_TIMEOUT`,
`REACHER_BREAKER_THRESHOLD`, `REACHER_BREAKER_COOLDOWN`,
`REACHER_FROM_EMAIL`, `REACHER_HELLO_NAME`, `REACHER_PROXY_*`.

`SMTP_HOST`, `SMTP_PORT`, `SMTP_EMAIL`, `SMTP_PASSWORD`, `SMTP_FROM_NAME`,
`SMTP_MAX_PER_HOUR` (global fallback for outreach).

`PROXY_LIST`, `PROXY_MANAGER_URL` for scraping at scale.

## Workbook engine

| Variable | Default |
|---|---|
| `WORKBOOK_PROVIDER_WORKERS` | 8 |
| `WORKBOOK_PROVIDER_TIMEOUT` | 10 seconds |
| `RESEARCH_CELL_BUDGET_USD` | 0.05 |
| `RESEARCH_NATIVE_TOOLS` | off |
| `RESEARCH_VENDOR_COST` | off |

## Feature flags (all off by default)

| Flag | Enables |
|---|---|
| `AUTOMATIONS_ENABLED` | [Automations](/guides/automations/) |
| `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` | `sequencer` / `send_email` actions |
| `INTENT_POLLER_ENABLED` | [Watches](/guides/signals/#watches-intent-poller) |
| `INGEST_API_ENABLED` | [Ingest API](/guides/ingest-api/) |
| `MCP_WRITE_ENABLED` | MCP write tools |
| `CRM_IMPORT_ENABLED` | `source.kind: crm_import` |
| `PEOPLE_SEARCH_SOURCE_ENABLED` | `source.kind: people_search` |
| `PROVENANCE_TRACKING_ENABLED` | Per-field provenance |
| `TECH_STACK_WEBSITE_FETCH_ENABLED` | `tech_stack` provider fetches (with `TECH_STACK_RESPECT_ROBOTS`, `TECH_STACK_INSECURE_TLS`) |
| `COMPANY_SIZE_HEURISTIC_ENABLED` | Keyless size inference |
| `SOURCE_RELIABILITY_RANKING`, `SOURCE_HEALTH_ENABLED`, `SOURCE_HEALTH_ENFORCE` | [Source reliability and health](/guides/sourcing/#reliability-and-health-opt-in) |

## Limits and budgets

Automations: `AUTOMATIONS_MAX_RULES_PER_WS` (50),
`AUTOMATIONS_MAX_ACTIONS_PER_RULE` (10), `AUTOMATIONS_MAX_ROWS_PER_EVAL`
(500), `AUTOMATIONS_GLOBAL_DAILY_USD` (0), `AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST`.

Signals: `SIGNAL_SCAN_MAX_HOT_LEADS` (50), `SIGNAL_SCAN_MAX_WARM_LEADS` (30),
`SIGNAL_SCAN_MAX_LEADS_PER_WORKSPACE` (20), `SIGNAL_SCAN_GLOBAL_MAX_LEADS` (500).

Outreach: `OUTREACH_TICK_INTERVAL` (900), `OUTREACH_TICK_MAX_ENQUEUE` (200),
`OUTREACH_UNSUB_TTL_DAYS` (90), `OUTREACH_SOFT_BOUNCE_MAX` (3),
`OUTREACH_BOUNCE_PAUSE_RATE` (0.05), `OUTREACH_COMPLAINT_PAUSE_RATE` (0.003),
`OUTREACH_CIRCUIT_MIN_SENDS` (20), `OUTREACH_PUBLIC_BASE_URL`,
`OUTREACH_BOUNCE_WEBHOOK_SECRET`, `OUTREACH_INBOUND_POLL_ENABLED`,
`OUTREACH_INBOUND_POLL_INTERVAL` (900), `OUTREACH_INBOUND_MAX_FETCH` (100),
`OUTREACH_INBOUND_MAX_CONSECUTIVE_FAILURES` (10),
`OUTREACH_INBOUND_LOOKBACK_DAYS` (3).

Intent poller: `INTENT_POLLER_DEFAULT_INTERVAL` (`daily`),
`INTENT_POLLER_MAX_WATCHES_PER_WS` (200), `INTENT_POLLER_DAILY_POLL_BUDGET`
(0), `INTENT_POLLER_POLL_NOW_DAILY_QUOTA` (50),
`INTENT_POLLER_POLL_NOW_MIN_INTERVAL_SEC` (60),
`INTENT_POLLER_MAX_CONSECUTIVE_FAILURES` (12), `INTENT_POLLER_JOBSPY_MAX_JOBS`
(5), `INTENT_POLLER_FEED_MAX_ENTRIES` (100),
`INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS_PER_POLL` (50),
`INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS` (500),
`INTENT_POLLER_MAX_FIRES_PER_SIGNAL` (200), `INTENT_POLLER_BACKFILL` (off).

MCP: `MCP_REQUIRE_AUTH`, `MCP_TOKEN_TTL_DAYS` (90), `MCP_MAX_WRITES_PER_DAY`
(0), `OPENGTM_MCP_TOKEN`, `OPENGTM_MCP_SSE_HOST` (`127.0.0.1`). Chat:
`CHAT_REQUIRE_AUTH`.
