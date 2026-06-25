# Yupcha

**A free, open-source, self-hostable alternative to [Clay.com](https://clay.com) (AGPLv3).**

Source leads, run enrichment waterfalls, research them with AI, and push the
results to your CRM, Sheets, or a webhook — all on your own infrastructure, with
your own provider keys, and with the bill shown to you *before* you run.

---

## Why Yupcha

- **See the exact bill before you run.** Every workbook has a spend estimate
  endpoint that prices a run as `rows × providers = $X` with a per-column
  breakdown of paid providers, so the UI can gate the run behind a confirmation
  ("N rows, worst-case $X — proceed?"). Set a **spend ceiling** and paid
  providers are skipped once the budget is spent.
- **Bring your own keys (BYOK).** LLM providers, enrichment vendors, and
  destinations are all configured with *your* API keys via the Settings UI (or
  `.env`). Yupcha is the engine; you own the spend and the data.
- **Self-host the whole thing.** No seats, no per-credit markup, no data leaving
  your box. AGPLv3.

---

## What it does

Yupcha is a spreadsheet-shaped enrichment engine ("workbooks") plus an agentic
layer that can build and run those workbooks for you.

| Capability | What it is | Status |
|---|---|---|
| **Lead sourcing** | ~90 data-driven discovery sources (DDG `site:`-scoped directories, B2B marketplaces, review/jobs/registry sites) materialize new rows from an ICP query | Implemented |
| **Enrichment waterfalls** | A column tries providers in sequence (cost-ordered) until one returns a confident value, with cross-row caching and confidence early-exit | Implemented |
| **Providers** | ~35 enrichment providers (32 built-in + declarative YAML manifests) across email find/verify, phone, firmographics, decision-makers, social, tech-stack, hiring, IP/domain, scoring | Implemented |
| **AI columns** | LLM transforms over row data (classify, rewrite, extract) | Implemented |
| **Research / "Claygent" columns** | A bounded ReAct agent that *browses the web* per row to answer a question and cite a source | Implemented |
| **Agent columns** | Goal-directed enrichment — the agent picks tools dynamically and records a reasoning trace per cell | Implemented |
| **Agentic chat / autopilot** | CopilotKit-style chat that can draft a plan from a goal ("build a list of 50 IT firms in Pune and find founders' emails") and execute it: create a sourcing workbook, add agent columns, set refresh | Implemented |
| **Output / push loop** | `output` columns push each row to a **webhook, HubSpot, Salesforce, Google Sheets, Airtable, or an email sequencer** — run-once idempotent, executed after enrichment | Implemented |
| **Email outreach** | Multi-step SMTP sequences with per-lead state and rate limiting | Implemented |
| **Buying signals** | Detects 7 signal types (hiring, funding, tech change, website change, news, growth, social) | Implemented |
| **Dedup** | Blocking + Jaro-Winkler fuzzy matching to find/merge duplicate leads | Implemented |
| **Multi-tenancy** | Workspaces with members/roles; lead data carries `workspace_id` | Partial — see Roadmap |
| **Spend transparency** | Pre-run cost estimate + per-provider cost/yield ledger + spend ceiling | Implemented |
| **SSRF-guarded scraping** | All user-supplied URLs (scraper, webhooks, research fetches) pass a public-URL allowlist that blocks private/loopback/metadata IPs | Implemented |

### Workbook column types

`lead_field` · `source` · `enrichment` · `waterfall` · `ai_formula` · `agent` ·
`conditional` · `output` · `research` · `http` (call any API + JSONPath extract) ·
`formula`.

---

## Quickstart

> **Status: setup is being hardened.** The intended experience is one command.
> The Docker Compose stack currently brings up the API, enrichment worker, Redis,
> and nginx; the bundled Postgres service is being added in a parallel change. If
> `docker compose up` fails on the database, point `DATABASE_URL` at your own
> Postgres (see `.env.example`) until that lands.

```bash
git clone https://github.com/yupcha-internal/lead-data.git
cd lead-data
cp .env.example .env        # fill in at least one LLM key; everything else is optional/BYOK
docker compose up           # API + worker + Redis + nginx
```

Then open **http://localhost:3000**.

You need **at least one LLM provider key** for AI/agent/research features
(OpenRouter, Google AI, Groq, Cerebras, NVIDIA, Mistral, and GitHub Models all
have free tiers — see `.env.example`). Enrichment vendors (Hunter, Apollo,
AbstractAPI, NumVerify, IPInfo, LeadMagic, …) and destinations (HubSpot,
Salesforce, Sheets, Airtable) are all optional and configurable at runtime in
**Settings → API Keys**.

### Local dev (without Docker)

```bash
uv sync                                                      # Python backend deps
uv run uvicorn apps.api.main:app --reload --port 8000       # API
cd apps/web && bun install && bun run dev                    # frontend
```

Background enrichment runs on the durable SQL-queue worker:
`python -m apps.api.worker` (claims `jobs` atomically; scale with
`docker compose up --scale worker=N`). Redis is optional — only used for live
WebSocket cell-update broadcasts.

---

## Architecture

```
apps/
├── api/          FastAPI backend (Python 3.11+)
│   ├── routers/  HTTP/WS endpoints (auth, leads, workbooks, copilotkit,
│   │             signals, scraper, crm, outreach, settings, analytics, …)
│   ├── services/ workbook engine, enrichment providers + waterfalls,
│   │             agent/autopilot, crm, outreach, signals, workspace, dedup
│   └── core/     config, SSRF url_guard, shared utilities
└── web/          React + TypeScript + ShadcnUI + Tailwind (Vite)
```

| Layer | Technology |
|---|---|
| API | FastAPI, SQLAlchemy 2 |
| Database | Postgres (recommended; SQLite supported for dev) |
| Queue / pub-sub | Redis + BullMQ worker |
| Frontend | React, TypeScript, ShadcnUI, Tailwind, Vite |
| Scraping | httpx, curl-cffi, Playwright, BeautifulSoup |
| Agentic chat | CopilotKit-style chat protocol with a gated ReAct tool loop |
| Monorepo | Turborepo + Bun |

---

## Not yet / Roadmap

Honest about the edges. The enrichment engine, output loop, AI/research/agent
columns, and spend transparency are production-ready. These are not:

- **Multi-workspace isolation is incomplete.** Workspaces, members, and roles
  exist, and lead rows carry `workspace_id`, but some write paths still hit a
  shared store: collection-job lead writes and inline enrichment write-back
  (WI-1/WI-3 partial) and buying-signal storage (WI-2) are not yet
  workspace-scoped. **Treat the current build as single-tenant** until this lands.
- **Per-workspace integration credentials (WI-6) are deferred.** CRM/SMTP/Sheets
  tokens are read from global settings today, so all workspaces share one set of
  destination keys.
- **Billing / credits / Stripe (WI-9) are not implemented.** LLM usage is
  *recorded* but never *enforced* — there is no credit balance, no metering gate,
  and no Stripe. The spend ceiling protects against runaway *vendor* spend, not
  internal credits.
- **Postgres migration (WI-10) is partial.** The app runs on Postgres via
  `DATABASE_URL`, but some legacy lead/workspace/signal data still lives in
  per-file SQLite stores; full unification onto Postgres is in progress.

See [`docs/clay-parity-specs.md`](docs/clay-parity-specs.md) for the work-item
breakdown and sequencing.

---

## License & "can I use this at work?"

Yupcha is licensed under the **GNU Affero General Public License v3.0**
([`LICENSE`](LICENSE)).

In plain terms:

- **Self-host it for your own team — free, no strings on internal use.** Use it,
  modify it, run it on your own servers.
- **The AGPL network clause applies only if you offer Yupcha (or a modified
  version) *as a service to other people over a network.*** In that case you must
  make your modified source available to those users. Running it internally does
  not trigger that obligation.

This is not legal advice — read the [full license](LICENSE) if you plan to offer
it as a hosted service.

---

## Docs & Contributing

- [`docs/clay-parity-specs.md`](docs/clay-parity-specs.md) — Clay-parity
  implementation specs and roadmap (WI-1…WI-10)
- [`docs/sources-master-inventory.md`](docs/sources-master-inventory.md) —
  every data source, where it lives, and quality notes
- [`docs/clay-alternatives-research.md`](docs/clay-alternatives-research.md) /
  [`docs/clay-alternatives-ingestion-catalog.md`](docs/clay-alternatives-ingestion-catalog.md)
  — competitive landscape and ingestion catalog
- [`docs/data-source-test-report.md`](docs/data-source-test-report.md) — source
  health/test results

Contributions welcome — open an issue or a PR. Code is grounded with file/line
references in the docs above; start there to find the right entry point.
