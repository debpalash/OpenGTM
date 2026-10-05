# Job type triage for the Go port

Companion to the [hybrid platform rewrite RFC](hybrid-platform-rewrite.md).
It records, for every job type registered in
[`apps/api/services/job_registry.py`](../../apps/api/services/job_registry.py),
whether it has been ported to a Go executor, and if not, exactly what stands in
the way. Cutover and rollback steps for the ported types are in
[`apps/server/README.md`](../../apps/server/README.md#job-types-running-in-go).

## How a type was judged

A type is ported only when all of these hold:

1. **Independent.** Its handler needs only PostgreSQL and code that already
   exists in Go. It does not call a Python-only AI, browser or lead-generation
   engine, and it does not read state that lives outside PostgreSQL.
2. **Parity-testable.** The same dataset can be loaded into two databases, the
   unchanged Python handler run on one and the Go handler on the other, and the
   resulting state compared row for row. Anything with an external side effect
   (HTTP, SMTP, IMAP) would additionally need a local simulator and a policy for
   the places where the Go and Python network stacks legitimately differ.
3. **Proven.** The parity test covers the happy path, every branch the handler
   has, the failure reconciler, malformed stored data, lease loss and
   cancellation (the attempt writes nothing), and tenant isolation (a second
   tenant is left untouched), across the time-zone combinations that change how
   timestamps are stored. A type without that proof is not registered at all.

A ported type stays **Python-owned by default**: no route is seeded, and an
operator moves it with `opengtm routes set <type> go` and back with
`... python`.

## Result

| Job type | Status | Why |
| --- | --- | --- |
| `retention_enforce` | **Ported** (M2) | PostgreSQL only. |
| `research_playbook_schedule` | **Ported** | PostgreSQL only: creates a pending run and its `research_playbook_run` job, books the next occurrence. |
| `audience_refresh` | **Ported** | PostgreSQL only (the tenant's `leads` table, `audience_members`, events, destination runs, `trigger_eval` jobs). One check is not reproduced: see [the workspace metadata note](#workspace-metadata-and-secrets-local-sqlite). |
| `audience_destination_sync` | Not ported | See [below](#audience_destination_sync). |
| `send` | Blocked on M8 | SMTP credentials, the mandatory footer and the unsubscribe-token key are per-workspace secrets in the local SQLite store. |
| `outreach_inbound_poll` | Blocked on M8 | IMAP credentials come from the same secret store; the IMAP client is Python. |
| `signal_scan` | Out of scope | Runs `run_signal_scan`: hiring-signal detection over live HTTP sources and score boosts, over workspaces listed from the SQLite metadata. Its small DB-only tail (refresh triggers) cannot be split from it. |
| `trigger_eval` | Out of scope | Evaluates rules with a bespoke safe-expression evaluator and runs actions that call the AI enrichment engine (`re_enrich`), CRM connectors (`push_crm`) and billing/caps. |
| `watch_poll` | Out of scope | Intent poller: SEC filings, JobSpy and RSS sources, job-change and account-group engines, billing, the lead store; workspace slug from the SQLite metadata. |
| `source_health_check` | Out of scope | Probes about 90 `site:` sources through the live search engine (`_ddg_search`, the source registry). Platform-global and off by default (`SOURCE_HEALTH_ENABLED`). |
| `run_workbook` | Out of scope | AI/provider enrichment engine (specialist worker, M2 enrichment). |
| `source_workbook` | Out of scope | Runs a multi-strategy lead-generation collection inline. |
| `collect` | Out of scope | The lead-generation job runner (search, scraping, browser tiers). |
| `bulk_enrich` | Out of scope | Lead-generation enrichment waterfall. |
| `refresh_workbook` | Out of scope | Re-sources and re-enriches through `materialize_source` and `run_workbook_enrichment`. |
| `ambitionbox_import` | Out of scope | Site-specific importer with API retries and a checkpoint ledger in the collection store. |
| `research_playbook_run` | Out of scope | Executes AI research steps per audience member. |

## Findings behind the blocked and out-of-scope types

### Workspace metadata and secrets: local SQLite

`apps/api/services/workspace/manager.py` keeps workspaces, slugs and the
`workspace_settings` table, which holds every per-workspace secret (SMTP, IMAP,
CRM tokens, webhook secrets), in `data/workspaces.db`, a SQLite file beside the
Python API. The RFC already lists this as an M8 prerequisite ("workspace
metadata, secrets, and collection ledgers moved to PostgreSQL"). Until it is
done, a Go executor cannot read a tenant's credentials, so `send`,
`outreach_inbound_poll`, the credentialed destination types and the signal
sources cannot reach parity; reading the SQLite file from Go would couple the
new stack to the part being retired and still not work on a different host.

The same store answers `workspace_slug()`, which `handle_audience_refresh`
calls to skip a workspace that no longer exists. The Go port does not consult
it (it cannot), so it refreshes an audience whose workspace row has vanished
from the metadata file but whose PostgreSQL rows remain. Nothing else in the
refresh uses the slug when the PostgreSQL lead store is in use.

### `audience_destination_sync`

Twelve destination types share one handler: `webhook`, `hubspot`, `salesforce`,
`instantly`, `smartlead`, `google_sheets`, `airtable`, `slack`, the three paid
media types (`meta_ads`, `google_ads`, `linkedin_ads`) and `warehouse_http`.
Porting it was considered with a local HTTP simulator and rejected because:

- Only `webhook` is plain HTTP. Every other type goes through a Python
  integration adapter and per-workspace secrets (previous section); the ad types
  also need OAuth and identifier hashing.
- Even `webhook` is not reproducible with parity. The URL, headers and body are
  rendered by the AI-column template resolver (`workbook/output._resolve`,
  strict placeholder validation), the optional `Authorization` header is a
  workspace secret, and the request goes through Python's DNS-pinning SSRF
  guard. The Go egress guard is an intentionally stricter reimplementation
  (it also blocks CGNAT and applies robots.txt and per-domain rate limits to
  plugin fetches), so the two stacks disagree on which destinations are
  reachable.
- A job type is routed as a whole. A webhook-only Go executor would break the
  first workspace that has a HubSpot destination, and there is no per-payload
  routing to keep those on Python.

The path forward is M8 (secrets in PostgreSQL), then one adapter at a time as a
declarative connector or plugin, with the sync loop and its ledgers (which are
plain SQL) moved last.

### Handler exceptions are recorded differently by the two queues

Python runs every handler in a child process; the parent records
`job <id> (<type>) child exited with code 1` as the attempt error and passes
that text to the failure reconciler. The Go queue records the handler's own
error text. Domain columns written by a handler itself
(`audiences.last_refresh_error`, `retention_runs.error`) therefore match; the
`jobs.error` string and the `reason` handed to a reconciler do not. The child
also rejects a payload that is not a JSON object before the handler runs, so
such a job fails in Python without any handler text at all.

### Other facts the ports rely on

- Column defaults of many tables exist only in the Python ORM
  (`attempted`/`succeeded`/`failed` of `playbook_runs`, for example). A Go
  insert must supply them; the parity suites catch the omission.
- `jobs` and the three schedule mirrors (`audience_schedules`,
  `playbook_schedules`, `retention_schedules`) carry no row-level security, and
  a scheduler deletes and cancels by identifiers taken from the job payload.
  The Python cleanup is not scoped to the job's workspace; the Go executors
  scope it, which only matters for a payload that names another tenant's
  identifier.
- `AUTOMATIONS_ENABLED` gates every `trigger_eval` enqueue in Python. The Go
  server reads the same variable (`config.Automations.Enabled`); set it
  identically on both stacks.

## Re-run the proofs

```bash
TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
  uv run pytest tests/test_retention_go_parity_pg.py \
                tests/test_playbook_schedule_go_parity_pg.py \
                tests/test_audience_refresh_go_parity_pg.py
```

Each suite is built on [`tests/parity_harness`](../../tests/parity_harness) and
[`internal/jobs/jobkit/paritytest`](../../apps/server/internal/jobs/jobkit/paritytest);
adding a type means adding a dataset, a scenario file, a Python runner and a
Go driver, not a new harness.
