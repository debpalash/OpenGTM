# Runbook: governance and research playbooks cutover to Go (plan 11)

> **Historical (plan 15).** OpenGTM now runs without the Python backend: the Python api, worker, scheduler
> and seed services, the root `Dockerfile` and the nginx canary (`go-split.conf`, `go-routes.conf`) are gone, and
> every Go path is on (`GO_ALL_NATIVE=true` in compose). This document records the side-by-side period. Current
> operation: `docs/go-port/runbooks/docker-compose.md`; final cutover: `docs/go-port/runbooks/15-cutover.md`.

Go serves 26 routes next to FastAPI: 17 under `/api/governance` (audit list and `export.csv`, retention policy/preview/enforce/runs, RBAC overrides, members, SSO, SCIM token) and 9 under
`/api/research-playbooks` (CRUD, start/list runs, results, retry, cancel). **Two routes are retained on Python** (`apps/backend-go/testdata/contract/retained_governance_routes.txt`, D1):
`GET /api/governance/capabilities` and `GET /api/research-playbooks/capabilities` (signed certification catalog). Three job types move to Go behind flags:
`retention_enforce`, `research_playbook_run`, `research_playbook_schedule`. Deviations: `apps/backend-go/testdata/contract/README.md`, section "Plan 11 deviations".

## Prerequisites

1. Plans 02 (auth, workspaces, audit middleware), 03 (settings and secrets: the SSO client secret and SCIM token need the master key) and 06 (workbook executor cohort, `PY_WORKER_*`) are cut over.
2. `PG_LEAD_STORE=true` on `api-go`. Routes register only when the group flag **and** `PG_LEAD_STORE` are on; otherwise Go registers nothing, logs a warning, and a request that reaches it
   gets 404 (it does not fall through to Python). nginx cannot see the flags: never raise the canary above 0% until the startup log has no `governance routes not registered` /
   `playbook routes not registered` line.
3. `PY_WORKER_URL` (and `PY_WORKER_TOKEN`) reachable from the Go **worker**: playbook steps call the Python `research_column` provider (decision D2). Without it a claimed run job is handed back to
   Python (one startup warning). The master key (secret envelope) must be configured on `api-go`, or `PUT /api/governance/sso` answers 503 `Secrets are not configured` (deviation).
4. **The Python scheduler keeps running** (decision D7): its `retention` and `playbooks` bootstraps enqueue the daily/periodic jobs; only the job's `executor` column changes with the flags.
   The Python **reaper** keeps running too: Go has no reaper, and Python's reaper reconciles dead Go leases (it picks the failure handler by job type, not executor).
5. Python replicas run the same release as the fixtures were recorded from. The cohort uses `GO_EXECUTOR_ENABLED=true` and `GO_EXECUTOR_WORKSPACES=<ids>` on **both** stacks (the enqueuing stack decides the executor).
6. nginx build contains `# --- governance-playbooks (plan 11) ---` in `go-routes.conf`, and `scripts/nginx-routing-check.sh` passes at 0% and 100%. Both knobs in `go-split.conf`
   (`$go_bucket`, `$go_ws_bucket`) carry the **same** percentage.
7. Restart `api-go` and the worker after any flag change (config is read at boot).

## Flag order

Flip one at a time, in this order, watching between steps. All default false.

1. `GO_GOVERNANCE_ROUTES_ENABLED=true` on `api-go`: audit, retention, RBAC, members, SSO, SCIM token routes. Retention enforce still enqueues `executor=python` jobs (the cohort is off). Behaviour differences from Python: `POST /retention/enforce` answers 409 `Retention enforcement already running` while a pending or running run exists (per-workspace advisory lock); a delegated non-admin manager cannot remove a member who carries a `deny` override (403 `Cannot grant or manage access above your own`); a final-failure reconcile of an already `failed` playbook run is a no-op. For SSO see `api-cutover.md` (`SSO_ALLOWED_ISSUERS` must be set first).
2. `GO_PLAYBOOK_ROUTES_ENABLED=true`: playbook routes. Runs still enqueue `executor=python`.
3. `GO_EXECUTOR_RETENTION=true` (api-go **and** worker, and the Python replicas that run the scheduler) with `GO_EXECUTOR_ENABLED=true` and a first canary workspace in `GO_EXECUTOR_WORKSPACES`:
   retention jobs of that workspace are executed by Go in 5000-row batches (`RETENTION_BATCH_ROWS`). Do the dry-run (below) first.
4. `GO_EXECUTOR_PLAYBOOKS=true` likewise: `research_playbook_run` and `research_playbook_schedule` of cohort workspaces are executed by Go; the Go worker calls Python per step over `PY_WORKER_URL`.
   A run job claimed without a worker is handed back, not failed.

Executor choice per workspace: Go claims a job only when `GO_EXECUTOR_ENABLED`, the per-type flag and the workspace cohort all say so. An empty workspace id is never in the cohort.

## Routing

- `GET /api/governance/capabilities` and `GET /api/research-playbooks/capabilities`: always `api_backend` (placed first in the section).
- The other 26 routes follow the canary bucket. `POST /api/governance/retention/enforce` and `POST /api/research-playbooks/{id}/runs` are **sticky per workspace** (`$go_ws_bucket`, key
  `X-Workspace-Id`, else the client key), so the run row and its job are never split across stacks during the canary.
- `GET /api/governance/audit-events/export.csv`: `proxy_buffering off`, `proxy_read_timeout 300s` (streamed, keyset-batched export).
- Unknown siblings, lookalikes and trailing-slash variants stay on Python at every percentage. Responses carry `X-Upstream: go|py`.

## Retention dry-run (before the first Go enforce)

1. Pick the canary workspace; keep `GO_EXECUTOR_RETENTION` off and the policy enabled with a known `retention_days`.
2. Call `POST /api/governance/retention/preview` through the gateway with the same body twice, once forced to each stack (`X-Upstream` check, or hit `api` and `api-go` directly).
   `expired_counts` must be **equal** for all seven categories (`audit`, `llm_usage`, `signals`, `activation`, `audience_history`, `agent_results`, `outreach_history`; eight tables, `activation` covers two). Cutoffs are strict `<` on each table's age column.
3. Only then turn `GO_EXECUTOR_RETENTION` on. Trigger one `POST /retention/enforce` in a **test workspace** with minimum days and verify the run reaches `completed`, `deleted_counts` equal the
   earlier preview, and the audit trail holds the policy event. Mid-run a legal hold stops the run at the next batch; a failed run stores the counts deleted so far.
   Mid-run, disabling the policy also stops a scheduler-requested run at the next batch.
3b. **Scheduled occurrence check (required):** on the canary, set the policy's `next_run_at` and its pending `retention_enforce` job's `next_run_at` to now and confirm the worker completes it
   (a run with `requested_by=scheduler`, status `completed`, and the next day's job enqueued). Jobs written by the Python scheduler have no `jobs.workspace_id`; Go admits them by their exact
   `fire_key` and the policy lookup. Do the same for a scheduled playbook (`research_playbook_schedule`). A job that fails with a payload/fire-key error here must be fixed before widening the cohort.
4. Raise the cohort workspace by workspace. Preview and enforce never touch other workspaces (RLS plus explicit predicates).

## What to watch

| signal | where | investigate when |
|---|---|---|
| retention runs stuck `running` older than the 1800 s job limit | `retention_runs`, queue | the Python reaper reconciles dead leases; a run that stays `running` after that needs a look |
| `retention_runs` `failed` with partial counts | `GET /retention/runs` | Go stores counts deleted so far; re-enforce is idempotent |
| playbook runs `pending` with `executor=go` and no progress | `playbook_runs`, `jobs` | worker down, `PY_WORKER_URL` unreachable (run jobs are handed back and retried) |
| `research_playbook_run` jobs handed back | worker log `playbook run jobs are handed back to Python` | `PY_WORKER_URL` missing on the worker |
| 503 on `PUT /api/governance/sso` | nginx and Go | no master key configured (by design) |
| 403 `Cannot grant or manage access above your own` | Go | expected for delegated managers (D-rbac-1); an owner/admin getting it means a deny override on them |

## Rollback

1. Set the canary to 0% (comment the `go` lines in `go-split.conf`, `nginx -t`, reload). **No data migration**: both stacks use the same tables. Python serves new requests.
2. Turn the flags off in reverse order (`GO_EXECUTOR_PLAYBOOKS`, `GO_EXECUTOR_RETENTION`, then the route flags) and restart. **In-flight Go jobs** finish on the worker, or are reconciled:
   a `go` row that reaches a worker whose flag is off is **handed back to Python untouched** (never failed), so no drain is needed before turning a flag off; a Go-claimed job whose lease dies is
   reconciled by **Python's reaper** (failure handler by job type). An interrupted retention run keeps the counts deleted so far; the next scheduled enforce continues (deletes are idempotent; scheduled jobs with no job workspace are handled by both stacks).
   An interrupted playbook run is restored (`pending`) or retried by the queue; a run cancelled while a member was running is never overwritten by Go.
3. If route flags are turned off while the canary is above 0%, those requests get 404: lower the canary first.

## Deviations to expect (full list in the README)

- D3 PATCH hardening: `null` on non-nullable playbook fields is 422 and a rename collision is 409 (Python 500); list capped at 1000. D4 batched retention purge (hold and run status re-read per batch,
  cutoffs fixed once). D5 run row and job inserted in one transaction (no orphan `pending` run). D6 member removal also removes SCIM group memberships.
- Playbook budget is **per cell per attempt** (`cell_budget_usd / steps`, enforced inside Python `research_column`); there is no cumulative run cap (Python parity).
- Concurrent `POST /retention/enforce` creates two runs, as Python (D8).
- RBAC escalation hardening (D-rbac-1): delegated managers cannot grant above their own role or permissions, lift denies on themselves, or touch the owner; an actor with a deny gets 403 where Python gives 409/404.
- `PUT /sso`: client secret written before the config row; 503 without a master key. Audit export: Python metadata key order and CSV formula guard (leading `= + - @ TAB CR`).
- Not fixed, outside this plan (final review): SCIM `removeMember` locks overrides before the member row (deadlock vs `PUT /rbac`), and SCIM deprovision has no owner guard.

## Verification commands

```sh
cd apps/backend-go && make vet test test-integration            # needs TEST_DATABASE_URL (see Makefile)
go test -race -run 'TestEveryGovernance|TestRetained|TestRetentionTargets|TestPlan11' ./internal/api
scripts/nginx-routing-check.sh                                    # 28 paths: capabilities always py, 26 follow the bucket at 0% and 100%
```
