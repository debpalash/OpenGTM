# Runbook: automations and watches cutover to Go (plan 08)

Go serves 18 routes next to FastAPI on the shared sticky canary bucket (`$auth_upstream`, `infra/nginx/go-split.conf`): 11 under
`/api/automations/` (triggers CRUD, pause, resume, preview, run, runs, `runs/{id}`) and 7 under `/api/watches` (CRUD, `poll`, `signals`).
**No route is retained on Python** (`apps/backend-go/testdata/contract/retained_automation_routes.txt`). Retained at job level: legacy
outreach actions (`sequencer`, `send_email`; hand-back, D2), `re_enrich` over column types Go cannot run (hand-back, D8), and the JobSpy,
DDG and account-group collectors (through `pyworker`, D1). Deviations D4-D7: `apps/backend-go/testdata/contract/README.md`, section
"Plan 08 coverage and rollout (Task 21)".

## Prerequisites

1. Plans 01-07 cutovers in place. The nginx build contains `# --- plan 08 automations ---` in `go-routes.conf`
   (`scripts/nginx-routing-check.sh` passes at 0% and 100%).
2. `AUTOMATIONS_ENABLED` (and `INTENT_POLLER_ENABLED`, `INTENT_POLLER_*`, `AUTOMATIONS_*` limits) **identical on both stacks**.
3. `GO_AUTOMATION_ROUTES_ENABLED=true` on `api-go`. Go registers the automations routes only when it is on **and**
   `AUTOMATIONS_ALLOW_LEGACY_OUTREACH=false` (see guard below); the watch routes also need `PG_LEAD_STORE=true`. With a condition unmet Go logs
   a warning and answers 404 for those paths, and nginx cannot know: **keep the canary at 0% (watch routes stay on Python when
   `PG_LEAD_STORE` is off)**.
4. Go worker: no `TZ` alignment with Python is needed any more: naive timestamps (`older_than`) are read as UTC and schedule/poll fire keys are UTC (`sched:<id>:<UTC +00:00>`, `watch:<id>:<UTC +00:00>`), independent of the Postgres session TimeZone. `push_crm` on the Go worker needs
   `CREDENTIALS_SOURCE=local` (without it the action fails closed with `crm credentials unavailable on this worker`).
5. Webhook secrets: `header_secret_ref` (and a watch's `webhook_secret_ref`) resolves per workspace only and must start with one of `OUTBOUND_SECRET_REF_PREFIXES` (default `WEBHOOK_,AUTOMATION_,DEST_,SLACK_`; set it on api-go **and** the worker, which reads it from its process env). It must not be a registered provider, CRM, SMTP, IMAP or ads credential. **Before cutover rename every existing webhook secret to an allowed prefix (for example `WEBHOOK_<old>`) and update the rules and watches that reference it.** A stored rule whose ref is outside the namespace, missing, empty or global-only now fails the action closed at delivery (`header_secret_ref is not an available webhook secret`; nothing is sent); previously the header was skipped. A key that Python found in global settings or process env must be set in each workspace that uses it, under an allowed name. Webhook urls must have a literal scheme and host (placeholders only in path/query); stored rules with a templated authority fail at delivery. Callers without `automations.manage` see webhook header values as `***`. `AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST`: a bare `example.com` matches only that host; add a leading
   dot (`.example.com`) for subdomains. The allow-list now also applies to destination webhooks and Slack destinations (shared deps): include `hooks.slack.com` if Slack destinations are used.
6. Mirrors and grants (check before the first ramp, repeat after migrations): `scheduled_triggers` and `watch_schedules` are NOT row-level
   secured (the bootstrap reads them without a workspace) and the runtime role holds SELECT, INSERT, UPDATE, DELETE on them and on the
   ledgers:
   ```sql
   SELECT c.relname, c.relrowsecurity,
          bool_and(has_table_privilege('<runtime role>', c.oid, p)) AS grants_ok
     FROM pg_class c, unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE']) p
    WHERE c.relname IN ('scheduled_triggers','watch_schedules','triggers','trigger_runs','trigger_action_results',
                        'trigger_cap_reservations','poll_budget_ledger','watch_subscriptions')
    GROUP BY 1, 2 ORDER BY 1;
   ```
   Expect `relrowsecurity = f` only for the two mirrors and `grants_ok = t` everywhere (`TestRuntimeRoleCanUseMirrorsAndLedgers`).

## Routing

`/api/automations/...` and `/api/watches[/...]` follow the bucket (default 0%); lookalikes (`/api/watchesx`, `/api/automationsx/...`) stay on
Python. `POST /api/automations/triggers/{id}/run` and `POST /api/watches/{id}/poll` follow the bucket too: the job they enqueue is
**executor-routed** (the `executor` column), not path-routed, so the API choice never decides who runs the job.

## Flag order (routes first, executors last)

1. `GO_AUTOMATION_ROUTES_ENABLED=true` on `api-go`, canary 0%. Ramp 5% -> 25% -> 100% (edit `go-split.conf`, `nginx -t`, reload).
2. `GO_EXECUTOR_TRIGGER_EVAL=true` per cohort: also `GO_EXECUTOR_ENABLED=true` and `GO_EXECUTOR_WORKSPACES=<one workspace id>` on the
   enqueuing APIs and the Go worker. Widen one workspace at a time.
3. `GO_EXECUTOR_WATCH_POLL=true`, same cohort rule, last.
Never enable an executor flag before the routes are stable. Restart services after any flag change (config is read at boot).

## The Python scheduler stays the single scheduler (D3)

Go ships `BootstrapSchedules` and `BootstrapWatchSchedules` as libraries only; nothing in Go schedules. The Python scheduler keeps enqueueing
`trigger_eval` and `watch_poll` jobs, and the executor flags decide who consumes them. Verify exactly one active job per `fire_key`
(`uq_jobs_fire_key_active` is the arbiter); this must return zero rows:
```sql
SELECT fire_key, count(*) FROM jobs
 WHERE status IN ('pending','processing') AND fire_key IS NOT NULL AND type IN ('trigger_eval','watch_poll')
 GROUP BY 1 HAVING count(*) > 1;
```
Also check no schedule is silently dropped: every enabled mirror row due in the past has an active job.
```sql
SELECT st.trigger_id FROM scheduled_triggers st
 WHERE st.enabled AND st.next_run_at < now() - interval '10 minutes'
   AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.type='trigger_eval' AND j.status IN ('pending','processing')
                   AND j.payload->>'trigger_id' = st.trigger_id);
```

## Hand-back

The Go worker decides eligibility before the run row exists. A `trigger_eval` whose rule has a legacy outreach action, or `re_enrich` over
agent columns, columns with a `condition`, or an enrichment/waterfall email target without `verify:false`, is returned whole to Python
(`status=pending`, `executor=python`, no retry spent, no write). Metric to watch: the worker log line `trigger_eval handed back to python`
(field `reason`), and the jobs it leaves behind:
```sql
SELECT date_trunc('hour', created_at) h, count(*) FROM jobs
 WHERE type='trigger_eval' AND error LIKE 'handed back to python:%' GROUP BY 1 ORDER BY 1 DESC LIMIT 24;
```
A steady stream is expected for rules with those actions; a jump after a deploy, or hand-backs that never complete on Python
(`status='pending' AND executor='python'` aging), means the Python worker is not consuming. A Python-run and a Go-run never overlap for one `fire_key`.

## Cap and credit reconciliation

```sql
-- reservations still held after a day (a crash after the marker keeps the reservation by design; review, then release by hand)
SELECT workspace_id, trigger_id, idempotency_key, reserved_usd, created_at FROM trigger_cap_reservations
 WHERE state = 'held' AND created_at < now() - interval '1 day';

-- in_flight markers older than a run (non-idempotent actions are never re-sent; a webhook with such a marker is `unknown outcome`)
SELECT workspace_id, trigger_id, run_id, action_type, idempotency_key, updated_at FROM trigger_action_results
 WHERE status = 'in_flight' AND updated_at < now() - interval '1 hour';

-- charged without a marker: MUST BE ZERO (ledger run_id is the action idempotency key; D6 commits both in one transaction)
SELECT l.workspace_id, l.run_id, l.amount_usd FROM credit_ledger_entries l
 WHERE l.reason LIKE 'automation\_%' AND NOT EXISTS
   (SELECT 1 FROM trigger_action_results r WHERE r.idempotency_key = l.run_id AND r.workspace_id = l.workspace_id);
```
Run these as the table owner (RLS applies to the runtime role). A non-empty third query is a stop-the-ramp incident.

## Webhook delivery is at-most-once (D7)

After the `in_flight` marker commits, a crash, retry or lease loss never re-sends the webhook; the next attempt records `skipped`/`replay`.
There is no automatic retry: a receiver that missed a call gets it only from a new run. Idempotent actions (`push_crm`, `re_enrich`) may
re-attempt on the same marker. Egress is SSRF-pinned (loopback, private, link-local, CGNAT, metadata, rebinding and redirects refused,
`skip_reason=blocked_url`).

## Rollback (per flag, reverse order)

1. `GO_EXECUTOR_WATCH_POLL=false`, then `GO_EXECUTOR_TRIGGER_EVAL=false`: new jobs are enqueued with `executor='python'`. Jobs already
   `executor='go'` finish on the Go worker (keep it running until `SELECT count(*) FROM jobs WHERE executor='go' AND status IN
   ('pending','processing') AND type IN ('trigger_eval','watch_poll')` is 0), or flip them back with `UPDATE jobs SET executor='python'
   WHERE status='pending' AND executor='go' AND type IN ('trigger_eval','watch_poll')`.
2. Comment the `go` line of `go-split.conf` out (0%), `nginx -t`, reload. `GO_AUTOMATION_ROUTES_ENABLED=false` last. Data is shared; no migration.

## `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` guard (D2)

Legacy `sequencer`/`send_email` need the Python outreach stack. If `AUTOMATIONS_ALLOW_LEGACY_OUTREACH=true` the Go API **refuses to register**
the automation routes (they answer 404; Python keeps the whole surface) and the Go worker hands such rules back. Keep the canary at 0% in that
configuration. With the flag false, Go rejects legacy actions on create the same way Python does.

## Known behaviours

- Dry run is side-effect free in Go: no webhook, CRM push or enrichment, nothing reserved or charged; it writes only the run record (`dry_run`) and `skipped`/`dry_run` result rows. The Python stack still executes the actions, so keep Python-routed automation runs off until cutover. The earlier advice to keep `GO_AUTOMATION_ROUTES_ENABLED` and `GO_EXECUTOR_TRIGGER_EVAL` off because of dry run no longer applies.
- Free actions now count against `max_actions_per_day` when it is set. Run targets outside `scope_workbook_ids` are dropped and a run with more `row_ids` than `MaxRowsPerEval` is 422. Rule and watch caps are serialised per workspace.
- Deleting a watch leaves pending `watch_poll` jobs; the handler no-ops on a missing or disabled watch.
- Python's boot-time tenancy backfill has no Go equivalent: see `api-cutover.md`.
