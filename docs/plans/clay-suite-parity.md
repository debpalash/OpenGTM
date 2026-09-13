# Clay Suite Parity Roadmap

Updated: 2026-09-13

OpenGTM should pursue workflow parity, not imitate every Clay screen. The target is a credible, self-hosted GTM operating system whose core loop—discover, enrich, segment, act, learn—works end to end.

## Current position

| Capability | OpenGTM today | Parity target | Priority |
| --- | --- | --- | --- |
| Tables and enrichment | Strong workbook, waterfalls, formulas, AI/research and output columns | Production-scale execution and broader provider coverage | P0 |
| Discovery | Multi-source collection, provenance, canonical entities | More verified providers and repeatable source quality | P0 |
| Audiences | Persistent workspace audiences, materialized membership, entry/exit history, restart-safe scheduled refresh, live diffs, destinations and unified profile timelines | Controlled-live destination validation and richer account-level rollups | P0 |
| Signals | Watches, intent polling, automation triggers, audience entry/exit triggers, and a unified profile timeline | Signal analytics and additional verified sources | P1 |
| Activation | Durable audience destination runs and delivery ledger for webhooks, HubSpot, Salesforce and paid media; outreach/output columns | Bidirectional CRM/warehouse sync and additional destination drivers | P1 |
| Ads | Consent-gated, SHA-256 hashed batch sync to Meta Custom Audiences, Google Customer Match and LinkedIn Matched Audiences | Controlled-live validation against approved platform accounts | P2 |
| Agents | Agent/research columns, MCP and Copilot surfaces | Account research agents with reusable playbooks | P1 |
| Governance | Workspace roles, RLS, audit-oriented ledgers | SSO/SCIM, granular RBAC, retention and admin controls | P2 |
| Ecosystem | Connector framework and BYOK providers | Packaged marketplace and community connector SDK | P2 |

## Ordered rollout

1. **Prove the core.** Complete controlled-live validation, publish a tagged release, and keep migrations plus RLS release-blocking.
2. **Make Audiences the shared object.** Persist filter definitions, refresh membership, record profile activity, and emit membership-change events.
3. **Close the activation loop.** Add durable destination runs, retries, health, mappings, and bidirectional HubSpot/Salesforce/warehouse sync.
4. **Ship agentic workflows.** Package account research and outbound plays around audiences, signals, workbooks, and MCP.
5. **Add paid-media destinations.** Implemented consent-aware hashed audience sync for Meta, Google and LinkedIn; controlled-live validation remains required before these drivers graduate from beta.
6. **Scale the ecosystem and enterprise layer.** Provider SDK, marketplace packaging, SSO/SCIM, policy controls, and higher-volume execution.

## Definition of parity

A capability only counts when it is tenant-safe, API-addressable, observable, retryable where applicable, tested offline, and validated against a real external system before its release claim changes from beta to supported.
