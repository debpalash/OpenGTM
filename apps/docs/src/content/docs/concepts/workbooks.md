---
title: Workbooks
description: The spreadsheet-shaped engine at the centre of OpenGTM. Rows are leads, columns do work.
sidebar:
  order: 1
---

A **workbook** is a table whose rows are leads and whose columns are
instructions. Some columns hold fields (`lead_field`), some create rows
(`source`), and most compute a value per row by calling providers, LLMs, web
research agents, HTTP APIs or safe formulas. `output` columns push rows to the
outside world.

## Lifecycle

| Status | Meaning |
|---|---|
| `draft` | Columns are being edited; nothing has run |
| `running` | A run job is in the queue or executing |
| `paused` | Stopped by a user or a spend ceiling |
| `complete` | The last run finished |

Each cell carries its own status (`pending`, `running`, `complete`, `error`,
`skipped`) and a trace of what produced it.

## Runs are durable jobs

Nothing user-facing runs inside the API process. `POST .../run` validates your
role, records a run, and inserts a job into the PostgreSQL queue in one
transaction. Workers claim jobs with `SELECT ... FOR UPDATE SKIP LOCKED`,
execute under the row's workspace scope, and commit each page of cells with its
progress cursor. A crash repeats at most one idempotent upsert.

Every provider call runs in a killable subprocess pool (default 8 workers,
10-second timeout per call) so one hanging vendor never stalls a run.

## Rows have identity

Rows created by a source carry a `(workbook_id, source_provider,
source_record_id)` identity enforced by a unique index, so re-running a source
updates instead of duplicating. CSV imports and the ingest API deduplicate on
normalised website domain, falling back to normalised company name.

## Views, templates and functions

- **Saved views** store filters, sort and hidden columns per workbook
  (`/api/v2/workbooks/{id}/views`).
- **Recipes** in the [template gallery](/guides/templates/) are complete,
  validated workbook definitions you can instantiate in one call.
- **Functions** are reusable column chains an admin can publish and anyone can
  apply to a workbook (`/api/functions`).

## Where the data lives

Workbooks, rows, cells and traces live in PostgreSQL, scoped by `workspace_id`
and protected by fail-closed row-level security. Manual edits to a
`lead_field` column write back to the underlying lead. See
[Workspaces and tenancy](/concepts/workspaces/) and the
[architecture](/reference/architecture/) page.
