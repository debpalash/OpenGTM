---
title: Chat and autopilot
description: A tenant-scoped chat that can search, enrich, build workbooks, track signals, and turn a compound goal into a plan you approve.
sidebar:
  order: 9
---

The chat endpoint speaks a CopilotKit-compatible protocol and streams
responses with tool use. It is the same engine the web UI's Chat page uses.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `POST /api/copilotkit` | Chat; streams `data: {…}` frames, ending with `data: [DONE]` |
| `GET/POST /api/copilotkit/info` | Runtime handshake and advertised actions |
| `GET /api/copilotkit/conversations`, `GET/DELETE .../{id}` | History per workspace and user |
| `GET /api/copilotkit/memories` | Stored memories |

Request body: `{messages: [{role, content}], conversation_id?, context?,
approved_tool_calls?}`. Frames carry `conversation_id` first, then `content`
deltas and structured frames such as `intent_clarification` or a confirmation
request.

## What chat can do

Read-only, no confirmation: `search_leads`, `get_lead_detail`,
`get_lead_stats`, `find_similar_leads`, `get_enrichment_gaps`,
`suggest_outreach`, `compare_leads`, `ambitionbox_search`, `ambitionbox_jobs`,
`find_people_at_company`, `verify_people_at_company`, `draft_plan`.

Requires your confirmation (medium): `update_lead_status`, `enrich_lead`,
`import_ambitionbox_to_workbook`, `set_workbook_refresh`, `add_agent_column`,
`add_signal_trigger`, `track_account_signals`, `draft_grounded_outreach`,
`create_people_workbook`, `enrich_people_contacts`.

Requires your confirmation (high, spends resources): `start_collection`,
`create_source_workbook`, `execute_plan`.

Dangerous tools never run on the first pass. The stream returns a confirmation
request; the client resubmits with `approved_tool_calls` listing what was
approved or denied.

## Autopilot: plan, then execute

For a compound goal such as "build a list of 50 IT staffing firms in Pune and
find their founders' emails", chat calls `draft_plan`, which returns a
step-by-step plan **without executing anything**. When you approve,
`execute_plan` runs the plan the server stored under that `plan_id` with a
single-use nonce: it creates the workbook, adds the source, adds agent columns,
sets refresh, and runs sourcing and enrichment on the durable queue.

Terse messages that could mean several GTM jobs (for example a bare company
name) get an `intent_clarification` frame instead of a broad collection run.

## Tenancy

The chat path has one tenant decision point. On cloud installs
(`CHAT_REQUIRE_AUTH`, defaulting to the PostgreSQL lead store setting) it
authenticates exactly like the REST API: bearer token plus `X-Workspace-Id`,
membership enforced, no fallback to a default workspace. A `conversation_id`
you do not own is treated as missing, so a forged id can never read or append
to someone else's chat. Self-hosted single-tenant installs can run keyless
against the `main` workspace.

Each turn is bounded by `CHAT_MAX_TOOL_ROUNDS` (8) tool rounds.
