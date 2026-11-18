# OpenGTM expert upgrade plan

As of 18 November 2026. Owner: project maintainer.

> **Superseded in part.** Priorities and the roadmap are now in the [19 November plan](2026-11-19-three-levels-plan.md). The workstream detail below still applies.

## Summary

For the next two quarters, OpenGTM should spend its time on trust, clarity and switching cost rather than new features.

Three gaps drive that:

- The Go runtime is merged, but the UI still has 5 open P0 findings from the [29 October audit](../audits/dashboard-ui-ux-2026-10-29/README.md).
- The release gates in #120 and #114 are still open.
- Moving off Clay, Apollo or a CRM is still a CSV-only path.

The plan runs as nine workstreams. Each has an owner role, acceptance criteria and a release gate. Items are grounded in the repository as of 18 November 2026. Items marked *proposed* need a decision before work starts.

**Top 10 priorities, in order**

1. Close the UI P0 set: honest analytics states, contrast, named controls and workbook grid semantics (audit A01 to A05).
2. Add an automated accessibility and visual gate to CI so fixed issues stay fixed.
3. Build one design system: owned tokens, one primitive library, and one confirmation and feedback pattern.
4. Reduce navigation from 24 top-level routes to 6 task areas, showing advanced options only when needed.
5. Add a guided first run: setup, first import and first enriched workbook in under 10 minutes.
6. Finish the security closure (#114) and release gates (#120), then add supply-chain controls: govulncheck, CodeQL, an SBOM and signed images.
7. Build staged import with preview, mapping memory, error report and undo, for Clay, Apollo, HubSpot, Salesforce and generic CSV.
8. Add full workspace export and restore, so users can leave as easily as they arrive.
9. Add observability and published service-level objectives (SLOs) for jobs, providers and outreach.
10. Improve the contributor flow: CI on outside PRs, a provider SDK and a review checklist.

## Current baseline

The product works end to end, but quality is uneven. The backend is hardened and heavily tested; the frontend and the operator experience lag behind.

| Area | Today | Main gap |
| --- | --- | --- |
| Backend | Go API, worker and scheduler; PostgreSQL with row-level security; Redis. Python remains only as a small browser service. | Release gates in #120 not yet passed on staging |
| Web app | React 19, Vite, Tailwind 4, shadcn; 148 TSX files, 30 pages, 24 top-level routes | Workbook editor is 2,730 lines, Automations 1,776, Outreach 1,218 |
| UI quality | The 29 Oct audit found 14 axe rule types with 483 occurrences: 5 P0 and 10 P1 findings | No `apps/web` commit since names an audit item; the 5 P0 fixes are not in place |
| Accessibility | Skip link and main landmark exist; axe-core is a dependency | No axe run in CI; 27 clickable `div`/`span` elements; 18 browser `confirm`/`prompt` calls |
| Security | Egress guard with dial-time rebinding checks, encrypted secrets, tenant row-level security, rate limits, SSO, audit log | No govulncheck, CodeQL, SBOM or image signing in CI; no MFA; CSP only on the public outreach page; #114 closure open |
| Import and export | CSV import with Clay mapping; leads export; HubSpot, Salesforce, Instantly and Smartlead destinations | No staged preview, undo or full workspace export; no Apollo or HubSpot import |
| Operations | `/healthz` and `/health` liveness checks | No Prometheus metrics or OpenTelemetry tracing |
| Community | 6 outside PRs open; CI for first-time contributors needs manual approval | No provider SDK or review checklist |

Sources: [UI audit](../audits/dashboard-ui-ux-2026-10-29/README.md), [transformation spec](../specs/dashboard-ui-ux-transformation.md), `.github/workflows/`, and a repository scan on 18 Nov 2026.

## 1. UI/UX, clutter and cognitive load

The biggest UX cost is too many choices at once. Users meet 24 top-level routes, five file and admin actions on each record table, and dense per-row controls before they finish one task. The fix is fewer, task-shaped screens that show advanced options only when needed, built on one design system.

**Information architecture.** Owner: product designer.

- Group the 24 routes into 6 task areas. *Proposed.*
  - Find: search, sources, signals.
  - Records: companies, people, audiences.
  - Enrich: workbooks, templates, agents.
  - Engage: outreach, campaigns, automations.
  - Measure: overview, analytics.
  - Admin: settings, workspaces.
- Give each page one primary action. Put secondary actions in an overflow menu. Keep destructive actions separate, and confirm them in an in-app dialog.
- Give every route breadcrumbs and a consistent page header: title, scope, primary action and status.
- Add a command palette (Ctrl/Cmd+K) for navigation and actions.

**Clutter and density**

- Default views show the identity columns plus at most 6 data columns. The rest live in saved column sets.
- Collapse advanced settings by default. Show them when the user opts in, or when a value differs from its default.
- Replace 9 and 10 px text (176 uses found by the audit) with a 12 px minimum and a 13 to 14 px body size.
- Offer two density modes, comfortable and compact, saved per user.

**Cognitive load**

- Pin the record identity (company or person) in every workbook view (audit A13).
- Before any paid or bulk action, show what will run, on how many rows, and at what estimated cost.
- Use plain status words everywhere: Queued, Running, Done, Failed. On failure, show the reason and the next step.
- Use one vocabulary across modules, and publish it as a glossary in the docs.

**Design system.** Owner: design-system engineer.

- Carry out work packages UI-02 and UI-03 from the [transformation spec](../specs/dashboard-ui-ux-transformation.md):
  - owned semantic tokens and one primitive API
  - a reduced-motion policy
  - 32 to 44 px hit areas
- Replace 141 literal colours and 385 direct palette uses (audit A09) with tokens, and add a lint rule that blocks new ones.
- Split the largest route modules into feature components as each screen is rebuilt (UI-13).

**Acceptance criteria**

- In a 5-person usability test, a new user can import 100 companies, enrich them and export them without help in under 10 minutes.
- The median number of clicks to complete a core task falls by 30 percent against a recorded baseline.

## 2. Accessibility (WCAG 2.2 AA)

The target is WCAG 2.2 AA on every core route. CI enforces it, and a manual screen-reader pass runs before each release. Five audit findings block that today.

| Finding | Fix | Package |
| --- | --- | --- |
| A02 Primary text contrast is 4.0:1 (light) and 3.6:1 (dark) | New palette with every text pair at 4.5:1 or more; contrast matrix test | UI-02 |
| A03 96 unnamed source switches | Label each switch with its visible source name | UI-03 |
| A04 13 unnamed settings buttons, including the API-key reveal | Accessible names on icon buttons; labelled fields | UI-03 |
| A05 Nested controls in workbook headers; no row or column counts | Full grid pattern: `aria-rowcount`, `aria-colcount`, and header menus outside the sort button | UI-06 |
| A06 33 small targets (20 × 20 px) | 24 px minimum (WCAG 2.5.8); 32 to 44 px as the product standard | UI-02, UI-03 |
| A07 72 label and name mismatches | Each accessible name starts with the visible text | UI-04 |
| A08 Animations ignore reduced-motion settings | Global reduced-motion policy | UI-02 |
| A11 Repeated H1s and a nested `main` | One page-header component owns the H1 and landmarks; focus moves to the H1 on route change | UI-04 |

**Beyond the audit**

- Replace the 27 clickable `div` and `span` elements with buttons or links.
- Replace the 18 browser `confirm` and `prompt` calls with an accessible dialog that returns focus to its trigger.
- Announce job progress, toasts and enrichment results through live regions. Rate-limit them so screen readers are not flooded.
- Provide keyboard alternatives for dragging, such as column reorder and resize (WCAG 2.5.7).
- Give every chart a data-table view and a text summary.
- Make pages reflow at 320 px width and zoom to 400 percent without losing content.

**Gates.** Owner: accessibility QA.

- CI: run axe-core across a fixed matrix of routes and states, in light and dark themes, at desktop and 390 px widths. A PR cannot merge with any serious or critical violation.
- Lint: enable `eslint-plugin-jsx-a11y` in recommended mode.
- Release: test the 5 core tasks with NVDA on Firefox, VoiceOver on Safari, and keyboard only. Publish an accessibility statement.

## 3. Ease of use

The goal is time to first value under 10 minutes: from `docker compose up` to an enriched, exported list, without reading the docs. Today the first-run page (`setup.tsx`) creates the admin account. After that, the user meets 24 routes and an empty workspace.

- **Guided first run.** Owner: product designer. After admin setup, show a 4-step checklist: connect one LLM key, add one search source, import a sample or own CSV, and run one enrichment. Each step shows its status and can be skipped.
- **Sample workspace.** One click loads demo data: 50 synthetic companies, so every page has something to show. The data is clearly labelled and can be removed.
- **Empty states that teach.** Every empty page says what it is for, shows the one action that fills it, and links to a one-minute guide.
- **Safe defaults.**
  - Free or keyless providers are on by default.
  - Paid providers stay off until a key is added.
  - Outreach sending stays off until a domain is verified.
- **Error messages with a next step.** Every failure names the cause in plain words and offers the fix, for example "Apollo key rejected: check it in Settings › Providers". Raw HTTP codes and stack traces never reach the UI.
- **Undo instead of confirm.** Reversible actions such as archive, remove from list and column delete happen at once with a 10-second undo. Only irreversible actions ask first.
- **Templates for the top jobs.** Ready-made workbooks for:
  - finding decision makers at target accounts
  - enriching a CRM export
  - building a lookalike list
- **Settings search.** One search box across providers, sources and preferences (UI-11).
- **In-product help.** Tooltips explain terms such as credits, waterfall and audience. A help panel links to the docs.

**Acceptance criteria**

- 4 of 5 first-time testers finish the checklist unaided.
- Setup questions from users fall by half after release.

## 4. Security and hardening

The runtime controls are strong: tenant row-level security, encrypted secrets, a dial-time egress guard, rate limits, SSO and an audit log. The gaps are verification, supply chain, account security and browser-side headers.

Owner: security engineer. Every change in this section gets a second reviewer.

**4.1 Close what is open**

- Finish the remaining verification items in #114:
  - provider attribution end to end
  - saved-key updates per workspace
  - MCP production wiring
  - invalid-IPv6 account handling
  - egress tests for the retained browser service
- Pass the #120 release gates: a staging upgrade from Python, a backup restore drill and a rollback rehearsal.
- Publish the supported versions, and a table of affected and fixed versions, in `SECURITY.md`.

**4.2 Supply chain** (none of these run in CI today)

- Run `govulncheck`, CodeQL for Go and TypeScript, and `gitleaks` on every PR. Fail the build on high severity.
- Pin every GitHub Action to a commit SHA. CI currently pins by tag, for example `actions/setup-go@v7`.
- For every image, generate a CycloneDX SBOM and run a Trivy scan. Sign images and attach SLSA build provenance with cosign.
- Add the OpenSSF Scorecard workflow and badge, branch protection with required reviews, and signed tags.

**4.3 Application**

- Set a strict Content-Security-Policy, HSTS, `X-Content-Type-Options`, `Referrer-Policy` and `frame-ancestors` on the app. Today the CSP is set only on the public outreach page.
- Add MFA with TOTP and passkeys (WebAuthn) for local accounts, and require it for global admins. No MFA exists today.
- Harden sessions: idle and absolute timeouts, sign out of all sessions, and a notice when a new device signs in.
- Issue per-workspace API tokens with scopes and expiry, and show when each was last used.
- Review the network access of every outside provider PR against a checklist. For example, the review of #139 found that an operator allow-list entry must name a host and port, never the host alone.

**4.4 Data protection**

- Support per-person export and erasure for GDPR and CCPA requests, across records, outreach history and caches.
- Show retention policies in Settings, and verify deletion with tests.
- Write a key rotation runbook for the secrets envelope, and support an external KMS as an option.

**4.5 Assurance**

- Write a threat model for each trust boundary: API, worker, browser service, MCP and inbound webhooks. Refresh it each release.
- Commission an external penetration test before 1.0. Start a bug bounty or hall of fame after.
- Fuzz the parsers: CSV import, manifests, and email and URL normalisation.

## 5. Import, export and migration

Adoption depends on switching cost. A team should be able to move its data in within an hour, and be able to leave just as fast.

Today OpenGTM can:

- import CSV, with a Clay mapping ([guide](../clay-migration.md))
- export leads
- push to HubSpot, Salesforce, Instantly and Smartlead

It has no staged review before an import is written, no undo, and no full workspace export. Owner: data-platform engineer.

| Source | Today | Target |
| --- | --- | --- |
| Generic CSV or XLSX | CSV import into shared records | Staged import: preview, column mapping with memory, duplicate preview, error report, undo |
| Clay | Mapping for Clay's CSV export | Map tables and enrichment columns to workbook templates; a migration report of what could not move |
| Apollo | No dedicated importer | Mapping for saved-list and contact CSVs; optional API import with the user's key |
| HubSpot | Push destination | Two-way sync: import companies, contacts and owners; field mapping; incremental sync |
| Salesforce | Push destination | Import accounts, contacts and leads; field mapping; sandbox support |
| Instantly, Smartlead | Push destinations | Import campaign history and suppression lists |
| ZoomInfo, Lusha, Pipedrive, Attio | None | CSV mappings first; API import if users ask for it |

**Staged import contract** (spec work package UI-08)

1. Upload a file or connect an account; detect the source platform from the headers.
2. Map columns: suggest mappings and remember them per source.
3. Preview 50 rows, duplicate matches and validation errors before anything is written.
4. Commit as one batch with an ID. Retrying the same batch does not duplicate data.
5. Offer a downloadable error report, and allow undoing the batch for 7 days.

**Export and portability**

- Full workspace export: records, workbooks with column definitions, audiences, templates and automations, as a versioned ZIP of JSON and CSV files plus a manifest.
- Restore from that export into a new workspace or instance, with a round-trip test in CI.
- Scheduled exports to S3-compatible storage, recorded in an export audit log.
- Never export secrets. List them instead, so the user can re-enter them.

**Acceptance criteria**

- A 10,000-row Clay or HubSpot export migrates in under 15 minutes, and the migration report lists no lost fields.
- Export, restore and re-export produce identical data.

## 6. Reliability, performance and operations

Operators cannot yet see whether OpenGTM is healthy. There are liveness checks (`/healthz`, `/health`), but the Go code has no Prometheus metrics or OpenTelemetry traces. Owner: SRE.

- **Metrics.** Expose a Prometheus endpoint covering:
  - API latency
  - job queue depth and age, and job outcomes by type
  - provider calls, errors and spend
  - outreach sends and bounces
- **Tracing.** OpenTelemetry traces from API request to worker job to provider call. Include the workspace ID as an attribute; never include PII.
- **Logs.** Structured JSON with request and job IDs, with tested PII redaction.
- **Dashboards and alerts.** Ship a Grafana dashboard and alert rules for queue age, error rate, provider failure rate, disk usage and database size.
- **SLOs** (*proposed*):
  - API p95 latency under 300 ms for reads
  - 99 percent of enrichment jobs start within 60 s
  - 99.5 percent monthly availability for the hosted demo
- **Resilience.**
  - Circuit breakers and backoff per provider.
  - A view of failed jobs with one-click retry.
  - Graceful shutdown that lets workers finish their current jobs.
- **Performance budgets.**
  - A 10,000-row workbook scrolls at 60 fps.
  - The initial JavaScript bundle stays under 300 KB gzipped.
  - The existing load-test runbooks in `docs/operations/` run nightly in CI.
- **Operations.** A backup and restore runbook, tested monthly; zero-downtime migrations; a Helm chart alongside Docker Compose. *Proposed.*

## 7. Data quality, providers and integrations

Users trust a GTM tool only when every value shows where it came from and how certain it is. Outside PRs #140 and #152 exposed three gaps:

- Discovery defaulted to India.
- Emails found by search were labelled "verified".
- Duplicates slipped through.

Owner: data-quality engineer.

- **Honest confidence.** Use one vocabulary for email status across every provider: found, pattern, smtp_verified, verified, risky, role, free. Show the status in the UI with its source.
- **Provenance on every field.** Show the provider, time and cost of each value in the record inspector (spec UI-07).
- **Market awareness.** Set the primary market per workspace in Settings, not through a process environment variable. Match market names as whole words.
- **Entity resolution.** Deduplicate by domain identity. Run a benchmark suite with ground truth in CI, and report precision and recall each release.
- **Provider policy** (decided 18 Nov 2026). Vendor integrations are welcome when they are opt-in, inactive by default, turned on by the user's own key, and disclosed by the vendor.
- **Provider SDK.** A documented manifest and Go adapter template with recorded-fixture tests, so every provider PR follows one pattern. Precedents: Firecrawl #118 and CompanyEnrich #128.
- **Provider health.** Show success rate, latency and cost per provider in Settings. Pause a failing provider automatically and tell the user.

## 8. Docs, developer experience and testing

The repository holds deep internal docs: plans, runbooks and audits. Users, though, need four short paths: install, first workflow, migrate and operate. Owners: developer-experience engineer and technical writer.

- **User docs** (in `apps/docs`): a quick start, a guide per core task, a migration guide per source platform, and a settings reference generated from the registry.
- **Operator docs:** install, upgrade, backup and restore, scaling, a security hardening checklist, and observability.
- **Contributor docs:** a one-page architecture overview, the provider SDK, the PR checklist, and how to run the Go integration suite locally.
- **Archive:** move superseded plans and per-plan cutover runbooks to `docs/archive/`, so search returns current material first.
- **Frontend tests:**
  - Fix the failing navigation test (audit A15).
  - Add Playwright end-to-end tests for the 5 core tasks.
  - Add visual regression snapshots per route.
- **Backend tests:** keep the PostgreSQL and Redis suite green, fuzz the parsers, and add contract tests for every public API route.
- **API reference:** generate an OpenAPI spec from the routes and publish it with the docs.

## 9. Release, governance and community

Outside contributions are arriving faster than review capacity. On 18 November, 6 outside PRs were open, and none had run CI until the maintainer approved them by hand. Owner: maintainer. The project is not recruiting co-maintainers (decided 18 Nov 2026; #122 closed).

- **Release train.** Monthly minor releases with a changelog, upgrade notes and a security section. Semantic versioning from 1.0.
- **CLA.** AI tools named in co-author trailers no longer need to sign; the human submitter signs for the work (PR #153, 18 Nov 2026).
- **PR flow.**
  - Label PRs by area automatically.
  - Review against a checklist: tests, network access, secrets, accessibility, docs.
  - Merge stacked PRs in order.
- **CI for outside PRs.** Keep the approval step for first-time contributors, but approve within one working day, and say so in `CONTRIBUTING.md`.
- **Public roadmap.** A GitHub project board that mirrors the phases below, with `good first issue` labels for each workstream.

## Roadmap

The plan assumes a start on 23 November 2026 and one owner per workstream. Each phase ends at a gate, and the next phase starts only when that gate passes.

| Phase | Weeks | Dates | Scope | Gate |
| --- | --- | --- | --- | --- |
| 1. Trust foundations | 1–6 | 23 Nov 2026 – 3 Jan 2027 | UI-00 to UI-03; UI-06 grid semantics; axe and lint gates in CI; #114 closure; #120 release gates; supply-chain CI; SHA-pinned actions | Zero serious axe violations on core routes; #120 gates passed; **1.0 release candidate** |
| 2. Clarity | 7–12 | 4 Jan – 14 Feb 2027 | Navigation regrouping (6 task areas); page header and landmarks (UI-04); records workbench (UI-05); first-run checklist and sample workspace; staged CSV import (UI-08); Prometheus metrics | 4 of 5 testers finish onboarding unaided; import preview, error report and undo shipped |
| 3. Switching | 13–20 | 15 Feb – 11 Apr 2027 | Clay, Apollo and HubSpot importers; full export and restore; HubSpot two-way sync; MFA; app CSP and headers; provider SDK; user and operator docs | 10,000-row migration under 15 minutes; export round-trip test green; external penetration test passed; **1.0** |
| 4. Scale | 21–26 | 12 Apr – 23 May 2027 | Salesforce import; tracing and SLOs; performance budgets; secondary screens (UI-09 to UI-11) | SLOs met for 30 days; performance budgets enforced in CI |

**Success metrics**

- Time to first value: under 10 minutes.
- Zero serious or critical accessibility violations on core routes.
- No known high-severity vulnerability older than 14 days.
- Migration completes with no lost fields reported.
- Outside PRs get a first review within 3 working days.

## Open decisions

| Decision | Recommendation | Main tradeoff | Needed by |
| --- | --- | --- | --- |
| Navigation regrouping into 6 task areas | Approve, after testing with 5 users | Familiar URLs change, so add redirects | Start of phase 2 |
| Freeze new features during phase 1 | Yes, except outside PRs already in review | Fewer new features to announce for 6 weeks | Now |
| Require MFA for global admins | Yes, at 1.0 | One more setup step for self-hosters | Phase 3 |
| Scope of two-way CRM sync | HubSpot first, Salesforce second | Salesforce has more enterprise demand but costs more to build | Phase 3 |
| Hosted demo with SLOs | Defer until after 1.0 | Fewer trial users before then | Phase 4 |
| Who staffs each workstream | Name one owner per section | Sections without an owner will slip | Now |
