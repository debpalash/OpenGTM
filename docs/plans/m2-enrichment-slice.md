# M2 slice: connector enrichment runs in Go

Status: implemented on `rewrite/m2-enrichment`. This note records the scoping
decision for milestone M2 of the
[hybrid platform RFC](hybrid-platform-rewrite.md) ("one HTTP enrichment job type
in Go, including manifest v1 connectors, result/evidence persistence, billing
coordination, and progress").

## What the Python path does

`POST /api/workbooks/{id}/run` enqueues one `run_workbook` job.
`handle_run_workbook` (`apps/api/services/workbook/enrichment.py`) loads the
workbook, orders its columns, runs rows concurrently (columns sequentially per
row) and writes one cell at a time. A cell is one of eight column types: a
provider waterfall (`enrichment`, `waterfall`), an LLM prompt (`ai_formula`,
`research`, `agent`), an arbitrary HTTP call (`http`), a formula, or an
`output` push to a CRM or webhook. A waterfall cell walks a provider chain,
where a provider is one of about 40 Python classes or a declarative manifest v1
connector, and then:

- checks provider cooldowns and the workbook budget (`planner.filter_chain`);
- for a paid provider, reserves catalog exposure, dispatches under the queue
  lease, and settles the outcome in `workbook_spend_attempts`, which also
  advances `workbooks.budget_spent_usd` (`spend_service`, `provider_accounting`);
- writes the value to `workbook_enrichments` and to the
  `workbook_rows.enrichments` JSON mirror, plus a Redis broadcast;
- writes extra result fields back to the linked lead record (per-workspace
  SQLite or the PostgreSQL `leads` table), optionally with per-fact provenance;
- records provider telemetry in `provider_stats` after the result commits.

Credits (`workspace_credits`, `credit_ledger_entries`) are debited once, in the
route, before the job exists (`check_and_debit`, idempotent per `run_id`). The
job never debits credits; its billing work is the spend-attempt ledger and
`budget_spent_usd`.

## Decision: what is the smallest shippable slice

Moving `run_workbook` as a whole is not shippable: most column types call an
LLM, drive a browser, run Python-only providers or push to third parties, and a
job type is routed to exactly one executor. The slice is therefore a **new job
type, `run_workbook_connector`**, that covers the part which is genuinely HTTP
plus accounting and nothing else:

| In scope | Out of scope (stays `run_workbook`, Python) |
| --- | --- |
| `enrichment` / `waterfall` columns with an **explicit** provider or waterfall (the user chose the chain) | Default waterfalls (they mix in Python-only providers) |
| every provider in the chain is a **manifest v1 connector** (`DeclarativeProvider`) | Python provider classes, WASM, scrapers |
| v2 workbook rows with **no linked lead** (`lead_id IS NULL`) | Lead write-back and per-fact provenance on linked rows (the lead store is per-workspace SQLite or another PostgreSQL table) |
| no `condition`, no `{column}` references in the column config | Dependency ordering, conditions |
| email targets only with `verify: false` | The SMTP verify cascade |
| `AUTOMATIONS_ENABLED` and `PROVENANCE_TRACKING_ENABLED` off | `on_row_changed` emits, provenance JSON |

Why a new type instead of routing `run_workbook`: rollback must be
`opengtm routes set <type> python` with Python-only operation intact. A routed
`run_workbook` would need every ineligible payload to be refused or handed back,
and a mixed fleet cannot do that safely. With a new type, the Python API decides
at enqueue time whether a run is eligible and which route is active; the Python
worker keeps a handler for the type (the unchanged `handle_run_workbook`), so a
rollback, or a pending job that outlives a rollback, simply runs in Python.

## Switch and rollback

`job_executor_routes` is the only switch:

- no row or `python` (the default): `/run` enqueues `run_workbook`, exactly as
  before. Nothing in the Python path changes behaviour.
- `go`: `/run` enqueues `run_workbook_connector` for eligible runs (still
  `run_workbook` otherwise), and only Go claims the new type.
- roll back with `opengtm routes set run_workbook_connector python`: new runs go
  back to `run_workbook`; jobs of the new type that are still pending are claimed
  by Python and handled by the unchanged handler.

## Go design

Package `internal/jobs/enrich`:

- one tenant transaction per state change under forced RLS, each fenced with
  `queue.HoldLease`; provider calls happen outside any transaction;
- rows run on a bounded worker pool (`concurrency` from the payload, capped),
  columns sequentially per row threading results into the row like Python;
- providers run through `declarative.RunProvider` and the shared guarded
  egress client (SSRF guard, DNS pinning, response size cap, per-domain
  limiter), with the payload's `provider_timeout` as a hard per-call deadline;
- the spend ledger is a port of `spend_service` with identical state
  transitions, contract hashes and attempt keys, so a job started by one
  executor can be finished by the other;
- the final write of a cell (the last paid attempt's settlement and the budget
  increment, the `workbook_enrichments` upsert, the row mirror and the evidence)
  is one transaction under the lease. An attempt whose provider already answered
  is settled even if the lease was lost, so a paid answer is never forgotten;
- progress is published on `LISTEN/NOTIFY` (`internal/progress`) and
  `workbooks.completed_rows` follows the Python semantics;
- cancellation (queue lease loss or `workbooks.status = 'paused'`) stops
  admission of new provider calls; retry passes, the final status, and the
  `execution_result` receipt match Python.

Known gaps are listed in [`apps/server/README.md`](../../apps/server/README.md#connector-enrichment-runs-in-go).

## Outcome and deviations from this plan

- The gate in `connector_run.py` is stricter than the table above: it also
  requires https connectors, no `${env:X}` other than the connector's own key,
  and that the key resolves identically in the API process, because the Go host
  grants a v1 connector only its endpoint origin and its declared secret.
- The lease is proven with a shared row lock (`FOR SHARE`), not
  `queue.HoldLease`'s `FOR UPDATE`: concurrent cell commits queued behind each
  other on the job row and capped a run near 250 cells/s; a shared lock keeps
  the fencing (cancel, finalize and reclaim UPDATE the row and wait) and removed
  the ceiling.
- Writing the parity harness exposed a Python defect: declarative providers left
  `EnrichmentResult.provider` empty, so `accounting_envelope` rejected every paid
  connector response and every paid attempt ended `uncertain`. It is fixed in
  its own commit (`provider_runner.py`, one line, with a test); Go assumes it.
- Progress goes to `LISTEN/NOTIFY`, not to the Redis channel the current grid
  listens on, so live cell updates for Go-run workbooks wait for the web client
  to subscribe to `/api/v2/events`.
