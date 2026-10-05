# RFC: Hybrid Go, Rust and Python platform with a plugin architecture

Updated: 2026-10-05
Status: foundations implemented on the `rewrite/hybrid-platform` branch (see
[Implementation status](#implementation-status)); four migrated job types and the
new `plugin_run` have Go executors that are opt-in per deployment; no existing API
route has moved to Go yet.
Tracking issue: [#33](https://github.com/debpalash/OpenGTM/issues/33).
Supersedes the Go/Python proposal previously at `go-python-backend-rewrite.md`.

## Decision

Rebuild OpenGTM step by step as a **hybrid platform** in which each language
owns the work it does best, behind versioned contracts:

- **Go** is the control plane: APIs, tenancy, orchestration, durable jobs,
  scheduling, the plugin host, and the single self-hosting binary.
- **Rust** owns measured data-plane hot paths: parsing, extraction,
  normalization, and matching. Kernels ship as WebAssembly modules inside the
  Go binary by default, and as native services only when benchmarks require it.
- **Python** owns AI research, browser automation, and code-first scrapers,
  running as isolated specialist workers.
- **TypeScript** owns the web UI and generated clients: React, shadcn/ui on Base
  UI, and the TanStack libraries.

Three goals shape every milestone alongside throughput:

1. **Easy to deploy and self-host.** A minimal install is one Go binary plus
   PostgreSQL. Larger profiles add services without changing the data model.
2. **Extensible by users.** Providers, scrapers, signal sources, destinations,
   workbook functions, and AI tools are plugins that use one manifest and run in
   one sandbox model. Users can write them in YAML, Python, Rust, Go, or
   TypeScript.
3. **Fast where it matters.** Bounded concurrency, pooled I/O, batched writes,
   and Rust kernels are admitted by benchmark, not by preference.

Deliver this through incremental replacement of job types, routes, and screens.
The first implementation milestone is still one production-compatible Go
enrichment worker, with parity tests, reproducible benchmarks, and a working
rollback.

## Implementation status

Built on `rewrite/hybrid-platform`, with tests against PostgreSQL 18 under
forced row-level security:

| Area | State |
| --- | --- |
| M1 foundations | Done: multi-role binary, validated configuration, tenant transactions, a Go executor for the shared `jobs` table, `job_executor_routes`, forward authorization, `LISTEN/NOTIFY` progress, an HTTP front door that proxies FastAPI and serves the dashboard, `doctor`. A cross-language test runs the Python and Go claimers on one database. |
| M3 plugin platform | Core done: manifest v2 (v1 connectors load unchanged), JSON Schema contract, guarded egress, declarative providers and scrapers, a WebAssembly host with capability enforcement, signing and `.ogc` bundles (byte-compatible with Python), and `opengtm plugin new/validate/test/record/run/pack/sign/verify/install/dev`. Since then: a signed plugin index with install (`OPENGTM_PLUGIN_INDEX`), per-workspace plugin secrets (Fernet `enc:v1`, `plugin_secrets` under forced RLS, write-only admin endpoints, fail closed on an unreadable secret) and the `process` runtime (M4 row). Not yet: `signal`/`destination` exports, index freshness checks, master key rotation. |
| First Go job type | `plugin_run`, a new job type rather than a migrated one, chosen so the full path (claim, tenant transaction, sandboxed run, lease-guarded result commit, progress, cancellation) is proven without a parity risk. Runs and results are served at `/api/v2/plugin-runs`. |
| Rust kernels | Normalization (domain, email, phone, name) and extraction, matching the Python normalizers on 5,814 parity cases. Run through wazero directly (the Extism SDK added 25–31 µs per call); the module is still a standard Extism plugin. |
| M5 web | Foundation done: TanStack Router with route-level code splitting, `twenty-ui` removed, a CI bundle budget (initial JavaScript 224.2 kB gzip against a 234.4 kB budget), a typed API client generated from `packages/contracts/openapi.v2.yaml` (OpenAPI 3.1; `bun run gen:api` / `check:api`, with Go drift tests in `internal/contract`), keyset-paged plugin runs, and a committed web baseline (`benchmarks/results/web-baseline.md`: bundle sizes, FCP/LCP/CLS/TBT, INP, route transitions). 118 web tests pass. Not yet: full route parity and the accessibility audit, schema-driven plugin settings, and a live-update path for Go-run workbooks (the grid still listens on Redis, Go publishes on `LISTEN/NOTIFY`). |
| FastAPI wedge | Fixed. Root cause: blocking lookups on the event loop inside async auth dependencies, plus connection-pool exhaustion. Auth lookups now run in the threadpool, with `DB_POOL_TIMEOUT` (default 10) and `THREADPOOL_SIZE` (default 100). The authenticated endpoint went from 0.5 req/s with 62 timeouts at 64 connections to 419 req/s at 64 and 396 at 128, with no errors. The Go authz client gained a timeout, singleflight, a concurrency bound and a circuit breaker. |
| M7 packaging | Done on amd64: `opengtm init` (profiles `lite`, `standard`, `full`), `migrate` (Go owner around Alembic), `backup`/`restore` (checksummed), `upgrade`/`rollback` (journaled; rollback restores the pre-upgrade backup), `doctor --dir`, GoReleaser configuration, a verifying `install.sh`, a Helm-free Kubernetes example, and the self-hosting and backup/upgrade/rollback guides. `scripts/packaging/e2e.sh` passed for all three profiles on real containers, including an upgrade across a real Alembic revision, automatic rollback of a failed upgrade and disaster recovery onto a new directory. Not yet: any arm64 run, a real tagged release, keyless signing (the accept path was tested only with a stub), the Kubernetes example applied to a cluster, restore time on a production-sized database, external PostgreSQL. Details in [M7 packaging](#m7-packaging-decisions-evidence-and-limits). |
| M0 baseline | Done (backend, web and job-type benchmarks): `benchmarks/run.sh` runs a reproducible queue and HTTP comparison of the Python stack and the Go server against a throwaway PostgreSQL database, writes JSON and Markdown, and `benchmarks/compare.py` fails on a regression above a threshold (default 15%) for later CI use. The committed snapshot is `benchmarks/results/baseline.md`. Headline numbers (one shared 32-thread machine, 3 repetitions, medians; read the caveats in `benchmarks/README.md`): with the fsync wait removed, the Go queue completes 351 / 946 / 1,257 no-op jobs/s at 1 / 4 / 16 slots against 209 / 393 / 371 for the Python `QueueService` with the handler in-process (1.7x / 2.4x / 3.4x), with cycle p50 of 2.7 / 4.0 / 13 ms against 4.8 / 9.9 / 43 ms. With durable commits both are bound by disk flushes (about 60 jobs/s at one slot on this disk) and only scale apart at 4 and 16 slots (2.4x, 3.9x). The production Python path spawns a child process per job and completes about 1.2 / 4.4 / 10.8 no-op jobs/s, about 0.8-1.2 s per job. The Go front door adds about 0.1-0.2 ms at p50 and 0-12% throughput when proxying FastAPI (`/health` about 1,000-2,300 req/s either way, capped by uvicorn); Go-owned `/api/v2/plugins` serves about 14,300 / 36,900 / 53,900 req/s at 1 / 16 / 64 connections (p50 0.06 / 0.33 / 1.0 ms). Found along the way: FastAPI's authenticated `/api/auth/workspace-context` stops answering at 64 concurrent connections. The web baseline is in `benchmarks/results/web-baseline.md`, the retention comparison in `benchmarks/results/retention.md` and the enrichment comparison, with a local provider simulator, in `benchmarks/results/enrich-baseline.md`. The FastAPI stall found here is fixed (FastAPI wedge row). Not yet: backend resource profiles for the queue and HTTP paths. |
| Migrated job types (M2, M6) | Four Go executors, all **Python-owned by default**: no migration seeds a route, an operator moves a type with `opengtm routes set <type> go` and back with `... python` (the Python handlers are untouched, so rollback is routing only; `routes set` refuses while jobs are processing unless `--force`, and refuses a type this binary cannot execute). Every commit is fenced by the job lease inside a tenant transaction under forced RLS. Each has a parity test: identical datasets in two fresh databases, the Python handler on one and Go on the other, identical resulting state. <br>• `retention_enforce` (`internal/jobs/retention`). <br>• `research_playbook_schedule` (`internal/jobs/playbooksched`) and `audience_refresh` (`internal/jobs/audiencerefresh`), on shared helpers in `internal/jobs/jobkit`. The parity suites run in four time-zone combinations; 11 deliberate breaks of the Go code were all caught. <br>• `run_workbook_connector` (`internal/jobs/enrich`, M2): a new job type for explicit waterfalls of manifest v1 connectors over rows not linked to a lead; `/run` enqueues it only when routed to Go and the run qualifies (https connectors, no AI/http/formula columns). The exercised rollback runs in both directions with no double charge. Along the way a Python bug was fixed: paid declarative connector calls in queued runs always settled as uncertain (`eaac5ff`). <br>Benchmarks (one shared machine, medians): the Go queue is 1.7–3.4x the in-process Python queue and the production Python path (a child process per job) is about 1–11 jobs/s; enrichment is 14.5x (free connector, 5 ms provider) and 11.1x (paid) Python's cells/s without fsync, and 3.0x/4.9x with durable commits, and 1.6x when the provider is the bottleneck. Retention is 37x faster as a whole small job (the Python child-process cost) but the Go handler alone is 4–5x slower than Python from 160,000 expired rows because its batched delete rescans; that is open. Known differences, cutover and rollback are in [`apps/server/README.md`](../../apps/server/README.md#job-types-running-in-go), and the triage of the other types in [`job-port-triage.md`](job-port-triage.md). Not yet: controlled-live traffic against real vendors, a cutover on a populated production-sized database. |
| M4 specialists | Partly done: the `process` runtime (`internal/plugin/process`) and a stdlib-only Python SDK (`packages/sdk-python`, `opengtm_sdk`). The wire protocol is versioned length-prefixed JSON over an inherited unix socketpair, **not gRPC**, which the RFC originally proposed. One process per run, a bounded pool, a scrubbed environment, rlimits, whole-tree kill on timeout, cancellation or crash, optional bubblewrap. Not yet: the default launcher is not a security boundary without bubblewrap, the distroless image has no Python (a Python worker image is needed), no browser pool, and the AI and browser job types (`run_workbook`, `source_workbook`, `collect`, `bulk_enrich`, `refresh_workbook`, `ambitionbox_import`, `research_playbook_run`) stay in Python by design. |
| M8 multi-host | Building blocks done, exit gate not met: PostgreSQL scheduler leases with fencing tokens (Python `scheduler_lease.py`, Go `internal/lease`, `opengtm leases`), a PostgreSQL workspace-metadata store (`WORKSPACE_META_STORE=postgres`), a PostgreSQL collection ledger (`COLLECTION_LEDGER_STORE=postgres`), and a backfill with verification and restore proof (`apps/api/scripts/multihost_backfill.py`). The flags default to current behaviour. Not yet: replicas on independent hosts (everything ran as processes on one host), and Python-run connectors still read secrets from SQLite. See [`m8-multihost-state.md`](m8-multihost-state.md). Run one scheduler until then. |

Measured so far (microbenchmarks, excluding network and database; see
`crates/opengtm-kernels/BENCHMARKS.md`): through WebAssembly, domain
normalization is about 2x faster than Python, while email, phone and name
normalization are not faster, because JSON marshalling across the boundary
costs more than the work. Extraction costs about the same per page as
BeautifulSoup, about 1 ms with cancellation enabled, but scales across cores
to about 10,400 pages per second with 32 goroutines. Following the admission
rule above, the cheap normalizers should be called in batches or
reimplemented in Go rather than called per value through WebAssembly.

## Goals and scope

- Sustain hundreds of completed enrichments per second on a documented workload,
  subject to provider quotas and latency.
- Reduce overhead per enrichment through connection reuse, bounded concurrency,
  batched persistence, and query improvements.
- Preserve tenant isolation, cancellation, retries, provider evidence, billing,
  and existing REST/MCP behavior.
- Make the minimal self-hosted install a single process plus PostgreSQL, with
  one-command install, upgrade, backup, and diagnostics.
- Give users a documented plugin system and SDKs so a new scraper or provider
  is a small, testable, signed package and needs no fork.
- Keep a fast, accessible dashboard with explicit web performance budgets.
- Make AI-assisted development fast through small packages, generated contracts,
  fixtures, and reproducible checks.

Out of scope for this RFC: new message brokers, workflow platforms, vector
databases, and a mandatory Kubernetes deployment. Each needs separate evidence.
Replacing working behavior without parity fixtures is not allowed.

## Current architecture and opportunities

The [deployed architecture](../architecture.md) uses Python/FastAPI, PostgreSQL
with forced workspace RLS, a PostgreSQL durable queue, Redis progress, Python
workers, and a scheduler, all from one application image. Workspace metadata,
secrets, and collection ledgers still have local SQLite dependencies that
constrain multi-host operation. Providers can already be added without Python
through the YAML [connector SDK](../connectors/README.md), with Ed25519-signed
packages and a trusted-publisher store. The plugin system below extends that
foundation rather than replacing it.

Concrete candidates for measurement:

| Area | Current code | Migration opportunity |
| --- | --- | --- |
| Queue | [`queue_service.py`](../../apps/api/services/queue_service.py) | Preserve claims, tenant fairness, retries, and reconciliation while measuring worker overhead |
| Job isolation | [`job_process_runner.py`](../../apps/api/services/job_process_runner.py) | Avoid a fresh Python interpreter for native Go work; retain killable isolation for specialist tasks |
| Provider isolation | [`provider_runner.py`](../../apps/api/services/workbook/provider_runner.py) | Preserve the existing warm, killable process pool behavior where needed |
| HTTP fetching | [`leadgen/http.py`](../../apps/api/services/leadgen/http.py) | `_fetch_plain` constructs a client per fetch; evaluate reusable transports while preserving proxy and redirect protections |
| Workbook refresh | [`workbook/refresh.py`](../../apps/api/services/workbook/refresh.py) | `_stale_row_ids` loads all workbook rows and overlays; move candidate selection into bounded SQL queries |
| Worker capacity | [`config.py`](../../apps/api/core/config.py) | Current defaults are one worker slot and two active jobs per workspace; record actual configuration before comparing languages |
| Declarative connectors | [`connectors`](../connectors/README.md) | Run manifest v1 connectors natively in the Go host, unchanged |

These are hypotheses, not measured bottlenecks. Establish a baseline before
attributing throughput limitations to Python or predicting a speedup.

## Language ownership

| Language | Responsibility |
| --- | --- |
| Go | REST/SSE/MCP APIs, auth and workspace authorization, queue and orchestration, scheduler, plugin host and capability enforcement, ordinary HTTP providers, the declarative scraper engine, result persistence, billing coordination, CLI, and serving the web bundle |
| Rust | Profiled CPU/memory hotspots: HTML and document extraction, name/domain/phone/address normalization, deduplication and entity matching, bulk CSV import/export. Built for `wasm32-wasip1` and run in the Go host by default; native gRPC service only with a benchmark showing WebAssembly overhead matters |
| Python | AI research and model SDKs, browser automation (Playwright), code-first user scrapers, specialized extractors, and integrations retained during migration |
| TypeScript | React dashboard, generated API client, and the TypeScript plugin SDK |

The Go coordinator owns each migrated workflow and its final state. Rust kernels
and Python specialists return typed results and evidence; they never write
tenant data directly. Legacy Python job handlers keep their existing persistence
behavior until their job type is explicitly migrated.

Running Rust as WebAssembly through a pure-Go runtime keeps the main binary free
of cgo, so it can be cross-compiled into one static artifact for every target
platform. The same runtime runs sandboxed plugins, so kernels and plugins share
one execution, limits, and observability path.

## Plugin architecture

### Extension points

| Kind | Purpose | Example |
| --- | --- | --- |
| `provider` | Enrich a record through one capability | Email finder, firmographics API |
| `scraper` | Discover or extract records from web pages or documents | Company team page, job board, directory |
| `signal` | Watch a source and emit buying-intent events | Funding filings, hiring changes, site changes |
| `destination` | Send qualified records or events elsewhere | CRM, sequencer, webhook, warehouse |
| `function` | Add workbook formula functions or transforms | Domain parsing, scoring, custom normalization |
| `tool` | Expose an action to agents; published to MCP automatically | Lookup, verification, research step |

Every plugin, whatever its runtime, produces the same typed output: normalized
fields plus evidence (source URL or API, fetched time, extraction path, cost,
and confidence). That keeps waterfalls, cell traces, and billing uniform.

### Three runtimes, one manifest

| Runtime | Written in | Isolation | Use it for |
| --- | --- | --- | --- |
| Declarative | YAML | Executed by the Go host; no user code | HTTP APIs and most scrapers: requests, selectors, pagination, mappings |
| WebAssembly | Rust, Go, TypeScript/JavaScript, or other languages with an Extism-style PDK | In-process sandbox with memory, CPU-time, and host-call limits | Custom logic, transforms, and workbook functions that need speed and safety |
| Out-of-process | Python (first-class SDK) or any language speaking the gRPC contract | Separate worker process or container with no database access | Browser automation, AI agents, and heavy dependency stacks |

Plugin manifest v2 is a superset of connector manifest v1. Existing v1 YAML
connectors load unchanged, and v1 stays supported until a documented deprecation
release. A v2 manifest declares identity, version, kind, runtime, inputs and
outputs (JSON Schema), configuration schema, required secrets, and
**capabilities**:

```yaml
manifest_version: "2"
name: acme_team_page
kind: scraper
runtime: declarative        # declarative | wasm | process
version: 0.1.0
author: your-github-handle
license: Apache-2.0
capabilities:
  network: ["https://*.acme.example"]
  secrets: []
  browser: false
limits:
  requests_per_second_per_domain: 1
  timeout_seconds: 20
inputs: { domain: string }
outputs: person
scrape:
  start: "https://{{input.domain}}/team"
  items: "css:.team-member"
  fields:
    full_name: "css:.name::text"
    title: "css:.role::text"
    linkedin_url: "css:a[href*='linkedin.com']::attr(href)"
  paginate: { next: "css:a.next::attr(href)", max_pages: 5 }
```

The host enforces capabilities. A plugin reaches the network only through the
host fetch API and only for declared destinations. It receives only declared
secrets, resolved per workspace at call time. It cannot reach PostgreSQL, the
filesystem, or other tenants' data. Workspace admins enable each plugin per
workspace and see its capabilities before enabling it.

### Writing a scraper

The goal is a working, tested scraper in minutes:

```bash
opengtm plugin new scraper acme_team_page --runtime declarative
opengtm plugin record acme_team_page --input domain=acme.example   # saves fixtures
opengtm plugin test acme_team_page                                 # runs offline against fixtures
opengtm plugin dev                                                 # hot reload against a local instance
opengtm plugin pack && opengtm plugin sign --key-id me-2026
```

- **Declarative first.** CSS, JSONPath, and regex selectors, with pagination,
  field mappings, and the per-domain limits shown above, cover most pages
  without code. Extraction runs in a Rust kernel. XPath is deferred until a
  plugin needs something CSS cannot express.
- **Python when needed.** The Python SDK provides typed decorators, a host fetch
  client, and a shared Playwright browser pool:

  ```python
  from opengtm_sdk import scraper, Person, Context

  @scraper(name="acme_team_page", outputs=Person)
  async def scrape(ctx: Context, domain: str):
      page = await ctx.browser.open(f"https://{domain}/team")
      for card in await page.query_all(".team-member"):
          yield Person(
              full_name=await card.text(".name"),
              title=await card.text(".role"),
              evidence=ctx.evidence(page.url, selector=".team-member"),
          )
  ```

- **Responsible defaults.** The host fetch client applies `robots.txt`, a
  per-domain rate limit, caching, an identifying user agent, the existing
  outbound-request protections (destination validation, redirect checks, DNS
  pinning), and workspace proxy settings. A plugin cannot turn these off
  silently. An override is a visible, workspace-level setting with an audit
  entry. Plugin authors remain responsible for respecting site terms.
- **Fixtures are the tests.** Recorded HTML/JSON fixtures make plugin tests
  deterministic and offline, and CI runs them for first-party and registry
  plugins.

### SDKs, distribution and trust

- SDKs: Python (`opengtm-sdk`), TypeScript, Rust, and Go, all generated from the
  same contracts in `packages/contracts`.
- Packages reuse the existing Ed25519 signature envelope and trusted-publisher
  store. Unsigned local plugins work under the `optional` policy. Production
  deployments can require signatures.
- A Git-based plugin index lists name, version, kind, capabilities, digest, and
  certification status. Installing a plugin is
  `opengtm plugin install <name>@<version>`. Promotion from `beta` keeps the
  existing controlled-live certification process.
- First-party providers and scrapers live in `plugins/` and use the same
  manifest and SDKs as community plugins, so the public extension surface is
  exercised continuously.

## Frontend

The dashboard stays a React and TypeScript single-page app built with Vite. The
Go binary embeds and serves it, so no separate web server is required. An
authenticated, data-dense dashboard does not need server rendering. Public pages
remain in the Astro docs site.

| Area | Choice and purpose |
| --- | --- |
| Components | shadcn/ui on Base UI (the existing `base-nova` style) as the single design system; owned source, accessible primitives, Tailwind CSS v4 tokens |
| Visual polish | Motion, backgrounds, and text effects inspired by React Bits, implemented in-house on the existing animation stack, lazy-loaded, limited to non-data surfaces, and disabled under `prefers-reduced-motion` |
| Routing | TanStack Router: type-safe routes and search parameters, loaders that prefetch through Query, route-level code splitting; replaces `react-router-dom` |
| Server state | TanStack Query, with SSE progress events written into the query cache |
| Grids | TanStack Table + TanStack Virtual for wide, large workbooks |
| Forms | TanStack Form + Zod, including plugin configuration forms generated from manifest JSON Schema |
| Timing | TanStack Pacer for debouncing, throttling, and batching live updates |
| Evaluate | TanStack DB for live workbook sync, adopted only after a measured win |
| API client | Generated from OpenAPI; no hand-written request types |

**License constraint:** React Bits is licensed MIT plus the Commons Clause, which
prohibits redistributing the components themselves. OpenGTM is AGPL-3.0 and is
redistributed by every self-hoster, so React Bits source must not be vendored.
Use it as design reference only, unless its author grants a compatible license.

Consolidating on shadcn/ui and Base UI retires the vendored `twenty-ui` package
and supersedes the Twenty interface migration plan. Close out or roll back the
in-progress workbook slice explicitly before removing the package.

Web performance budgets are set from the M0 baseline and enforced in CI. They
cover initial JavaScript per route, LCP and INP on the main screens, and grid
scrolling at 100,000 rows. Plugins extend the UI through declared settings
schemas and result renderers first. Arbitrary plugin-supplied UI code needs a
separate security design.

## Deployment and self-hosting

| Profile | Services | Covers |
| --- | --- | --- |
| `lite` | `opengtm` binary + PostgreSQL | API, UI, workers, scheduler, declarative and WebAssembly plugins, Rust kernels; progress over PostgreSQL `LISTEN/NOTIFY` |
| `standard` | `lite` + Redis + Python workers + browser pool | AI research, browser scrapers, Python plugins, shared rate limits across replicas |
| `full` | `standard` + SearXNG, Reacher, OpenTelemetry Collector | Self-hosted search, email verification, observability |

- **One binary, many roles.** `opengtm serve | worker | scheduler | migrate |
  doctor | plugin | backup | upgrade`. Small installs run every role in one
  process; larger installs scale each role separately from the same image.
- **One-command install.** `opengtm init` writes a validated configuration with
  generated secrets and a Compose file for the chosen profile, then
  `docker compose up -d` starts it. Release binaries are published for Linux and
  macOS on amd64 and arm64.
- **Safe upgrades.** `opengtm upgrade` takes a backup, runs migrations as the
  single owner, checks schema and contract versions, and runs health checks.
  The rollback steps are documented and tested in CI.
- **Diagnostics.** `opengtm doctor` checks the database role (`NOBYPASSRLS`),
  migrations, plugin signatures, egress, and resource limits.
- **Configuration.** One `opengtm.yaml` with environment overrides, validated
  against a schema at startup, with a generated reference page in the docs.
- **Supply chain.** Multi-arch images published by digest, signed with
  Sigstore, and released with an SBOM and a release manifest listing every image.
- **Kubernetes** is optional. A Helm chart can follow the Compose profiles,
  but no feature may depend on it.

Migrations stay with the existing Alembic owner until the Go API owns the
schema. They then move to the Go `migrate` role, which is required for the
Python-free `lite` profile. Every service declares its supported schema and
contract versions, and an incompatible combination fails at startup.

## M7 packaging: decisions, evidence and limits

Implemented on `rewrite/m7-packaging`; user documentation is
[`docs/self-hosting.md`](../self-hosting.md) and
[`docs/operations/upgrade-backup-rollback.md`](../operations/upgrade-backup-rollback.md).

**Decisions**

- *Migration owner.* Go owns locking (a PostgreSQL advisory lock plus an install
  lock), planning, refusing a database newer than the release, and verifying the
  head; Alembic stays the engine, run from the release's own Python image
  (`opengtm migrate`, and the `migrate` service in the generated Compose file).
  A Go-native runner would mean porting about 60 Python revisions before the Go
  API owns the schema, for no gain. When it does, only the engine changes. The
  Python-free `lite` profile is blocked on M6 (Go routes), not on packaging.
- *Rollback is a restore.* `opengtm rollback` restores the pre-upgrade backup
  (after a safety backup of the current state). Alembic downgrades are neither
  guaranteed to preserve data nor testable the way a restore is. The cost,
  losing writes since the backup, is stated in the runbook and requires an explicit
  yes when the upgrade had succeeded.
- *One Compose file, three profiles.* `lite`, `standard` and `full` are Compose
  profiles of a single embedded `compose.yml` selected by `COMPOSE_PROFILES`;
  `opengtm upgrade` refreshes it to the version shipped with the new binary
  unless the operator edited it.
- *Backups use the stock PostgreSQL tools* (`pg_dump` custom format, restored in
  one transaction) so a backup stays usable without opengtm. Roles, which
  `pg_dump` does not carry but policies reference, travel in `roles.sql`.
- *Release supply chain.* GoReleaser for six binaries, checksums and SPDX SBOMs;
  native per-architecture image builds (no QEMU) merged into one multi-arch tag
  and signed by digest; keyless cosign signatures bound to the release workflow's
  identity; a signed `release-manifest.json`. Publishing waits for the packaging
  suite.

**Evidence.** `scripts/packaging/e2e.sh` runs against real containers (see the
workflow in `.github/workflows/packaging.yml` for the CI matrix on `ubuntu-24.04`
and `ubuntu-24.04-arm`). The Go packages are tested against PostgreSQL 18:
checksummed backup and restore into scratch databases (roles recreated, forced
RLS preserved, sequences continue), wipe-and-restore removing objects from a
newer schema, corruption and path-traversal rejection, the migration owner
against the real Alembic graph, and the upgrade and rollback decision logic
against a recording fake.

**Not proven, and why**

- arm64 was not executed here; it is configured as native `ubuntu-24.04-arm`
  runners in CI and the images build without emulation, but no arm64 run has
  happened yet.
- "Upgrade from the previous release" has no published predecessor for the new
  Compose install: `v3.0.0` predates the runtime-role and seed scripts the install
  needs, and no Go server release exists. The suite therefore uses the fork point
  from `main` (or the newest qualifying tag, once there is one) as the previous
  Python image, which crosses real Alembic revisions, and the same Go server code
  labelled with an older version.
- The release workflow, signing and publishing were not run. GoReleaser's
  configuration was checked and a snapshot build produced all six archives and
  checksums locally; cosign verification was exercised with the real client only
  against a forged bundle, and with a stub for the accepting path.
- The Kubernetes example is schema-validated only; it was not applied to a cluster.
- `lite` is not Python-free and has no Redis, so live workbook updates over
  WebSocket need `standard`. The `full` collector is provisioned but nothing
  first-party exports to it yet. There is no browser pool until M4.
- Restore time and backup size were not measured on a production-sized database.

## Proposed stack

Use supported stable releases, pin exact versions and image digests at
implementation time, and upgrade through compatibility checks.

| Area | Choice and purpose |
| --- | --- |
| Go | Go 1.27 release line with a pinned patch; `net/http`, `context`, pooled transports, `embed`, and `pprof` |
| Database | PostgreSQL 18, current supported patch; preserve forced RLS and existing domain constraints |
| Go database access | pgx v5 + sqlc for pooled connections and generated types from explicit SQL |
| Durable execution | Existing PostgreSQL `jobs` queue, extended only with reviewed compatibility migrations |
| Progress and limits | PostgreSQL `LISTEN/NOTIFY` in `lite`; Redis for progress fan-out and shared rate limits in larger profiles; durable state stays in PostgreSQL |
| Plugin and kernel runtime | wazero (pure Go, no cgo) with an Extism-compatible plugin interface; WASI for Rust kernels |
| Public API | Existing REST + SSE + MCP semantics; OpenAPI contracts and generated Go/TypeScript clients |
| Internal contracts | Protobuf + gRPC with Buf generation and compatibility checks; bounded request sizes and authenticated internal callers |
| Rust | Stable Rust, Cargo, Clippy, rustfmt; `wasm32-wasip1` target by default, Tokio only for native services |
| Python | Existing supported Python baseline, uv lockfile, Pydantic validation, pytest, Ruff, Playwright |
| Bulk transformation | Evaluate Polars before writing a custom Rust engine |
| Frontend | React, TypeScript, Vite, Tailwind CSS v4, shadcn/ui on Base UI, TanStack Router/Query/Table/Virtual/Form/Pacer, Bun tooling |
| Observability | OpenTelemetry, structured logs, metrics, distributed trace propagation across Go, Rust, and Python, and Go profiling |
| Benchmarks | k6 for API load, an enrichment completion harness, a deterministic provider simulator, and Lighthouse CI for web budgets |
| Deployment | Compose profiles, single multi-role binary, signed multi-arch images, one migration owner, release manifest |

## Target execution model

```text
React SPA (embedded) / REST / SSE / MCP
        |
        v
opengtm (Go): API + worker + scheduler + plugin host     legacy FastAPI during migration
        |
        v
PostgreSQL: domain data + durable jobs + billing ledger
        |
        v
Go enrichment worker
   |              |                     |                       |
HTTP providers   declarative/WASM      Rust kernels (WASM;     Python specialists
                 plugins (sandbox)     native if justified)    (AI, browser, Python plugins)
   |              |                     |                       |
   +--------------+---------------------+-----------------------+
                              |
                  guarded result transaction
                              |
             progress -> LISTEN/NOTIFY or Redis -> SSE
```

Specialist and plugin contracts include task/contract version, job ID,
workspace ID, attempt/lease token, deadline, trace context, bounded inputs, and
a result containing normalized fields, evidence, usage, and typed errors.
Authenticate the caller and validate workspace authorization; a workspace ID in
a payload alone is not sufficient.

Credentials resolve through an authenticated workspace-scoped mechanism. Keep
secrets out of durable job payloads, logs, traces, and reusable fixtures.
Provider clients may reuse transports, but must not reuse tenant-specific
credentials or cookies across workspaces.

Keep bounded pools of WebAssembly instances and specialist processes.
Cancellation propagates through Go contexts and RPC deadlines. Browser tasks
and uncooperative libraries also need process termination and parent-side
failure reconciliation.

## Correctness requirements

1. **Tenant boundary:** bind verified workspace identity inside each database
   transaction using transaction-local settings. Runtime roles remain
   non-superuser and `NOBYPASSRLS`; unset workspace context fails closed. Test
   pooled connections alternating tenants and all retry/worker paths.
2. **Queue ownership:** route each job type/payload version to exactly one
   executor implementation. Both claim queries enforce routing, and queued jobs
   remain compatible with rolling upgrades. Retain `SKIP LOCKED`, heartbeats,
   retry budgets, tenant fairness, and domain failure reconciliation.
3. **Lease fencing:** increment an attempt/lease token when claiming work.
   Heartbeats, domain commits, and completion validate the current token and
   cancellation state transactionally. Additive schema changes must teach both
   executors the protocol before enabling Go claims. Old workers cannot reclaim
   ownership or commit after a newer attempt takes over.
4. **Side effects:** use provider idempotency keys where supported, durable
   outcome records, and unique internal billing entries. External calls can
   repeat after an ambiguous timeout; reconcile uncertain outcomes instead of
   claiming exactly-once external execution.
5. **Cancellation:** a committed cancellation cannot be overwritten by success
   or failure. Stop admission of new provider calls and reconcile in-flight work.
6. **Isolation and limits:** bound queues, goroutines, WebAssembly instances,
   process pools, database connections, response sizes, and batch sizes.
   Coordinate provider limits across replicas and credentials; tenant fairness
   must survive worker scaling.
7. **Plugin containment:** plugins act only through declared capabilities and
   host APIs. Escapes, undeclared egress, secret access outside the declaration,
   and cross-tenant reads are release-blocking defects with dedicated tests.
8. **Compatibility:** preserve evidence provenance, connector cursors, counts,
   merge/undo semantics, API errors, agent scopes, and manifest v1 connectors.
   Keep one schema migration owner at a time.

## Migration milestones and exit gates

| Milestone | Deliverable | Exit gate |
| --- | --- | --- |
| M0: baseline | Fixed fixtures, provider simulator, representative workloads, backend resource profiles, current queue/API measurements, and web metrics | Reproducible baseline with hardware, commit, configuration, and workload identity; initial web budgets recorded |
| M1: foundations | Go module and multi-role binary, sqlc queries, versioned contracts, telemetry, additive executor routing/fencing, Compose development setup | Python-only operation still passes; mixed-version claims, RLS, and lease recovery tests pass |
| M2: one worker | One HTTP enrichment job type in Go, including manifest v1 connectors, result/evidence persistence, billing coordination, and progress | Behavioral parity, crash/cancellation tests, benchmark comparison, feature flag, and exercised rollback |
| M3: plugin platform | Manifest v2, wazero host with capability enforcement, declarative scraper engine, `opengtm plugin` CLI, fixture recording, signing, and plugin index | A third party can build, test, sign, and install a scraper and a provider without touching core code; containment tests pass |
| M4: specialists | Python worker pool and SDK over gRPC for AI, browser, and Python plugins, with process isolation and bounded capacity | Contract, timeout, secret-isolation, retry, and reconciliation tests pass |
| M5: web platform | TanStack Router migration, shadcn/ui and Base UI consolidation, generated client, schema-driven plugin settings, budgets in CI | Route parity, accessibility checks, and budgets met; `twenty-ui` removed. Can run in parallel from M1 |
| M6: API migration | Go routes introduced by domain, with the existing FastAPI fallback | Auth, REST/SSE/MCP contract parity and supported deployment smoke checks pass |
| M7: self-host packaging | `lite`/`standard`/`full` profiles, Go migration owner, `init`/`upgrade`/`backup`/`doctor`, signed multi-arch releases | Fresh install, upgrade from the previous release, backup restore, and rollback pass in CI on amd64 and arm64 |
| M8: multi-host readiness | Workspace metadata, secrets, and collection ledgers moved to PostgreSQL; scheduler leadership defined | Backfill/restore proof, reconciliation, and replicas on independent hosts pass |
| Rust kernels | One profiled kernel at a time, starting with extraction or normalization and deduplication | Documented end-to-end improvement including WebAssembly or RPC overhead, build cost, correctness, and operational overhead |

Choose the precise first job type during M0 based on independent scope and
available parity fixtures. M8 is mandatory before any multi-host deployment;
single-host migration does not depend on it. Run one scheduler until leadership
and failover are implemented.

Roll out per job type, route, or screen to an explicit workspace cohort. Stop
new claims before changing executor ownership, drain or recover in-flight
attempts, and keep old-compatible payloads and schema. Shadow comparisons use
recorded or simulated responses so they do not duplicate paid calls. Re-enable
the legacy executor through the same routing protocol to roll back; do not drop
rollback support until the migrated capability passes its release gates.

## Performance evidence

Define one completion as one input record finishing its specified enrichment
waterfall with the result, evidence, and internal accounting committed. Record
terminal no-match outcomes separately from successful enrichment. Jobs, API
requests, and individual provider calls are distinct counters.

Use 100, 300, and 1,000 completed enrichments per second as load points, not
promised capacity. Compare implementations on identical hardware, data, limits,
provider behavior, and durability settings. Run at least a 30-minute steady
load after warm-up, plus overload and recovery phases; publish the actual
achieved rate if a target is unsustainable.

Include single-provider, multi-step waterfall, slow-provider, rate-limit,
timeout, malformed-response, worker-crash, cancellation, multi-tenant, and
plugin-heavy workloads. Label simulated-provider capacity separately from
quota-permitted controlled-live throughput. AI research, browser-heavy work,
and scrapers get separate capacity profiles.

Report committed completions/sec; queue delay; end-to-end p50/p95/p99; API
latency; CPU/RSS; database connections, writes and lock waits; provider attempts
and 429s; timeouts/errors; tenant fairness; outstanding backlog; and cost per
completed record. Show that steady-state backlog and memory remain bounded.
Report the `lite` profile's resource floor separately, because small self-hosted
machines are a primary target.

Release requires parity tests, no duplicate internal accounting in replay
fixtures, no tenant leaks, exercised failure recovery and rollback, and
published evidence of improvement. Set concrete latency/resource thresholds
from M0 rather than inventing them before measuring.

## Repository layout and contributor workflow

```text
apps/server           Go: API, worker, scheduler, plugin host, CLI (one binary)
apps/web              React dashboard (embedded into the binary at build time)
apps/py-workers       Python specialists and the Python plugin runtime
apps/api              legacy FastAPI, retired route by route
crates/               Rust kernels (wasm32-wasip1; native only when justified)
packages/contracts    Protobuf, OpenAPI, plugin manifest JSON Schema
packages/sdk-*        Python, TypeScript, Rust, and Go plugin SDKs
plugins/              first-party providers, scrapers, signals, destinations
```

Keep Go packages small and organized around domains: queue, tenancy, plugin
host, provider execution, persistence, progress, and billing. Preserve existing
Python entry points until their replacement is released.

Maintain scoped contributor instructions with package ownership, commands,
invariants, and examples. Check generated contracts and SQL code into the repo
where appropriate, and have CI detect regeneration drift. Each migration PR
includes a before/after behavior fixture, acceptance checks, rollout switch,
and rollback instructions. A plugin PR includes its manifest, fixtures, and
offline tests.

Extend existing CI with Go tests and race checks, contract compatibility, real
PostgreSQL RLS/queue tests, plugin containment tests, Python specialist tests,
Rust checks once crates exist, web budgets, and fresh-install/upgrade tests per
profile. Keep the frontend build and existing backend tests as gates during
coexistence. Run performance comparisons in a controlled environment; shared CI
timing alone is insufficient evidence of throughput.

## Co-maintainer work and first implementation checklist

We welcome co-maintainers for Go execution, the plugin platform and SDKs, Rust
kernels, Python AI/browser integration, the React dashboard, PostgreSQL
correctness, self-host packaging, or reproducible performance testing. Follow
[CONTRIBUTING.md](../../CONTRIBUTING.md), including DCO sign-off.

- [x] Record the M0 backend and web baselines, and select the first enrichment job type. (Baselines in `benchmarks/results/`; `plugin_run`, `retention_enforce` and `run_workbook_connector` are the first job types.)
- [x] Define domain ownership, task contracts, executor routing, and lease fencing.
- [x] Add the Go multi-role binary skeleton, pgx/sqlc access, and tenant transaction helper.
- [x] Preserve Python-only operation and exercise mixed-executor claims.
- [x] Implement one HTTP provider path, including manifest v1 connectors, with bounded concurrency and safe egress.
- [x] Commit results, evidence, and internal billing under the active lease.
- [x] Draft plugin manifest v2 and the capability model; prototype the wazero host with one declarative scraper.
- [x] Exercise retry, crash, cancellation, tenant isolation, plugin containment, and rollback fixtures.
- [ ] Publish comparable throughput/resource reports and controlled-live results. (Reports are published; controlled-live results against real vendors are not.)
- [ ] Release the first Go worker behind explicit routing and a feature flag. (Built and routable; no tagged release has been cut.)

Open decisions: first job type, numerical SLOs after baseline, exact dependency
pins, credential resolution across services, plugin index hosting and
governance, the `lite` profile's progress transport under load, and the
measured case for each Rust kernel.

## Technology references

- [Go 1.27 release notes](https://go.dev/doc/go1.27)
- [PostgreSQL version policy](https://www.postgresql.org/support/versioning/)
- [pgx v5](https://pkg.go.dev/github.com/jackc/pgx/v5)
- [sqlc](https://github.com/sqlc-dev/sqlc)
- [wazero](https://wazero.io/)
- [Extism](https://extism.org/)
- [Buf contract tooling](https://buf.build/docs/)
- [uv](https://docs.astral.sh/uv/)
- [Pydantic](https://docs.pydantic.dev/latest/)
- [Playwright for Python](https://playwright.dev/python/)
- [Polars](https://docs.pola.rs/)
- [shadcn/ui](https://ui.shadcn.com/) and [Base UI](https://base-ui.com/)
- [TanStack](https://tanstack.com/)
- [React Bits license](https://github.com/DavidHDev/react-bits/blob/main/LICENSE.md)
- [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/)
- [Sigstore](https://www.sigstore.dev/)
- [k6](https://grafana.com/docs/k6/latest/)
