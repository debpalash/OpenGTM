# Runbook: connectors, HubSpot, /api/data and AmbitionBox cutover to Go (plan 07)

Go serves 19 routes next to FastAPI with the same percentage split as plans 02-06 (`$auth_upstream`,
`infra/nginx/go-split.conf`): `GET /api/connectors/catalog|compatibility` (2), `/api/crm/hubspot/status|connect|sync|contacts` (4),
the 7 `/api/data` routes (list, export, stats/timeline, stats/domains, mark-used, PATCH and DELETE `{id}`) and the 6 `/api/ambitionbox`
routes (companies, jobs, detail, similar, collect, `POST /imports`). **No route is retained on Python**
(`testdata/contract/retained_connector_routes.txt` lists only non-route leftovers). `TestEveryPythonConnectorRouteIsPortedOrRetained`
fails when a Python decorator is in neither list. `/admin` and `/scim` stay unexposed, as before.

## Prerequisites (do not skip)

1. Plans 01-06 cutovers are in place (`api-go`, `go-split.conf`, `go-routes.conf` behind nginx). The nginx build contains the plan 07
   section of `go-routes.conf` (`# --- plan 07 connectors-crm ---`).
2. `PG_LEAD_STORE=true` and `GO_CONNECTOR_ROUTES_ENABLED=true` on `api-go`. With either off Go registers none of the 19 routes, logs a
   warning (`connector routes not registered`) and every ported path answers 404 from Go. Never raise the canary in that state.
3. Connector catalog configuration, **both must be set** on `api-go` (the defaults are cwd-relative and silently produce an empty catalog
   when the working directory differs):
   - `OPENGTM_CONNECTOR_MANIFESTS_DIR`: the declarative manifest directory the Python API serves.
   - `OPENGTM_CONNECTOR_TRUST_STORE`: the trusted-publishers JSON (`docs/connectors/trusted-publishers.json` in the repo).
   The resolved paths and the effective policy are logged at startup (`connector catalog sources`); check that log line after every deploy.
4. `CONNECTOR_SIGNATURE_POLICY` semantics (same as Python, `connectors.EffectivePolicy`): unset means `optional`; **set but empty means
   `required`** (fail closed); `required` rejects unsigned or untrusted manifests in `/compatibility` and shows them as not trusted in
   `/catalog`. Keep the value identical on `api` and `api-go`, otherwise the two stacks answer different catalogs.
5. Certification: `CERTIFICATION_KEY` (and path/build SHA as for Python) set on `api-go`; the key is read per request and never logged.
   A tampered manifest or an expired or forged certification never renders as `trusted` or `certified`.
6. Redis and the Go worker (`cmd/worker`) for AmbitionBox imports (see below). `executor='python'` jobs keep being consumed by the Python worker.
7. Runtime role grants (check before the first ramp, repeat after any migration that touches them): the role `api-go` connects as needs
   `SELECT, INSERT, UPDATE, DELETE` on the legacy global tables `email_data` and `links`, and `USAGE` on their id sequences:
   ```sql
   SELECT t, p, has_table_privilege('<runtime role>', 'public.'||t, p)
     FROM unnest(ARRAY['email_data','links']) t, unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE']) p;
   SELECT has_sequence_privilege('<runtime role>', pg_get_serial_sequence('public.'||t, 'id'), 'USAGE')
     FROM unnest(ARRAY['email_data','links']) t;
   ```
   All must be `t`. `TestRuntimeRoleCanReadWriteEmailData` (integration) asserts the same against the test role.

## Routing summary

| Path | Upstream |
| --- | --- |
| The 19 routes above | Go for the canary percentage, else Python |
| Everything else under `/api/connectors/`, `/api/crm/`, `/api/data`, `/api/ambitionbox/` (lookalikes `/api/crm/other`, `/api/data-x`, `/api/dataset`, unknown subpaths, trailing-slash variants such as `/api/data/`) | always Python |

The plan 07 locations are per-route regexes (not prefixes), so `/api/crm/hubspot/` cannot capture `/api/crm/other` and a path Go does not
serve is never sent to Go. Trailing-slash variants stay on Python on purpose (FastAPI redirects them). Ported paths carry `X-Upstream: go|py`.
`scripts/nginx-routing-check.sh` asserts every listed route at 0% (`py`) and 100% (`go`) and every lookalike as `py` at both.

## Flag order

1. Deploy `api-go` with `GO_CONNECTOR_ROUTES_ENABLED=true`, both `OPENGTM_CONNECTOR_*` paths set, canary at 0%. Verify the startup log line.
2. Ramp the canary as in the earlier runbooks (5%, 25%, 100%; edit the `go` line of `go-split.conf`, `nginx -t`, reload). Sticky per client.
3. **Only then** enable the AmbitionBox import executor, per cohort: `GO_EXECUTOR_ENABLED=true`, `GO_EXECUTOR_AMBITIONBOX=true` and
   `GO_EXECUTOR_WORKSPACES=<one cohort workspace id>` on both the API that enqueues (`api`/`api-go`) and the Go worker. `ExecutorFor` answers
   `go` only when all three hold for the workspace; otherwise `python`. Widen the workspace list one workspace at a time.
   `POST /imports` is sticky to the cohort so one run is started and resumed by one executor family.
4. AmbitionBox egress: production clients are built with `ambitionbox.NewEgress` and a **nil allow-list** (SSRF-safe dialer, fixed base
   host, redirects refused). Do not pass an allow-list in production; it exists for tests with a loopback simulator only.

## Executors, the reaper and resuming a run

- Python's reaper recovers dead jobs (lease expiry) for **any** executor and calls the **Python** failure handler
  (`handle_ambitionbox_import` failure reconciliation). Go runs its own failure hook (`queue.RegisterFailureHook`) after a persisted failed
  outcome; lease-expiry recovery stays with the Python reaper. A Go-executed run that dies hard is therefore reconciled by Python code
  operating on the same `connector_runs` row, which is intended (same columns, same cursor JSON).
- Resuming across executors: a run commits each page and its cursor in one transaction, so either executor can pick up a `retrying` run at
  the stored `next_page`. Two workers never advance the same page (`next_page` re-check), never double-insert (`uq_workbook_row_source_identity`
  plus the `source_ingest` advisory lock), and `uq_connector_runs_active` still yields 409 `connector_run_active`. A hand-back or lease loss
  leaves the run `retrying`, never `running` forever. To move a workspace out of the Go cohort mid-run, remove it from
  `GO_EXECUTOR_WORKSPACES`; the next lease of the run is taken by the Python worker.

## HubSpot and Salesforce token storage

- Per-workspace tokens live in the encrypted workspace secret store (`secrets` envelope; `PUT /api/settings/workspace-integrations/hubspot`
  or `POST /api/crm/hubspot/connect`). The Python global fallback (global settings, then the process environment, `HUBSPOT_TOKEN`,
  `SALESFORCE_*`) is kept for single-tenant installs and is visible in tests; a token of workspace A never serves workspace B.
- **Fail-closed lookup (deviation from Python):** a secret-store or decryption error means "not connected": no vendor request is sent and
  there is no environment fallback on that error. Python falls back to the environment on any error. Only a clean "not found" falls through.
- `hubspot/sync` reads leads only through the caller's workspace; foreign or unknown lead ids are dropped silently (200, counts own leads).
- Salesforce has no route in this plan; the client (library for plan 08 `push_crm` and the destinations plan) validates the user-typed
  `SALESFORCE_INSTANCE_URL` on every call (https, Salesforce suffixes, public address, dial-time check, no redirects; D3).

## Canary ramp and per-route rollback

- Ramp: as in `leads-signals-cutover.md` (single `go` line in `go-split.conf`, `nginx -t`, reload).
- **Rollback, whole plan:** comment the `go` line out (0%), reload. No data migration to undo: Go and Python write the same tables.
- **Rollback, one route group:** delete (or point at `api_backend` with `proxy_pass http://api_backend;`) the matching location in the plan 07
  section of `go-routes.conf`; the other groups keep splitting. Or set `GO_CONNECTOR_ROUTES_ENABLED=false` on `api-go` and keep the canary
  at 0% (all four groups at once; Go answers 404 afterwards, so do this only at 0%).
- **AmbitionBox executor rollback:** remove the workspace from `GO_EXECUTOR_WORKSPACES` (or set `GO_EXECUTOR_AMBITIONBOX=false`). Runs in
  flight finish or resume on Python.

## Verification

- Before any ramp: `scripts/nginx-routing-check.sh` (ported routes `py` at 0%, `go` at 100%, lookalikes `py` at both).
- `cd apps/backend-go && make vet test test-integration` (integration needs `TEST_DATABASE_URL`, `TEST_REDIS_URL`).
- Known red until plan 06 Task 23: `TestMetaRoutesMatchFixtures` (the `provider_stats` fixture); it is owned by plan 06 and is not a plan 07 regression.
- At each percentage: `curl -si -H "Authorization: Bearer $T" https://<host>/api/connectors/catalog | grep -i x-upstream`; compare bodies between
  stacks for the same token with `jq -S`.
- **No token appears in logs.** After exercising `POST /api/crm/hubspot/connect` with a canary token value and a failing
  `hubspot/sync`, search every sink (api-go stdout, worker stdout, nginx access and error logs, Postgres statement log):
  `docker compose logs api-go worker-go nginx | grep -c "<the token value>"` must print 0 (also grep the encoded and truncated forms).
  Vendor error text is scrubbed of the token before it reaches an error string, a run receipt or a log line.
- Run state: `SELECT status, cursor FROM connector_runs WHERE kind='ambitionbox' ORDER BY created_at DESC LIMIT 5` for a cohort import;
  a `running` row with an expired lease is the Python reaper's to recover.

## Caveats

- **Legacy global tables.** `email_data` and `links` have no `workspace_id` and no RLS (D5). The 7 `/api/data` routes are reachable only by a
  global admin (`403 Not authorized` otherwise, applied by the route group; `TestEveryDataRouteRequiresGlobalAdmin`). `mark-used` with no ids
  and no filters updates **every** row, and `DELETE` of a missing id answers 200 `{"status":"not found"}`, both as Python. Making the table
  tenant scoped is a schema and product change outside this plan.
- Response `/compatibility` error `path` fields carry the served directory as configured, so they differ between stacks if the two use
  different `OPENGTM_CONNECTOR_MANIFESTS_DIR` values.
- `crm_import` source materialisation, output-column execution and the destinations consumers stay on Python (`CRM_IMPORT_ENABLED`); nothing to cut over.

## Deliberate deviations from FastAPI

Contract parity is the release bar; the exceptions are listed per route in `apps/backend-go/testdata/contract/README.md`
("Plan 07 coverage and rollout (Task 19)" indexes them). Summary: D3 Salesforce SSRF guard; D4 crash-class inputs answer 4xx/502 instead of 500
(catalog bad manifest name 422, non-column `sort_by` falls back to `id`, negative `limit`/`offset` 422, AmbitionBox `location`/`company_type` 422,
bad import preconditions 404/409, upstream failure 502, `hubspot/sync` `limit` bounded 1..1000); D5 `/api/data` stays global; plus bounded
bodies (1 MiB), refused redirects, naive certification timestamps read as UTC, and the fail-closed token lookup above.
- Post-port fix wave: a connector manifest can read only `auth.env_var` plus the optional `auth.extra_env` list, and platform keys (`DATABASE_URL*`, `STRIPE_*`, `*_MASTER_KEY`, `SECRET_KEY`, `INTERNAL_SERVICE_TOKEN`, `REDIS_URL`, `SMTP_PASSWORD`, `IMAP_PASSWORD`) always render empty (`missing ENV`); a manifest that relied on another env var must declare it in `extra_env` (and be re-signed). `GET /api/data` `limit` is capped at 1000 (422 `less_than_equal`) and `q` wildcards (`%`, `_`) match literally.
