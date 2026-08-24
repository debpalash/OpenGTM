<p align="center">
  <img src="apps/web/public/opengtm-lockup.svg" alt="OpenGTM logo" width="280" />
</p>

# OpenGTM

**Go-to-Market Agents for atomic teams.**

OpenGTM is a free, open-source, self-hostable alternative to
[Clay.com](https://clay.com), licensed under AGPLv3.

Source leads, run enrichment waterfalls, research them with AI, and push the
results to your CRM, Sheets, or a webhook — all on your own infrastructure, with
your own provider keys, and with the bill shown to you *before* you run.

---

## Why OpenGTM

- **See the exact bill before you run.** Every workbook has a spend estimate
  endpoint that prices a run as `rows × providers = $X` with a per-column
  breakdown of paid providers, so the UI can gate the run behind a confirmation
  ("N rows, worst-case $X — proceed?"). Set a **spend ceiling** and paid
  providers are skipped once the budget is spent.
- **Bring your own keys (BYOK).** LLM providers, enrichment vendors, and
  destinations are all configured with *your* API keys via the Settings UI (or
  `.env`). OpenGTM is the engine; you own the spend and the data.
- **Self-host the whole thing.** No seats, no per-credit markup, no data leaving
  your box. AGPLv3.

---

## Our promises (in writing)

These are structural commitments, not marketing — the whole point of OpenGTM is
that a closed, seat-priced, credit-metered incumbent cannot match them without
undoing its own business model:

- **The REST API, webhooks, and MCP tools are never plan-gated.** Automating
  OpenGTM from your terminal, your own agent, or n8n is a first-class use, not an
  upsell. Metering the API is the single most-hated move of the tools we're an
  alternative to; we commit, in writing, never to make it.
- **BYOK at direct cost, zero markup.** You pay the LLM/enrichment vendor
  directly with your own key. Any future managed-key option bills at provider
  cost plus one disclosed flat fee — never a per-credit markup.
- **See the bill before you run — always.** The spend estimate and per-provider
  cost ledger are core, not a premium tier.
- **Self-host is fully functional, forever.** No feature is held back to force a
  cloud upgrade; the paid cloud line will be governance (SSO/audit/DPA/support),
  never capability.

---

## What it does

OpenGTM is a spreadsheet-shaped enrichment engine ("workbooks") plus an agentic
layer that can build and run those workbooks for you.

| Capability | What it is | Status |
|---|---|---|
| **Lead sourcing** | ~90 discovery sources plus typed connectors. Durable connector runs page, checkpoint, resume, deduplicate by provider record ID, and report whether the requested target was actually met | Implemented |
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
| **Multi-tenancy** | Workspace roles at the API boundary plus fail-closed PostgreSQL RLS on leads, signals, workbooks, connector runs, automations, outreach, ingest, and audit data | Implemented for the single-node deployment; control-plane HA remains |
| **Spend transparency** | Pre-run cost estimate + per-provider cost/yield ledger + spend ceiling | Implemented |
| **SSRF-guarded scraping** | Tenant-facing custom HTTP, scraper, webhook, research, and website-enrichment paths reject private/loopback/metadata targets and re-check redirects/browser requests | Implemented; hosted deployments should add an egress proxy |

### Workbook column types

`lead_field` · `source` · `enrichment` · `waterfall` · `ai_formula` · `agent` ·
`conditional` · `output` · `research` · `http` (call any API + JSONPath extract) ·
`formula`.

---

## Quickstart

One command brings up the full stack — API, enrichment worker, **Postgres**,
Redis, and nginx — with sane defaults:

```bash
git clone https://github.com/debpalash/lead-data.git opengtm
cd opengtm
cp .env.example .env        # fill in at least one LLM key; everything else is optional/BYOK
docker compose up           # API + worker + Postgres + Redis + nginx
```

To point at an existing database instead of the bundled Postgres, set
`DATABASE_URL` in `.env` (SQLite is also supported for local dev).

### Stable local URLs with Portless

[Portless](https://github.com/vercel-labs/portless) is included as a development
dependency. It gives the Vite app a stable `opengtm` hostname and can also alias
the Docker/nginx service that already listens on port 3010:

```bash
# Local machine: HTTPS at https://opengtm.localhost
bun --cwd apps/web dev

# Existing Docker stack: register its stable name
bun run portless:docker

# LAN sharing on an unprivileged port (Linux requires avahi-utils)
portless proxy stop
bun run portless:lan
bun run portless:docker
# http://opengtm.local:1355
```

Run `bun run portless:list` to inspect routes and `bun run portless:doctor` to
check proxy, DNS, certificates, and route health. A root-installed proxy may use
ports 80/443 for a URL without `:1355`.

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

Background enrichment and source imports run on the durable SQL-queue worker:
`python -m apps.api.worker` (claims `jobs` atomically; scale with
`docker compose up --scale worker=N`). Redis is optional for single-process
development and carries tenant-isolated live progress plus bounded reconnect
history in Compose. A scheduler recovers stale jobs and enqueues
recurring refreshes; it does not execute user work itself.

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
| Queue / pub-sub | Postgres job queue + isolated worker processes; Redis pub/sub for live UI updates |
| Frontend | React, TypeScript, ShadcnUI, Tailwind, Vite |
| Scraping | httpx, curl-cffi, Playwright, BeautifulSoup |
| Agentic chat | CopilotKit-style chat protocol with a gated ReAct tool loop |
| Monorepo | Turborepo + Bun |

### Durable source orchestration

```text
REST / agent tool
      │ validates workspace role and source query
      ▼
connector_runs + jobs  ── one atomic transaction
      │
      ▼
SQL queue worker ── fetch page ── normalize typed records
      │                              │
      ├── checkpoint cursor          └── stable provider + record identity
      ▼
workbook_rows upsert + run counters ── one page transaction
      │
      └── Redis event → workbook table + progress indicator
```

Connector adapters return a shared page/collection contract with source totals,
pagination state, exhaustion, warnings, and explicit partial-result status. A
worker commits each page and its cursor together, so a crash repeats at most one
idempotent upsert instead of losing or duplicating rows. Postgres enforces unique
`(workbook_id, source_provider, source_record_id)` identities, and both rows and
run history are protected by fail-closed workspace RLS.

See [`docs/architecture.md`](docs/architecture.md) for the complete process/data
ownership map, queue state machine, tenant boundary, failure behavior, and
scaling limits.

---

## Not yet / Roadmap

The production data plane is tenant-scoped and PostgreSQL/RLS protected, and all
user work runs through the durable worker. The remaining boundaries are:

- **The workspace control plane is single-node.** Workspace membership, active
  workspace selection, encrypted per-workspace secrets, and the detailed
  collection-stage ledger live in SQLite files on the shared `data/` volume.
  This is reliable for the documented Compose topology, but replicas on
  separate hosts need those stores moved to PostgreSQL first.
- **A few legacy utilities are global and admin-only.** The document queue,
  legacy person/scrape history, CRM-data utility, and reusable-function catalog
  are isolated from normal workspace users rather than fully tenantized.
- **Hosted-SaaS hardening is not complete.** Before exposing OpenGTM to mutually
  hostile public tenants, add a controlled outbound egress proxy, managed KMS,
  SSO/audit export, backup/restore drills, and an external security review.
- **Billing is an optional mechanism, not a hosted billing operation.** The
  feature-flagged credit ledger, idempotent debit, 402 gate, and Stripe top-up
  webhook exist. Tax, refunds, subscriptions, and customer lifecycle operations
  do not.
- **Clay's breadth and polish remain a product gap.** The open provider catalog,
  very-large-grid ergonomics, Clay-table migration, reactive dependency
  recomputation, templates, integrations, and real-user accuracy benchmarks
  need continued work.

See [`docs/clay-parity-specs.md`](docs/clay-parity-specs.md) for the work-item
breakdown and sequencing.

---

## License & "can I use this at work?"

OpenGTM is licensed under the **GNU Affero General Public License v3.0**
([`LICENSE`](LICENSE)).

In plain terms:

- **Self-host it for your own team — free, no strings on internal use.** Use it,
  modify it, run it on your own servers.
- **The AGPL network clause applies only if you offer OpenGTM (or a modified
  version) *as a service to other people over a network.*** In that case you must
  make your modified source available to those users. Running it internally does
  not trigger that obligation.

This is not legal advice — read the [full license](LICENSE) if you plan to offer
it as a hosted service.

---

## Docs & Contributing

- [`docs/clay-parity-specs.md`](docs/clay-parity-specs.md) — Clay-parity
  implementation specs and roadmap (WI-1…WI-10)
- [`docs/architecture.md`](docs/architecture.md) — runtime topology, data
  ownership, queue semantics, tenant isolation, and scale boundaries
- [`docs/sources-master-inventory.md`](docs/sources-master-inventory.md) —
  every data source, where it lives, and quality notes
- [`docs/clay-alternatives-research.md`](docs/clay-alternatives-research.md) /
  [`docs/clay-alternatives-ingestion-catalog.md`](docs/clay-alternatives-ingestion-catalog.md)
  — competitive landscape and ingestion catalog
- [`docs/data-source-test-report.md`](docs/data-source-test-report.md) — source
  health/test results

Contributions welcome — open an issue or a PR. Code is grounded with file/line
references in the docs above; start there to find the right entry point.
