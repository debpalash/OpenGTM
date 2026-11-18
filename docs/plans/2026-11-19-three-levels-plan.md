# OpenGTM plan: three levels past Clay

As of 19 November 2026. Owner: project maintainer. This plan replaces the priorities and roadmap in the [18 November upgrade plan](2026-11-18-expert-upgrade-plan.md). That plan's workstream detail (sections 1 to 9) still applies where this one does not say otherwise.

## Goal

OpenGTM should be the tool that GTM engineers, and the agents working for them, choose over Clay. It gets there in three levels. Each level has a test that can be run, so "better" is measured and not claimed.

| Level | What it means | Test |
| --- | --- | --- |
| 1. Usable | Anyone can install the current product and get a filled table. | A published 4.x release; a clean install reaches an enriched, exported list in under 10 minutes; the demo fills at least 80% of its cells. |
| 2. Open | Everything Clay gates by plan is included, and the catalogue of sources and providers is open for anyone to extend. | 100 or more providers, most of them contributed as manifests; every cell shows its source, time and cost; a workspace exports and restores without loss. |
| 3. Better to use | Clearer, faster and more accessible than Clay, for people and for agents. | Zero serious accessibility violations on every route; 6 task areas in place of 24 routes; every action available through the API, the CLI and MCP; a hosted edition that people pay for. |

## What changed since 18 November

Checks on 19 November corrected several assumptions in the earlier plan.

- **The five P0 audit findings (A01 to A05) are fixed in code.** Analytics shows honest error states, text tokens meet contrast, source switches and settings buttons are named, and the workbook grid has row and column counts. The 9 and 10 px text is gone.
- **An accessibility gate already runs in CI.** It covers 13 routes in light and dark themes and 4 at phone width. Its baseline still allows one rule, `target-size`, on 18 to 27 elements per route. Six routes are not covered.
- **The open queue is empty.** 0 open PRs; 2 open issues (#134, kept on purpose, and #120, release readiness).
- **The supported-versions policy is decided** (`SECURITY.md`): 4.x is supported, and 3.x gets security fixes for 90 days after v4.0.0.
- **The provider count is about 30 against Clay's advertised 150 or more.** Only 4 providers use the manifest format, and its documentation is out of date.
- **No current plan covers a hosted edition.** The building blocks are partial: prepaid Stripe credits exist, but self-service sign-up, subscriptions and plan entitlements do not.

## Level 1: usable

Nothing else matters until people can install what is on `main`.

| Work | State on 19 Nov | Done when |
| --- | --- | --- |
| Release gates (#120) | Release workflow builds images on a tag. No scripted install, restore or upgrade rehearsal. | Install, backup and restore, and upgrade rehearsals run as scripts and pass in CI. |
| v4.0.0 | Not tagged. Changelog does not describe the Go cutover. | Tag pushed by the maintainer after the gates pass; release notes state the 3.x end-of-support date. |
| First-run fill rate | Last measured in July on the Python version: 31%. Unknown for Go. | Measured on a clean install with no paid keys, recorded in `docs/benchmarks/`, and at least 80%. |
| First-run path | After setup the user lands on an empty chat. Demo data loads only through a compose profile. | A 4-step checklist (LLM key, source, import or sample data, first enrichment) and a "Load sample workbook" button. |
| Try without installing | Needs Docker and about 4 GB of RAM. No one-click deploy. | A lighter compose profile, one-click deploy configs for two platforms, and a read-only public demo. |
| Quick integration wins | No direct OpenAI provider; no Attio, HeyReach or lemlist. | All four shipped with tests and docs. |

## Level 2: open

### Nothing gated

Clay's pricing page puts webhooks, the HTTP API column and CRM sync on its Growth plan, and warehouse sync, SCIM and IP allow-lists on Enterprise. It charges one Action per run even with the customer's own keys, and its free plan stops at 200 rows per table. (Read from clay.com/pricing on 19 Nov 2026. The page gives two prices for some plans, so re-check the wording before quoting it in public.)

OpenGTM's rule: **no capability is ever gated in the self-hosted product.** The hosted edition charges for running the service, not for features (see "Hosted edition").

### The catalogue: our own sources and hundreds of paid services

The catalogue grows in two directions.

**Paid services, through manifests.** A manifest is one YAML file that describes a vendor's HTTP call: authentication, request template and response mapping. The user supplies their own key. Adding a provider should take an afternoon and need no Go.

- Correct the format documentation and publish a contributor guide with a worked example, a test recipe and a PR checklist.
- Ship manifests in packs: 15 by the end of November, 50 by the end of January 2027, 100 by 1.0.
- Accept a manifest only when its request and response are confirmed against the vendor's official API documentation and it has a test built on the vendor's sample response.
- Extend the format where several vendors need it: polling for asynchronous APIs, pagination, and OAuth 2.0.
- Keep the provider policy of 18 November: opt-in, inactive by default, turned on by the user's own key.

**Our own sources, built on open-source work.** These are free to run and are what a hosted competitor cannot copy cheaply. `docs/research/` already lists candidates (`leadgen-oss-ecosystem.md`, `sources-master-inventory.md`).

- Each source is a Go package with recorded fixtures, a health check and a licence review. AGPL-compatible code may be adapted with attribution in `NOTICE`. Anything else is reimplemented from the public behaviour, not copied.
- Priorities: website visitor identification (the largest signal type we lack), social and community signals, company registries for more countries, job-posting and technology-change signals.
- Every source reports its hit rate and freshness, shown in Settings.

**Automatic provider order.** Clay's waterfall order is manual. OpenGTM should order providers by measured hit rate and cost for each kind of record (`provaccuracy` is the start) and show the reason. This is a real technical advantage, and it gets better as the catalogue grows.

### Trust in every cell

- Source, time, cost and confidence on every value, in the record inspector.
- A cost estimate before any paid or bulk run: what will run, on how many rows, at what price.
- One vocabulary for email status across providers: found, pattern, smtp_verified, verified.

### Easy to arrive, easy to leave

Staged import with preview and undo; importers for Clay, Apollo and HubSpot; full workspace export and restore with a round-trip test. Detail is in section 5 of the 18 November plan.

## Level 3: better to use

### Structure

- **Six task areas in place of 24 routes:** Find, Records, Enrich, Engage, Measure, Admin. Old URLs redirect.
- **One primary action per page.** Secondary actions go in a menu. Destructive actions use an in-app dialog that names what is being destroyed.
- **One design system.** `src/design-system` becomes the only import path for primitives, enforced by a lint rule. Tokens have one source. Today 36 files import the design system and 71 import the older component layer directly.
- **Smaller modules.** Split the workbook editor (2,730 lines), automations (1,776) and outreach (1,218) as each screen is reworked.

### Visual quality

- A calm, dense, data-first look: one accent colour, neutral surfaces, numbers in tabular figures, and motion only where it explains a change.
- Comfortable and compact density modes, saved per user.
- Light and dark themes checked by the same contrast tests.
- Every empty page says what it is for and offers the one action that fills it.

### Accessibility

Target: WCAG 2.2 AA on every route.

- Remove the last `target-size` exceptions and bring the baseline to empty.
- Cover every route in the CI gate, at desktop and phone widths.
- Replace the 16 native `confirm` and `prompt` calls with accessible dialogs, and block new ones with a lint rule.
- Keyboard alternatives for every drag action; a data-table view for every chart.
- Before each release, test the 5 core tasks with a screen reader and keyboard only. Publish an accessibility statement.

### Built for agents

GTM agents are users of this product, not an add-on.

- Every UI action has an API route, documented in a generated OpenAPI spec.
- The MCP server covers the full loop: find, enrich, review, activate. Spending actions state their cost and need a scope that allows them.
- A `gtm` command-line tool wraps the API for scripts and CI.
- Workbooks, playbooks and automations can be exported as files, reviewed in a pull request and applied again.
- Per-workspace API tokens with scopes, expiry and last-used time.

## Hosted edition

The earlier position was to defer a hosted product. This plan sets the terms and starts the groundwork, because the same work (sign-up, entitlements, metering) also improves self-hosting.

**Principles**

1. The self-hosted product stays complete. Nothing moves from open source to paid.
2. The hosted edition charges for operation: uptime, upgrades, backups, managed browser and search capacity, and support.
3. Bring-your-own keys carry no markup and no per-run fee.
4. Managed keys, where offered, are billed at provider cost plus one published percentage.
5. Enterprise pricing covers governance and assurance (contracts, SLAs, dedicated instances), never capability.

**Proposed plans** (prices are a starting point for testing, not a commitment)

| Plan | Price | Includes |
| --- | --- | --- |
| Self-hosted | Free, AGPL-3.0 | Everything. |
| Cloud | Flat monthly fee per workspace (test $49 and $99), unlimited seats | Hosted instance, own keys at zero markup, daily backups, managed search and browser pool with a fair-use limit. |
| Cloud with managed keys | Cloud plus usage | Provider calls through our keys, at cost plus a published percentage, paid from prepaid credits. |
| Dedicated | Annual contract | Single-tenant instance, chosen region, SLA, security review support. |

**What exists and what is missing**

| Block | State | Needed |
| --- | --- | --- |
| Tenant isolation | PostgreSQL row-level security, per-workspace secrets | An external audit before any paying tenant. |
| Billing | Stripe Checkout top-up and a credit ledger | Subscriptions, a customer portal, invoices, dunning. |
| Sign-up | First-run admin setup and invites only | Self-service sign-up with email verification and abuse controls, behind a hosted-mode switch. |
| Entitlements | Role gates only | A small entitlement layer: plan limits on fair-use resources (search and browser capacity), never on features. |
| Metering | Per-workspace spend ledger | Usage reports per workspace and a monthly statement. |
| Operations | Liveness checks, loopback metrics | Metrics, tracing, alerts, tested restores, an incident runbook, a status page. |
| Outreach abuse | Sending stays off until a domain is verified | Sending limits, complaint handling and a clear acceptable-use policy for hosted tenants. |
| Legal | AGPL-3.0, CLA in place | Terms, privacy policy, a data processing agreement, sub-processor list. |

**Gates before charging anyone**

- v4.0.0 released, and at least 3 outside teams running it in production for 90 days.
- External security audit of tenant isolation passed.
- Backup restore tested monthly, with the last result published.
- Hosted mode behind one configuration switch, off by default, with tests in both modes.

**Sequence:** waitlist page after v4.0.0; private beta with 5 to 10 design partners; paid general availability after the gates.

## Roadmap

Dates assume the maintainer plus agent teams. Each phase ends at a gate.

| Phase | Dates | Scope | Gate |
| --- | --- | --- | --- |
| 0. Ship | 19 Nov – 6 Dec 2026 | Release rehearsals; OpenAI, Attio, HeyReach, lemlist; first manifest pack and guide; dialogs, target sizes and full route coverage in the accessibility gate; first-run checklist and sample workbook; fill rate measured | **v4.0.0 published** |
| 1. Clarity | 7 Dec 2026 – 17 Jan 2027 | Six task areas; one design system; density modes; staged CSV import; cost preview; provenance in the inspector; one-click deploy; public demo; 50 manifests | 4 of 5 new users finish the checklist unaided; zero serious accessibility violations on all routes |
| 2. Open | 18 Jan – 14 Mar 2027 | Visitor identification; social signals; automatic provider order; Clay, Apollo and HubSpot importers; workspace export and restore; OpenAPI spec; `gtm` CLI; scoped API tokens; MFA; supply-chain controls; 100 manifests | Export round-trip test green; 10,000-row migration under 15 minutes; **1.0** |
| 3. Cloud | 15 Mar – 31 May 2027 | Hosted mode, sign-up, subscriptions, entitlements, metrics and tracing, external audit, legal documents, private beta | Cloud gates above passed; first paying customers |

## How the work is run

- One lead reviews and decides. Implementation is delegated in small, independent tasks, each in its own branch with its own tests.
- Every change reaches `main` through a pull request with green CI. No task is reported done without the test output.
- External facts (a vendor's API, a competitor's pricing) come from the primary source, cited in the change.
- A missing measurement is recorded as a gap, never as zero.

## Success metrics

| Metric | Today | Target |
| --- | --- | --- |
| Published release with the Go runtime | None | v4.0.0 by 6 Dec 2026 |
| Time from install to first enriched export | Not measured | Under 10 minutes |
| Demo fill rate, no paid keys | Not measured for Go (31% in July, Python) | 80% or more |
| Providers in the catalogue | About 30 | 100 by 1.0 |
| Serious accessibility violations, all routes | 0 on 13 covered routes, with `target-size` exceptions | 0 on every route, empty baseline |
| Top-level navigation items | 20 (15 in three groups plus 5 utility) | 6 areas |
| Outside production installs known | Not measured | 3 before cloud beta |

## Decisions taken in this plan

| Decision | Choice | Reason |
| --- | --- | --- |
| Hosted edition | Plan it now, charge after 1.0 and an audit | The groundwork also serves self-hosters; charging before an audit risks tenant data. |
| Feature gating | None in self-hosted, ever | It is the clearest difference from Clay and the reason to trust the project. |
| Catalogue growth | Manifests first, native Go only when the format cannot express the API | Lowest cost per provider and open to contributors. |
| Navigation | Regroup into 6 areas with redirects | 20 items is the main source of clutter. |
| Feature freeze from the 18 November plan | Lifted for integrations and manifests; kept for new product surfaces until v4.0.0 | Integrations are low-risk and widen the audience for the release. |
