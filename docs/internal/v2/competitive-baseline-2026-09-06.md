# Clay competitive baseline

Checked: 2026-09-06. Sources: official Clay product pages and documentation.
These are documented vendor capabilities, not independent performance tests.
OpenGTM observations below derive from its README and internal recovery plan;
implemented features are not assumed to be live-validated.

## Current comparison

| Area | Documented Clay baseline | OpenGTM V2 response |
|---|---|---|
| Canonical data | Audiences combines CRM, warehouse, and enrichment data into persistent people/account profiles and dynamic segments. [Audiences](https://university.clay.com/docs/audiences) | Shared account/person identity, merge lineage, evidence history, and reactive segments across every surface |
| Workflows | Versioned graphs support enrichment, code, agent and function steps, run traces, replay, pause/resume, and draft/publish history. [Workflows](https://university.clay.com/docs/workflows) | Build on the durable queue with version-pinned typed execution, fenced ownership, and replay-safe outputs |
| Enrichment | Clay advertises 150+ providers, waterfalls, and BYOK. [Pricing](https://www.clay.com/pricing) | Validate individual connector actions, provider ordering, exact identities, independent verification, and measured yield |
| Persistent research | Account Research Agents retain per-account memory, use connected context, and maintain auditable fields on schedules. The feature is open beta; BYOK models are not supported during that beta. [Account Research Agents](https://university.clay.com/docs/account-research-agents) | Evidence-backed memory, freshness and contradiction handling, selected BYOK models, and exact action continuity |
| Signals | Recurring signals write results to shared profiles and can serve multiple audiences. [Audiences](https://university.clay.com/docs/audiences) | Deduplicated events, source times, health/recovery receipts, and reliable downstream triggers |
| Reuse and MCP | Functions provide reusable logic; administrators can expose functions through MCP with user budgets and account scope. [Functions](https://university.clay.com/docs/functions), [MCP settings](https://university.clay.com/docs/mcp-settings) | Tenant-scoped, versioned functions with common UI/chat/API/MCP execution contracts |
| Activation | CRM, warehouse, HTTP, advertising, and sequencing capabilities are documented. [Integrations](https://www.clay.com/integrations), [Pricing](https://www.clay.com/pricing) | First prove CRM/webhook idempotency, mapping, readback and recovery; expand from measured usage |
| Governance | Connection allowlists constrain editing and use across features; enterprise plans document SSO/RBAC. [Connection access](https://university.clay.com/docs/access-settings-for-connections), [Pricing](https://www.clay.com/pricing) | Finish tenant/control-plane boundaries, connection permissions, audit export, and operational validation |
| Conversational building | Sculptor helps construct tables; newer workflow documentation also describes graph creation/editing from prompts. [Sculptor](https://university.clay.com/docs/sculptor), [Workflows](https://university.clay.com/docs/workflows) | Demonstrate exact follow-through across find, verify, workbook, watch, and draft actions on held-out requests |
| Economics | Clay separates platform Actions from Data Credits. BYOK avoids Data Credits but still consumes Actions; external model token costs are not displayed by Clay according to its documentation. [Actions and Data Credits](https://university.clay.com/docs/actions-data-credits) | Account for direct provider and model cost, reserve budgets atomically, and report cost per useful result |

## Pricing and documentation uncertainty

The current official FAQ lists Launch starting at $185/month and Growth at
$495/month. Pricing cards display lower annual-billing equivalents. Allowances
and feature descriptions are not fully consistent across official pages; capture
billing cadence and date before using any price comparison. [Plan FAQ](https://www.clay.com/faq/what-features-are-included-in-the-new-plans-how-do-i-pick-the-right-plan)

The Sculptor page and newer workflow documentation differ in their descriptions
of supported operations. Do not treat a limitation on one page as proof that the
current product cannot complete a task. Validate directly before competitive
marketing. Vendor row limits likewise do not establish independently measured
throughput or reliability.

## Differentiation hypotheses to test

1. Customer-operated infrastructure, retention, and portable open workflows.
2. Complete direct-cost BYOK accounting without a platform action meter.
3. Evidence and exact entity selections preserved throughout conversational work.
4. Transparent fault recovery and receipts for every persistent or external action.
5. Reproducible live benchmarks exposing accuracy, useful coverage, intervention,
   latency, recovery, and cost per accepted result.

These are proposed advantages, not established superiority. BYOK, MCP, chat
building, cost previews, persistent agents, and execution traces individually
are not unique: Clay documents each in some form.

## OpenGTM baseline cautions

- The June 6 parity specification is historical. Its near-parity language must
  not substitute for this current scope or a comparative benchmark.
- The recovery plan records ten successful local-native runs with recorded
  provider responses and zero completed controlled-live release runs. Preserve
  that distinction; local fixtures cannot establish market performance.
- The README acknowledges control-plane SQLite, remaining global utilities,
  hosted hardening, large-grid ergonomics, reactive recomputation, and breadth
  gaps. Evaluate actual code and live behavior before labeling those closed.
- Self-hosting does not mean no data leaves the server: configured external
  enrichment and LLM providers receive selected request data. Product claims
  should reflect the actual data flow.

Implementation sequencing, acceptance gates, and the comparative evaluation
method are in the [V2 delivery plan](README.md).
