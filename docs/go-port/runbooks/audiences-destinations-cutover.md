# Runbook: audiences and destinations cutover to Go (plan 10)

Go serves 23 routes next to FastAPI: 8 under `/api/audiences` (list, create, patch, delete, refresh, `members`, `accounts`, `events`) and 15 under `/api/audience-destinations` (list, create, patch, delete, `sync`, `runs`, `health`, run `deliveries`/`retry`/`cancel`, the three `inbound-token` routes, `inbound-receipts`, and the public CRM callback `inbound/{id}`).
**Two routes are retained on Python** (D1, `apps/backend-go/testdata/contract/retained_audience_routes.txt`): `GET /api/audiences/capabilities` and `GET /api/audience-destinations/types` (the signed-certification catalogs shared with integrations and governance). Go also claims two job types behind flags: `audience_refresh` and `audience_destination_sync`.
Deviations: `apps/backend-go/testdata/contract/README.md`, section "Plan 10 (audiences and destinations ...)".

## Prerequisites

1. Alembic revision `d3f5a7b9c1e2` (widens `destination_runs.status` to `VARCHAR(32)`; `completed_with_errors` is 21 characters) is applied to the database both stacks use, and the Go schema check passes. Without it a run with a failed delivery cannot be stored on PostgreSQL, from either stack.
2. `PG_LEAD_STORE=true` on `api-go` and the worker. The audience routes register only with `GO_AUDIENCE_ROUTES_ENABLED` **and** `PG_LEAD_STORE` **and** the plan 08 membership-event emitter (`AudienceEvents`, wired with the automation hook); without the emitter Go never persists a membership event it cannot emit, logs `audience routes not registered: membership event emitter not wired`, and the audience routes stay off (a request that reaches Go gets 404). The destination routes need `GO_DEST_ROUTES_ENABLED` and `PG_LEAD_STORE`. nginx cannot see the flags: never raise the canary above 0% until the startup log has no `routes not registered` line.
3. Plan 08 (automations: `AudienceEvents`, the webhook action) is cut over or at least wired in the worker. Plan 07/08 ports for each type you list in `GO_DEST_TYPES`: **`warehouse_http`, `webhook`, `slack` and the three ad types (`meta_ads`, `google_ads`, `linkedin_ads`) have Go delivery** (a webhook with a header ref needs `CREDENTIALS_SOURCE=local` on the worker, otherwise it fails loudly). `hubspot`, `salesforce`, `instantly`, `smartlead`, `google_sheets` and `airtable` are handed back to Python even when listed, until their port is wired; a type that has no port wired logs a warning at worker start and its rows are handed back to Python with no retry spent (the worker does not refuse to start). The listed types must be consistent on Python and the Go worker.
4. `SECRETS_MASTER_KEY` and `SECRET_KEY` are identical on both stacks and the worker. Without a master key a `warehouse_http`/`slack` create or config patch answers 503 `Secrets are not configured`.
5. **Destination secrets are workspace secrets only.** Go resolves `header_secret_ref`, `webhook_secret_ref` from the destination's own workspace, never from a global secret or the process environment (Python fell back to both, which let an editor read deployment keys through a warehouse header or Slack URL). Only `header_secret_ref` and `webhook_secret_ref` work this way: the editor chooses the secret name and the value leaves for an editor-chosen URL. The ad tokens are the exception, see ad credential checks. Before moving routes or a type, set every key a destination uses **per workspace** (a global or environment `WAREHOUSE_*` or Slack key must be copied into each workspace that relies on it; list each workspace's secret names in Settings or through the secrets API before the cutover). A missing workspace key fails the delivery with a clear error; it never falls back.
    **Names must also use an allowed prefix:** `header_secret_ref` (webhook, `warehouse_http`) and `webhook_secret_ref` (Slack) must start with an `OUTBOUND_SECRET_REF_PREFIXES` entry (default `WEBHOOK_,AUTOMATION_,DEST_,SLACK_`; the worker reads it from its process env) and must not be a registered provider/CRM/SMTP/IMAP/ads credential; Slack refs additionally need `SLACK_` or `DEST_`. **Before cutover rename warehouse, webhook and Slack secrets to an allowed prefix and update the destinations.** Stored destinations outside the namespace now fail at delivery (closed, nothing sent). Optional `AD_TOKENS_WORKSPACE_ONLY=true` makes ad tokens workspace-only too (default false keeps workspace, global, env); with `CREDENTIALS_SOURCE=http` ad rows are then handed back to Python. Destination webhooks obey `AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST`: include `hooks.slack.com` when Slack destinations are used.
6. Executor cohort: Python decides the executor at enqueue. `audience_refresh` goes to Go only when `GO_EXECUTOR_ENABLED=true`, the workspace is in `GO_EXECUTOR_WORKSPACES` **and** `GO_EXECUTOR_AUDIENCE_REFRESH=true`; `audience_destination_sync` only when the first two hold and the destination type is in `GO_DEST_TYPES`. Set the same values on both stacks; Go never claims a Python row. The API reads `GO_EXECUTOR_WORKSPACES` on every call but the worker reads it at start: restart the worker after changing it, or the stacks disagree about the cohort.
6a. `CREDENTIALS_SOURCE=local` on the worker before listing `warehouse_http` (or `slack`): with `CREDENTIALS_SOURCE=http` the worker cannot tell a workspace secret from a global one, so it hands those rows back to Python and logs a warning at start.
7. **The Python scheduler stays** (D9): `bootstrap_audience_schedules` is the only ticker; it enqueues `audience_refresh` with `executor` per the flag above and the Python reaper keeps running. Go exposes `ScheduleNext`/`ReconcileFailure`/`DueSchedules` for its routes and handlers, but nothing calls them on a timer. The Postgres session time zone must be UTC (`audience_*` timestamps are naive-UTC columns written by both stacks).
8. The inbound callback is rate limited in memory per process: 120 requests per minute per client IP and per verified token, so N `api-go` replicas give N times the budget. It forwards the real client address from `TRUST_PROXY_CIDRS` (default loopback only; compose sets the fixed `lead_net` subnet; in Kubernetes set the ingress or nginx pod CIDR; the API must not be reachable from a LAN without nginx). The callback requires `READ COMMITTED`: **do not set `default_transaction_isolation` (or a pgbouncer equivalent) to anything stricter**, or every callback fails closed with 500.
9. Tunables (same on both stacks, defaults shown): `OPENGTM_DESTINATION_TOKEN_TTL_DAYS` 90 (1-365), `AUDIENCE_REFRESH_PAGE_SIZE` 500, `DEST_MEMBER_PAGE_SIZE` 500, `DEST_AD_BATCH_SIZE` 5000, `DEST_DELIVERY_TIMEOUT` 30 s, and Go-only `AUDIENCE_ACCOUNTS_MAX_MEMBERS` 200000 (422 above it).
10. nginx build contains `# BEGIN audiences-destinations` in `go-routes.conf`, and `scripts/nginx-routing-check.sh` passes at 0%, 50% and 100%. `$go_bucket` and `$go_ws_bucket` in `go-split.conf` carry the same percentage (create, patch and refresh are sticky per workspace).
11. Restart `api-go` and the worker after any flag change (config is read at boot).

## Flag order

Flip one at a time, in this order, watching between steps (all default off or empty):

1. `GO_AUDIENCE_ROUTES_ENABLED=true` and `GO_DEST_ROUTES_ENABLED=true` on `api-go`. Routes only; the canary stays at 0%. Then raise the nginx canary 5% -> 25% -> 100%. Compare `X-Upstream` and responses on a workspace at each step.
2. `GO_EXECUTOR_AUDIENCE_REFRESH=true` (plus `GO_EXECUTOR_ENABLED` and the cohort) on Python and the Go worker. A refresh is idempotent per fire key; start with one cohort workspace and compare `audience_members` counts and `refresh_health`.
3. `GO_DEST_TYPES`, **one type at a time**, each after the previous has run clean: `warehouse_http` first (workspace-only secret, HTTPS only), then the ad types (`meta_ads`, `google_ads`, `linkedin_ads`: check credentials below), then `slack` and `webhook`, then the plan 07/08 types (`hubspot`, `salesforce`, `instantly`, `smartlead`, `google_sheets`, `airtable`) as their ports are wired. Delivery is not idempotent across stacks for providers without a dedupe key: use one cohort workspace first.

## Routing

- `GET /api/audiences/capabilities` and `GET /api/audience-destinations/types` are always `api_backend` (retained).
- Everything else listed follows the client bucket (`X-Upstream` shows `py` or `go`); unknown siblings and trailing-slash variants stay on Python at every percentage, and no path is exposed that Python did not already serve.
- `POST /api/audiences`, `PATCH /api/audiences/{id}` and `POST /api/audiences/{id}/refresh` are sticky **per workspace** (`X-Workspace-Id`), so a refresh, its membership events and its destination-sync enqueue never split across stacks during the canary.
- `POST /api/audience-destinations/inbound/{id}` is the public CRM callback (bearer token, no login). It follows the client bucket (key: client address), so it is Python at 0% exactly as today, and forwards `X-Real-IP` and `X-Forwarded-For`. Go serves it from a pre-mux hook (not a mux pattern: it overlaps `{id}/sync` and `{id}/inbound-token`); `POST .../inbound/sync` and `.../inbound/inbound-token` still belong to those routes, as in Python.
- Inbound token rotation: **existing tokens keep working on both stacks** (only the SHA-256 digest is stored, both stacks verify it the same way), and a token created on one stack is accepted by the other. Go serializes rotate and revoke on the destination row lock (Python could leave two active tokens under concurrent rotation). Rotating a token does not interrupt a CRM that still sends the old token until the response is delivered; share the new token with the CRM immediately after rotating (it is shown once).
- Inline refresh (`POST /api/audiences`, `POST .../refresh`) is bounded by 120 s: a timeout is a 504 (the lock is released and degraded health is written).

## Cancellation semantics

- `POST .../runs/{id}/cancel` marks the run `cancelling`; a Go run settles at the next check: rows not yet delivered become `cancelled`, an in-flight ad batch finishes its current request and its remaining rows are closed `cancelled` ("Run cancelled before delivery"; Python leaves freshly created removal rows `pending` inside a finished run, Go closes them). A cancel that arrives while the run is `pending` is settled by whichever stack claims the job.
- A run that is `running` at the moment of a rollback is completed by `OnFailure` reconciliation: when the Go (or Python) job fails terminally the failure hook settles the open run and its deliveries (`failed`), and the audience refresh hook reschedules the next occurrence. Do not hand-edit run rows.
- Retry keeps the original `run_id` on removal rows and does not block on another active run (Python parity); `sync` is single-flight per destination and atomic with its enqueue (a second concurrent sync gets the active run).

## Ad credential checks

Before listing an ad type in `GO_DEST_TYPES`, per workspace and platform:

1. The token exists: Meta `META_ACCESS_TOKEN`, LinkedIn `LINKEDIN_ACCESS_TOKEN`, Google `GOOGLE_ADS_ACCESS_TOKEN` and `GOOGLE_ADS_DEVELOPER_TOKEN`. These four keep Python's lookup order (workspace secret, then global secret, then environment), by ruling: the key names are fixed, the value is only ever sent to the platform's fixed API host, and a deployment-wide ad account is a supported setup. A workspace editor can therefore write to any ad audience or customer id that a deployment-level token can reach by choosing it in the destination config; if that is not acceptable, set no deployment-level ad token and give each workspace its own. A missing key fails the delivery with an error naming the secret (never its value).
2. The destination config is valid: consent attested with a `consent_source`; platform ids are numeric (or valid path segments: `.`/`..` rejected) and `api_version` well formed. A legacy stored config that Python accepted but fails Go's validation fails its delivery with a clear error (fix the config), it is not silently sent.
3. Egress: hosts are fixed (Meta Graph, LinkedIn, Google Ads), redirects are not followed, responses are capped at 1 MiB, errors are status-only text and the `Authorization` header is never logged. The delivery `payload` stores identifier hashes only. A token expiry shows as `HTTP 401/403` in the delivery error.
4. Use one cohort workspace and a small audience first; compare the delivery rows (hashed identifiers, statuses, counters) with a Python run of the same audience.

## Draining the Go paths

1. Set `GO_DEST_TYPES` empty (or remove the type) and `GO_EXECUTOR_AUDIENCE_REFRESH=false` on Python and restart it: new jobs enqueue as `executor='python'`.
2. Wait until `SELECT count(*) FROM jobs WHERE type IN ('audience_refresh','audience_destination_sync') AND executor='go' AND status IN ('pending','processing')` is 0 (sync jobs time out at 1800 s, refresh at 900 s). A job already claimed by the Go worker finishes.
3. Then restart the Go worker with the same flags off: with the handler off it hands every row back to Python untouched (`queue.HandBack`); it never fails a row as unsupported.
4. Runs `running` at that moment are completed by `OnFailure` reconciliation if their job later fails; inspect runs stuck `running` beyond the job timeout and let the reaper retry them rather than editing rows.

## Rollback

1. Drain as above (types off, refresh flag off, wait for `executor='go'` rows to leave `pending/processing`), on Python and the Go worker; restart both.
2. Set the nginx percentage back to 0 (comment the `go` lines in `go-split.conf`, `nginx -t`, reload). The public inbound callback returns to Python; tokens issued on either stack keep working.
3. `GO_AUDIENCE_ROUTES_ENABLED=false` and `GO_DEST_ROUTES_ENABLED=false` last (routes unregister; Go then answers 404 for them, so only after the canary is 0).
4. No data migration is needed: both stacks use the same tables and revision `d3f5a7b9c1e2` stays (it only widens a column). Scheduled `audience_refresh` jobs now carry `jobs.workspace_id`. Rows written by Go (receipts, runs, deliveries, fire keys `destination_sync:<run>` and `audience_refresh:<aud>:<iso>`, idempotency keys `dest:<dest>:lead:<lead>:<sha256>`) are read by Python as is. Workspace-only secrets that you copied per workspace can stay.

## Deviations to expect (full list in the README)

- Stricter validation: 409 instead of 500 on a duplicate destination rename, 422 instead of 500 on `null` name/enabled, ad ids and `api_version` validated, Slack templates limited to `{name}` and brace escapes (other syntax is a 422), accounts rollup bounded (422), 1 MiB request bodies, list caps and id tiebreaks.
- Public callback rate limits (120/min per IP and per token) and a 1 MiB body cap; a non-POST to the callback path is a 404 (FastAPI: 405).
- Warehouse is HTTPS only and its error text redacts the secret; provider errors are status-only; Meta payload JSON is compact.
- Inline refresh bound of 120 s (504); `sync` single-flight and atomic with its enqueue.

## What to watch

- API log: `audience routes not registered`, `destination routes not registered`, `proxy headers from untrusted peer`, `inbound ...` failures (error type only, never the token, body or config).
- Worker log: `audience_refresh jobs are handed back to Python: no audience event emitter wired`, warnings for `GO_DEST_TYPES` types without a port or needing `CREDENTIALS_SOURCE=local`, per-delivery errors (status only).
- Database: destinations with `consecutive_failures` growing or `health` degraded, runs stuck `running` past the job timeout, `audience_membership_events` without a matching trigger enqueue, `jobs` with `executor='go'` stuck `pending` (worker flag off, handed back).
- Python bugs found while porting are collected in the README section "Python bugs found and not fixed" for issue #114.

## Verification commands

```sh
cd apps/backend-go
go vet ./... && go test -race ./internal/api -run 'Audience|Inbound|Destination|NoSecrets' -count=1
TEST_DATABASE_URL=<private clone> go test -race -tags integration ./internal/api ./internal/audiencerefresh ./internal/audiencesched ./internal/deststore ./internal/destsync ./internal/destinbound ./cmd/worker
scripts/nginx-routing-check.sh        # from the repo root: 0%, 50% and 100%
```
