# Clay Suite Parity Roadmap

Updated: 2026-09-13

OpenGTM should pursue workflow parity, not imitate every Clay screen. The target is a credible, self-hosted GTM operating system whose core loop—discover, enrich, segment, act, learn—works end to end.

## Current position

| Capability | OpenGTM today | Parity target | Priority |
| --- | --- | --- | --- |
| Tables and enrichment | Strong workbook, waterfalls, formulas, AI/research and output columns; horizontally safe queue with bounded parallel slots, indexed tenant ownership, advisory-lock-enforced tenant backpressure/fair claiming, graceful drain, admin telemetry, and a repeatable controlled-load gate | Execute the load gate on PostgreSQL at supported scale and broaden provider coverage | P0 |
| Discovery | Multi-source collection, provenance, canonical entities | More verified providers and repeatable source quality | P0 |
| Audiences | Persistent workspace audiences, materialized membership, entry/exit history, restart-safe scheduled refresh, live diffs, destinations, profile timelines and account-level coverage/intent rollups | Controlled-live destination validation | P0 |
| Signals | Watches, intent polling, automation triggers, audience entry/exit triggers, unified profile timelines, tenant-safe volume/momentum/source/account analytics, and fail-closed signed source maturity | Controlled-live certifications for JobSpy, SEC EDGAR, website, technology and news sources | P1 |
| Activation | Durable audience destination runs and delivery ledger for webhooks, HubSpot, Salesforce, Instantly, Smartlead, idempotent-key Google Sheets upserts, atomic Airtable upserts, checksum-manifested JSONL warehouse ingestion and paid media; authenticated idempotent CRM inbound reconciliation with conflict policies; outreach/output columns; fail-closed signed support-maturity catalog | Controlled-live CRM, sequencer, Sheets, Airtable, and warehouse certifications plus additional destination breadth | P1 |
| Ads | Consent-gated, SHA-256 hashed batch sync to Meta Custom Audiences, Google Customer Match and LinkedIn Matched Audiences; fail-closed signed support maturity | Controlled-live certification against approved platform accounts | P2 |
| Agents | Agent/research columns, MCP and Copilot surfaces; versioned chained research playbooks with prior-step context, an Agents composer, bounded audience runs, durable per-profile results, restart-safe recurring schedules, and signed capability maturity | Controlled-live certification of grounded research, chaining, audience runs, and schedules | P1 |
| Governance | Workspace member lifecycle and capability policies; RLS; audit and retention; tenant-bound OIDC SSO; workspace-scoped SCIM 2.0 User and Group synchronization with hashed rotatable tokens; audited workspace-scoped credentials across CRM, sequencer, Airtable and Google Sheets outputs | Controlled-live IdP, directory, and connector validation | P2 |
| Ecosystem | Versioned YAML connector SDK, deterministic `.ogc` bundles, Ed25519 detached signatures, fail-closed installer, publisher trust store, runtime trust policy, CI review artifacts, BYOK providers, and a workspace-aware catalog that separates publisher trust from signed controlled-live maturity | Broader controlled-live certified community catalog | P2 |

## Ordered rollout

1. **Prove the core.** Complete ten consecutive HMAC-attested controlled-live gauntlet passes, publish a tagged release, and keep migrations plus RLS release-blocking.
2. **Make Audiences the shared object.** Persist filter definitions, refresh membership, record profile activity, and emit membership-change events.
3. **Close the activation loop.** Add durable destination runs, retries, health, mappings, and bidirectional HubSpot/Salesforce/warehouse sync.
4. **Ship agentic workflows.** Package account research and outbound plays around audiences, signals, workbooks, and MCP.
5. **Add paid-media destinations.** Implemented consent-aware hashed audience sync for Meta, Google and LinkedIn; controlled-live validation remains required before these drivers graduate from beta.
6. **Scale the ecosystem and enterprise layer.** Provider SDK, marketplace packaging, SSO/SCIM, policy controls, and higher-volume execution.

## Definition of parity

A capability only counts when it is tenant-safe, API-addressable, observable, retryable where applicable, tested offline, and validated against a real external system before its release claim changes from beta to supported.
