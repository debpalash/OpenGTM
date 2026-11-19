# Changelog

All notable changes to OpenGTM are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[SemVer](https://semver.org/). Container images are published to
`ghcr.io/debpalash/opengtm` on every tag.

## [Unreleased]

Go runtime cutover, planned as 4.0.0. The 2,212 commits since v3.0.0 replace the Python application runtime with
native Go services. Nothing below has been released: v3.0.0 is still the only published version, and release
readiness is tracked in [#120](https://github.com/debpalash/OpenGTM/issues/120).

### Added
- Native Go runtime (#115): API (with the web UI embedded), worker, scheduler, MCP server, `opengtm-migrate` and
  `opengtm-seed`. PostgreSQL with forced workspace row-level security owns the data and the durable job queue;
  Redis carries live progress. The Python browser service is kept for Playwright and browser-dependent scraping.
- `opengtm-migrate` (goose): applies the schema from an empty database, or adopts a database at Alembic revision
  `e7b2c4d6f8a0`, then applies Go migrations (currently up to schema 5). Migrations are forward-only. See
  `docs/go-port/runbooks/migrations.md`.
- First-run setup: the first admin is created in the UI (`/setup`, `POST /auth/setup`) with a one-time setup token
  logged by `api-go`; there is no default admin account. Opt-in `demo` Compose profile seeds a demo workbook.
- Dashboard workbench with separate Companies and People, shared record history and Clay/CSV interchange (#127;
  schema 5 needs all writers stopped while migrating).
- Opt-in providers: Firecrawl search fallback (#118), Spicrawl markdown/PDF research (#119), Scavio Google Maps
  (#129), CompanyEnrich company and people research (#128), Keenable search source (#157) and `custom_openai`
  for self-hosted OpenAI-compatible servers (#161).
- Release rehearsal scripts (`scripts/release/rehearse-install.sh`, `rehearse-backup-restore.sh`,
  `rehearse-upgrade.sh`), a workflow that runs the first two on pushes to `main`, and the operator runbook
  `docs/operations/release-rehearsals.md`.
- CI: Playwright and axe accessibility gate for the web app (#155); a CLA bot (#133, #136, #145, #153).

### Changed
- `docker compose up` starts the Go stack: postgres, redis, a one-shot `migrate`, `api-go`, `worker-go`,
  `scheduler-go`, the internal `browser` service and nginx. Release images are named
  `opengtm-{api,worker,mcp,migrate,scheduler,browser}`.
- The Go services and the browser service refuse to start without `SECRET_KEY`, `INTERNAL_SERVICE_TOKEN` and
  `BROWSER_SERVICE_TOKEN`; `scripts/install.sh` generates them. `OUTREACH_PUBLIC_BASE_URL` must be https unless
  `APP_ENV` is a development value.
- Retired flags: the `GO_EXECUTOR_*`, `GO_*_ROUTES_ENABLED`, `GO_SCHEDULER_ENABLED`, `COPILOT_GO_ENABLED` family and
  the worker's `PY_WORKER_*` settings no longer have any effect (a leftover `false` is ignored with a warning).
  See `docs/go-port/runbooks/15-cutover.md`.
- Python installations: `opengtm-migrate` upgrades a v3.0.0 database (Alembic `3f4051627384`) directly: it applies the
  eight intervening Alembic revisions' DDL and the job-queue steps in one transaction, stamps `e7b2c4d6f8a0`
  and continues. `opengtm-migrate import-v3 --data-dir DIR [--dry-run]` copies the SQLite stores (workspaces and
  members, workspace secrets, provider keys, chat history, workbook functions, chat memories); use the same
  `SECRET_KEY`. A default v3.0.0 install keeps leads, outreach and signals in PostgreSQL, so the bridge upgrades them
  in place; `import-v3` stops before writing anything if a fallback `leads.db`, `outreach.db` or `signals.db` holds
  rows (override with `--skip-unmigrated`). `enrichment_cache.db` and `openmemory.db`
  are not imported and are listed with row counts. Any other Alembic revision is refused. Runbook:
  `docs/go-port/runbooks/migrations.md`; rehearsal: `scripts/release/rehearse-upgrade.sh`.
- Toolchain go1.27.4 and `golang.org/x/net` v0.62.0. The patched HTML tokenizer keeps the first of duplicated
  attributes, as browsers do (#158).
- Email verification: `angi.com` is no longer treated as disposable (#130); negative MX lookups are retried after
  one minute (#143).
- Replaced the docs homepage's synthetic product examples with a restrained, real-workspace presentation and
  direct laptop/server install paths; updated the quickstart with generated credentials, a pinned GHCR image, and
  loopback-plus-TLS server setup; rewrote homepage and introduction copy around self-hosted lead sourcing, spend
  limits, and provider data boundaries.

### Security
- Supply chain (#158): govulncheck, CodeQL and gitleaks workflows; every GitHub Action pinned by commit SHA; ten
  reachable vulnerabilities fixed by upgrading `golang.org/x/net` and the Go toolchain.
- Egress (#165): an allow-listed host (`EGRESS_ALLOW_HOSTS`, `CUSTOM_OPENAI_BASE_URL`) can no longer resolve to a
  cloud metadata or link-local address; the checked address is the one dialled.
- Spreadsheet formulas are neutralized in lead CSV exports (#100) and pushed Sheets values are stored as text (#101).
- `SECURITY.md` lists supported versions: 3.x gets security-only fixes for 90 days after v4.0.0 is published (#164).
- Closed the verification gaps of audit #114, including a fix for a provider miss aborting MCP `enrich_company`
  (#160).

### Removed
- The Python API, worker and scheduler runtime, and the Python fallback and canary routing flags (#115). Only the
  browser service remains in Python.
- The GPL-3.0 `holehe` email provider (#137).
- The unauthenticated `FILES_API_MODE=legacy` files API.

### Fixed
- Release notes now use the lowercase, pullable GHCR image name.

## [3.0.0] - 2026-09-24

First public release: workbooks, enrichment waterfalls, agentic research/chat,
outputs, outreach, signals, automations, workspace-scoped RLS, MCP, the n8n
node and Chrome extension. The items below were completed for this release.

### Added
- `POST /api/leads/verify-email`, `POST /api/leads/score` and
  `POST /api/leads/tech-stack`: single-record utilities that reuse the MCP
  tool paths, so scripts and the n8n node no longer need a workbook.
- Documentation site (`apps/docs`, Astro 7.3 + Starlight) published at
  https://opengtm.palash.dev, including a generated REST API reference.
- `scripts/export_openapi.py` keeps `apps/docs/openapi/openapi.json` in sync
  with the FastAPI app; CI fails when it drifts.
- Release tooling under `scripts/release/`: a preflight checker and a
  public-snapshot builder that strips maintainer-only paths.
- Tag-driven release workflow (GHCR multi-arch image + draft GitHub release),
  manual docs deploy workflow, Dependabot, and CODEOWNERS.
- A workspace-scoped attention center at `/notifications` with critical failed
  tasks/workbooks and urgent unread high-intent signals, plus a live header
  preview and severity filters.

### Changed
- `uv.lock` and `bun.lock` are now tracked for reproducible installs.
- Maintainer-only planning documents moved under `docs/internal/`.
- Theme selection moved from the top toolbar to the sidebar footer, including
  the collapsed and mobile sidebar menus.
- The standalone **New chat** sidebar link is now an icon on the Chat row.
  It opens an inline prompt; submitting the prompt drafts a new conversation
  without sending it. The icon remains available in the collapsed sidebar.
- Simplified the README into a quick local path and a version-pinned, TLS-ready
  server path; installers prepare a writable bind-mounted data directory.

### Fixed
- Protected deep links now retain query parameters and fragments through sign-in,
  including chat drafts.
- Populated website, email and other typed lead-field cells can be edited by
  double-clicking the cell while retaining their formatted links and badges.
- Docker builds include the docs workspace manifest needed for the frozen Bun
  workspace install.
- The n8n node now calls endpoints that exist (`/api/collect`, `/api/lead` +
  `/api/leads/bulk-enrich`, the new utilities above); five of its seven
  operations previously returned 404.

### Removed
- The legacy document-download pipeline (Scribd, torrent-index and archive
  resolvers, the `/api/queue` router, the `/ws` queue socket, the
  `download_link` job and the Downloads page). It predated the GTM product
  and had no place in a public release.
- Lead exports and scraped fixture snapshots under `data/` are no longer
  tracked; they are regenerated locally and ignored.
- Private submodule pointers (`packages/proxy-manager`, `data_collector`);
  both integrations remain optional and degrade to no-ops when absent.

[Unreleased]: https://github.com/debpalash/OpenGTM/compare/v3.0.0...main
[3.0.0]: https://github.com/debpalash/OpenGTM/releases/tag/v3.0.0
