# Clay Suite Parity Roadmap

Updated: 2026-09-13

OpenGTM should pursue workflow parity, not imitate every Clay screen. The target is a credible, self-hosted GTM operating system whose core loop—discover, enrich, segment, act, learn—works end to end.

## Current position

| Capability | OpenGTM today | Parity target | Priority |
| --- | --- | --- | --- |
| Tables and enrichment | Strong workbook, waterfalls, formulas, AI/research and output columns | Production-scale execution and broader provider coverage | P0 |
| Discovery | Multi-source collection, provenance, canonical entities | More verified providers and repeatable source quality | P0 |
| Audiences | Persistent workspace audiences, materialized membership, entry/exit history, restart-safe scheduled refresh, live diffs, destinations, profile timelines and account-level coverage/intent rollups | Controlled-live destination validation | P0 |
| Signals | Watches, intent polling, automation triggers, audience entry/exit triggers, unified profile timelines, and tenant-safe volume/momentum/source/account analytics | Additional controlled-live verified sources | P1 |
| Activation | Durable audience destination runs and delivery ledger for webhooks, HubSpot, Salesforce, checksum-manifested JSONL warehouse ingestion and paid media; authenticated idempotent CRM inbound reconciliation with conflict policies; outreach/output columns | Additional destination drivers and controlled-live CRM/warehouse validation | P1 |
| Ads | Consent-gated, SHA-256 hashed batch sync to Meta Custom Audiences, Google Customer Match and LinkedIn Matched Audiences | Controlled-live validation against approved platform accounts | P2 |
| Agents | Agent/research columns, MCP and Copilot surfaces; versioned chained research playbooks with prior-step context, an Agents composer, bounded audience runs, durable per-profile results and restart-safe recurring schedules | Controlled-live agent validation | P1 |
| Governance | Workspace roles, RLS, domain ledgers, append-only mutation auditing, and previewable scheduled retention with legal holds and immutable run summaries | SSO/SCIM, granular RBAC and identity lifecycle controls | P2 |
| Ecosystem | Versioned YAML connector SDK, machine-readable schema, contributor template, CI validator, BYOK providers and workspace-aware catalog API | Signed package distribution, review automation and broader community catalog | P2 |

## Ordered rollout

1. **Prove the core.** Complete controlled-live validation, publish a tagged release, and keep migrations plus RLS release-blocking.
2. **Make Audiences the shared object.** Persist filter definitions, refresh membership, record profile activity, and emit membership-change events.
3. **Close the activation loop.** Add durable destination runs, retries, health, mappings, and bidirectional HubSpot/Salesforce/warehouse sync.
4. **Ship agentic workflows.** Package account research and outbound plays around audiences, signals, workbooks, and MCP.
5. **Add paid-media destinations.** Implemented consent-aware hashed audience sync for Meta, Google and LinkedIn; controlled-live validation remains required before these drivers graduate from beta.
6. **Scale the ecosystem and enterprise layer.** Provider SDK, marketplace packaging, SSO/SCIM, policy controls, and higher-volume execution.

## Definition of parity

A capability only counts when it is tenant-safe, API-addressable, observable, retryable where applicable, tested offline, and validated against a real external system before its release claim changes from beta to supported.
