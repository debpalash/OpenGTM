---
title: Architecture
description: Runtime topology, data ownership, the durable job queue, tenant boundary and scaling rules.
sidebar:
  order: 1
---

This describes the deployed architecture, not an aspirational one, and states
the boundaries that must move before the single-node self-hosted topology
becomes a multi-node hosted service.

## Runtime topology

```text
browser / REST / MCP
        |
        v
nginx :3000/:3010  ---- static React assets
        |
        v
FastAPI (enqueue + query only; RUN_INLINE_WORKER=0)
        |                         |
        | PostgreSQL             | Redis tenant channels
        v                         v
jobs + tenant data <---- workers ----> live SSE progress
        ^                  |
        |                  +---- isolated provider processes / external APIs
        |
scheduler (reconciliation and recurring enqueue only)

migrate (one-shot owner) ---> Alembic ---> runtime role is non-super/NOBYPASSRLS
```

Every first-party Python service and the frontend asset publisher use one
immutable application image. PostgreSQL and Redis are private to the container
network and bound to loopback for host administration; nginx is the only LAN
listener.

## Data ownership

| Data | Authority | Isolation / durability |
|---|---|---|
| Leads, signals, workbooks, rows, cell traces, connector runs | PostgreSQL | `workspace_id`, fail-closed RLS, runtime `NOBYPASSRLS` role |
| Automations, outreach, watches, ingest, MCP audit | PostgreSQL | tenant RLS; non-tenant schedule mirrors contain no recipient data |
| Durable execution queue (`jobs`) | PostgreSQL | global control table; tenant identity is required in payloads and checked by handlers |
| Users | PostgreSQL | global authentication table |
| Workspace / member / active-workspace metadata | `data/workspaces.db` (SQLite) | authenticated control plane; shared-volume, single-node boundary |
| Workspace secrets | encrypted values in `data/workspaces.db` | role-gated and encrypted at rest; single-node boundary |
| Collection job / stage / LLM detail | per-workspace SQLite ledger | tenant file path; queue job remains durable in PostgreSQL |
| Live progress and reconnect history | Redis | exact tenant channel plus bounded 500-event list |
| Frontend bundle | immutable image → named volume → nginx | asset publisher refreshes it on every deploy |

PostgreSQL is the production data plane. SQLite remains a control-plane
compatibility layer, not the source of truth for production workbook or lead
rows.

## Request and tenant boundary

1. The access token resolves an active user. Refresh tokens are rejected on
   HTTP, SSE, and WebSocket authentication paths.
2. `X-Workspace-Id` (or the stored active workspace) is checked against
   workspace membership.
3. The request binds a context-local workspace and sets PostgreSQL
   `app.workspace_id` transaction-locally.
4. Owner, admin and editor roles gate mutations and spend; viewers are
   read-only.
5. Application checks provide clear 404/403 behaviour while PostgreSQL RLS is
   the final isolation boundary. A missing workspace GUC fails closed.

Workers do the equivalent with `workspace_scope(workspace_id)` for each claimed
job. A handler lacking a workspace id or slug fails instead of falling back to
a global tenant.

## Durable job lifecycle

```text
API transaction
  validate role + ownership
  create domain run/ledger record
  INSERT jobs(status=pending, tenant payload, optional fire_key)
        |
        v
worker claim: SELECT ... FOR UPDATE SKIP LOCKED
  status=processing, worker_id, locked_at, heartbeat
        |
        v
handler under workspace_scope
  emit Redis progress
  write page/cell/batch transaction
        |
        +--> success: completed
        +--> retryable error: retrying -> pending at next_run_at
        +--> terminal error: failed + domain failure reconciliation
        +--> cancellation: preserve cancelled; terminate child process
```

The queue supports priorities, bounded retries, backoff, heartbeats,
stale-claim recovery, per-job timeouts, killable subprocess work, and
`fire_key` single-flight constraints. Cancellation is a committed terminal
state: parent finalisation cannot overwrite it with success or failure.

API and agent entry points use the same queue. Copilot collection, bulk
enrichment, CSV imports, workbook sources, connector imports, refreshes,
automations, outreach, and recurring scans are never detached threads inside
an API process.

## Connector contract

Typed connectors return pages with normalised records, provider record IDs,
cursor and exhaustion state, source totals, and warnings. The worker commits a
page, its row upserts, counters, and the next cursor together. Unique
`(workbook_id, source_provider, source_record_id)` identity makes replay after
a crash idempotent. A partial unique index permits only one active run per
workbook/connector pair.

`connector_runs` distinguishes requested, fetched, added, updated, skipped,
target-met, exhausted, and failed states, so "100 requested" cannot be reported
as complete after silently returning 50.

## Progress delivery

The worker publishes only workspace-scoped events. The API subscribes to one
exact Redis channel after authenticating workspace membership; it never
pattern-subscribes to all tenants. A bounded Redis list lets reconnecting
clients replay recent events. In-process queues are a development fallback,
not the cross-process production transport.

## Outbound execution safety

- Tenant-facing URL fetches reject non-HTTP schemes, credentials, encoded IP
  tricks, private/loopback/link-local/metadata addresses, and hosts resolving
  to blocked addresses.
- Redirects are handled manually or disabled; every followed hop is checked.
- Browser routes check navigations and subresources before continuing.
- Automation webhooks use DNS-pinned connections with redirects disabled.
- Provider jobs have time and process limits; credentials resolve per workspace
  and are never placed in queue payloads.

A controlled egress proxy remains the appropriate final boundary for a public
hosted service, because application DNS checks cannot provide the same
network-level guarantee for every third-party browser or TLS stack.

## Scaling rules

- Scale workers horizontally; PostgreSQL row locking prevents double claims.
- Scale API processes on one host; API containers share the control-plane
  `data/` volume.
- Run exactly one scheduler unless scheduler leadership is added.
- Run migrations once with the owner role before starting the runtime image.
- Do not place API or worker replicas on independent hosts until workspace
  metadata, secrets, and collection-stage ledgers move from SQLite to
  PostgreSQL or a strongly consistent control-plane service.

## Remaining path to a hosted, multi-node service

1. Move workspace membership, secrets, active-workspace state, and collection
   ledgers into PostgreSQL with explicit control-plane policies.
2. Tenantise or remove the remaining global admin-only legacy utilities.
3. Put outbound fetches behind an audited egress proxy, move encryption keys to
   a managed KMS, and complete an external security review and restore drill.
4. Publish reproducible provider accuracy, coverage and cost evaluations and
   make the declarative registry a documented contribution surface.
5. Close visible product gaps: large-grid interaction, Clay-table import,
   dependency-aware reactive recompute, more templates, broader CRM and
   sequencer sync.
