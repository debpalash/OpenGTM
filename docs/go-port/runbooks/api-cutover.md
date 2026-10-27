# Runbook: Go API side-by-side cutover (plan 02)

> **Historical (plan 15).** OpenGTM now runs without the Python backend: the Python api, worker, scheduler
> and seed services, the root `Dockerfile` and the nginx canary (`go-split.conf`, `go-routes.conf`) are gone, and
> every Go path is on (`GO_ALL_NATIVE=true` in compose). This document records the side-by-side period. Current
> operation: `docs/go-port/runbooks/docker-compose.md`; final cutover: `docs/go-port/runbooks/15-cutover.md`.

Go serves a subset of the API next to FastAPI behind the same nginx. Routing is a percentage
split, so ramp and rollback are config edits plus `nginx -s reload`.

## Routing summary

| Path | Upstream |
| --- | --- |
| `= /auth/token`, `= /auth/refresh`, `= /auth/me`, `^~ /api/workspaces`, `^~ /api/mcp/tokens` | Go for the canary percentage, else Python |
| `location ^~ /auth/sso/` (OIDC status, login, callback) | **always Go** (`api_go`, no canary; the flow cookie `opengtm_sso_flow` is set by Go `/login` and checked by Go `/callback`; the access log omits the query) |
| `= /auth/setup`, `= /auth/setup/status` (first-admin setup) | **always Go** (no canary), see `docker-compose.md` "First run" |
| `= /api/events/ticket` (SSE ticket, Go only) | Go for the same canary bucket as `/api/events` |
| everything else (`/api/*`, `/health`, SPA) | Python / static, unchanged |
| `/admin/*`, `/scim/*` | **not exposed by nginx** (user decision). Go serves them on its own port only, exactly like Python today. The SPA fallback answers these paths; they never reach either API. |

Files: `infra/nginx/go-split.conf` (http level: `split_clients` -> `$go_bucket`, `map` -> `$auth_upstream`),
`infra/nginx/go-routes.conf` (the five locations, included in `server`), `infra/nginx/default.conf`
(`upstream api_go`, the two includes). Note: the brief put `split_clients` in `go-routes.conf`, but it is
http-level only, so it lives in its own file. Responses on Go-eligible routes carry `X-Upstream: go|py`.

## Prerequisites (do not skip)

1. **Plan 01 cutover is done**: `docs/go-port/runbooks/sqlite-to-postgres.md`. Go reads workspaces and
   members from PostgreSQL; Go must not serve traffic before Python runs on Postgres.
2. **Migrate before starting api-go.** Run `opengtm-migrate` (compose `migrate` service; `migrations.md`). The Go
   schema gate requires `goose_db_version` >= `config.RequiredGoSchemaVersion`; api-go refuses to start otherwise.
   An Alembic database at `e7b2c4d6f8a0` is adopted, an older one is refused until it is upgraded with the legacy tool.
3. `SECRET_KEY` identical on `api` and `api-go` (tokens must cross over). api-go refuses to start when
   `SECRET_KEY` is empty, the insecure default, or shorter than 32 bytes, unless `APP_ENV=test`. `APP_ENV=dev`
   (the compose default) does NOT relax this; compose itself fails when `SECRET_KEY` is unset. The worker and
   the MCP server enforce the same rule.
   `MCP_TOKEN_TTL_DAYS=0` (or negative) is accepted and means MCP tokens never expire, as in Python.
   `DATABASE_URL` must use a non-superuser, NOBYPASSRLS role: api-go queries `pg_roles` at startup and
   refuses to start otherwise (`PG_RLS_REQUIRE_SAFE_ROLE=false` downgrades it to a CRITICAL log, same as
   Python; not recommended).
4. `api-go` is already defined in `docker-compose.yml` under the `go` profile (image `opengtm-api-go:local`, built from
   `apps/backend-go/Dockerfile` target `api`), as are the nginx mounts. Start it with
   `docker compose --profile go up -d api-go`; see `docker-compose.md` for env, flags and healthcheck.

## Canary ramp

The percentage lives on the single `go` line in `go-split.conf`, shipped commented out (0%).
Buckets are sticky per **client address only** (`$remote_addr`); the key never includes an `Authorization` header or any other client-chosen value, so a caller cannot steer itself onto the weaker stack. Behind another proxy or load balancer, restore the real client address (realip) first, or all clients share one bucket. `/auth/setup*`, `/auth/sso/*`, `/api/outreach/unsubscribe` and `/api/outreach/webhooks/bounce` are pinned to Go at every percentage. `/auth/token` and `/auth/refresh` still follow the canary: **move them to 100 % first** (the inactive-user and SSO-scope fixes only apply to Go-served traffic), before the rest of the ramp. The workspace-sticky routes (run, copilot) key on the `X-Workspace-Id` header and fall back to the client address.

```
# 0%:    "# 5%  go;" (commented)       # nginx rejects a literal 0%
# 5%:    "    5%  go;"
# 25%:   "    25% go;"
# 100%:  "    100% go;"
docker compose exec nginx nginx -t && docker compose exec nginx nginx -s reload
```

Steps: 0 -> 5 -> 25 -> 100, soaking at each step.

**Gate to promote** (every step): `make test-integration` and the contract tests green; Go-route error
rate (5xx and 401/403 share, split by `X-Upstream`) <= Python baseline for 24 h; and
`scripts/cross_token_check.sh` passes (Python-minted token works on Go and vice versa; refresh tokens
rejected as access tokens).

Dashboards to watch: nginx 5xx and 4xx by `X-Upstream` on the five routes, p95 latency per upstream,
api-go `/healthz` and restarts, Postgres connection count and RLS/permission errors, login failure rate
(`/auth/token` 401s), rate-limit 429s.

Shared limiter keys (all plans' rate-limited routes: collect, workbook run/cell/sources, domain-intel, lead utilities, outreach `smtp/test`): since the plan 09 final fix wave the bucket is the authenticated workspace (else user, else client IP via `X-Real-IP` from a `TRUST_PROXY_CIDRS` peer), never `X-Workspace-Id`, a bearer prefix or `X-Forwarded-For`; `TRUST_PROXY` was removed. The `TRUST_PROXY_CIDRS` default is loopback only, so set it to the network nginx reaches the API from (compose sets it to the fixed `lead_net` subnet). Details: contract README, "Rate-limit keys (all plans)".

## SSO (Go-served)

Before the reload that routes `/auth/sso/` to Go:

1. Set `SSO_ALLOWED_ISSUERS` to the full issuer URL of every enabled workspace config (comma list). Host-only approval no longer works: an issuer must equal an entry exactly (scheme, host, port, path). List them with, as the schema owner:
   `SELECT workspace_id, value::json->>'issuer' FROM workspace_settings WHERE key='OIDC_CONFIG';`
   `SSO_ALLOWED_ISSUER_HOSTS` is deprecated: when set it only narrows and never approves alone. Stored configs approved only by host stop working until the issuer is listed.
2. Set `SSO_REDIRECT_BASE_URL=https://<public origin>` (**required whenever SSO is enabled**: there is no `Host` / `X-Forwarded-Proto` fallback, and SSO config/login is refused with a clear error without it); the IdP's registered redirect URI stays `https://<origin>/auth/sso/<slug>/callback`.
3. A self-hosted IdP on a private address: set `SSO_EGRESS_ALLOW_HOSTS=<idp host>[:port]` (comma list). `EGRESS_ALLOW_HOSTS` does not apply to SSO. Nothing is allowed by default; public IdPs work as before.
4. Review Python-era bindings of privileged accounts and delete unexpected rows. Go honours existing `(workspace, issuer, subject)` bindings (also for owners and admins, who would otherwise be locked out under `enforce_sso`) but never creates one for an owner, admin or global admin:
   `SELECT i.workspace_id, i.issuer, i.subject, u.username, m.role FROM workspace_oidc_identities i JOIN users u ON u.id=i.user_id LEFT JOIN workspace_members m ON m.workspace_id=i.workspace_id AND m.user_id=i.user_id WHERE m.role IN ('owner','admin') OR u.is_admin;`
5. SSO session scope (`sso_ws` claim) is enforced only by Go. Before relying on it (for example for `enforce_sso` workspaces), route every workspace API an SSO user reaches, plus `/auth/refresh`, to Go at 100 %. Python-served routes accept an SSO token for any workspace the user is a member of, and Python's `/auth/refresh` drops the claim.

## Verification

```
bash scripts/nginx-routing-check.sh            # throwaway nginx + stub upstreams, asserts 0% and 100%
docker compose exec nginx nginx -t
PY_URL=http://127.0.0.1:8000 GO_URL=http://127.0.0.1:8001 CHECK_EMAIL=... CHECK_PASSWORD=... \
  scripts/cross_token_check.sh                 # live services; credentials via env only
```

## Rollback

- Instant: comment out the `go` line in `go-split.conf`, then `nginx -t && nginx -s reload`. Python serves the
  same rows with the same `SECRET_KEY`; existing tokens stay valid.
- Or remove the `include /etc/nginx/go-routes.conf;` line from `default.conf` and reload.
- Data rollback is not needed: Go writes only rows Python already reads. Known response (not data)
  deviation: a workspace slug collision returns 409 on Go where Python returns 500; no extra rows are
  created in that case. Go also requires `exp` on tokens (Python tolerates absence; all minted tokens carry it).

## Required deploy step before Python is retired: tenancy backfill

(Fresh databases need no backfill: first-run `/setup` claims the ownerless workspaces. This step matters only for databases migrated from a Python install, where the admin and workspaces already exist.)

Python runs `ensure_tenancy_backfill(admin.id)` at every boot (`legacy/python/apps/api/main.py:100-116`): it picks the oldest global admin and gives that user a workspace membership and a default workspace. **Go has no equivalent.** While the Python API still boots, nothing is lost. A Go-only boot against a fresh or restored database leaves the global admin without memberships, so every workspace-scoped route answers 403.

Before Python is retired (and before the first Go-only start of any new environment), run the backfill once from the Python tree, or insert the rows by hand:

```bash
cd legacy/python && uv run python -c "
from apps.api.database import SessionLocal
from apps.api.models import User
from apps.api.services.workspace.manager import ensure_tenancy_backfill
db = SessionLocal()
admin = db.query(User).filter((User.is_admin == True) | (User.role.in_(['admin','superadmin']))).order_by(User.id.asc()).first()
assert admin, 'no admin user'
ensure_tenancy_backfill(admin.id)"
```

Verify: `SELECT count(*) FROM workspace_members WHERE user_id = <admin id>;` is at least 1 and `user_active_workspace` has a row for the admin. Record the step in the deploy checklist of the release that retires Python.

## Upgrade notes (post-port fix wave)

Operator-visible behaviour changes since the first Go cutover plans. Read before upgrading an environment that already runs Go, and before the first Go start of a new one.

**Schema and migrations**
- The schema is owned by the Go runner `opengtm-migrate` (`migrations.md`); the compose `migrate` service runs it from the api-go image, and the Python migrate job is retired. Its baseline is Alembic `e7b2c4d6f8a0` (down `d3f5a7b9c1e2`: PUBLIC `EXECUTE` on `outreach_send_by_message_id` revoked, granted to `yupcha_app`, `ix_outreach_sends_message_id`). An Alembic database at `e7b2c4d6f8a0` is adopted; an older one must first be upgraded with `alembic upgrade e7b2c4d6f8a0` from `legacy/python`. `alembic_version` stays stamped `e7b2c4d6f8a0` for the Python services. Go binaries check `goose_db_version` (`RequiredGoSchemaVersion`), not `alembic_version`. `scripts/go_schema_dump.sh` regenerates `schema.sql` from the Go migrations and re-appends the REVOKE/GRANT.
- Run the one-time `suppressnorm` after upgrade (`suppressnorm --dry-run`, then `suppressnorm`; shipped in the worker image as `/suppressnorm`; needs `DATABASE_URL`; see `outreach-cutover.md`).

**Secrets and outbound references**
- Rename webhook and destination secrets to an allowed prefix and update the rules, watches and destinations that reference them. `OUTBOUND_SECRET_REF_PREFIXES` (default `WEBHOOK_,AUTOMATION_,DEST_,SLACK_`; set it on api-go and the worker) lists the allowed prefixes. Slack refs need `SLACK_` or `DEST_`. Provider, CRM, SMTP, IMAP and ads credentials can never be referenced, and the fixed deny-list also covers `OIDC_`, `STRIPE_`, `SECRET_`, `JWT_`, `DATABASE_`, `REDIS_`, `INSTANTLY_`, `SMARTLEAD_` and `AIRTABLE_` whatever `OUTBOUND_SECRET_REF_PREFIXES` says. A process without a settings registry refuses every reference.
- A missing, empty, global-only or out-of-namespace secret ref now fails closed at delivery (nothing is sent) instead of skipping the header. Create and patch answer 422 with one text for not-allowed and not-found.
- Optional `AD_TOKENS_WORKSPACE_ONLY=true` makes ad tokens workspace-only (default false).
- `AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST` now also applies to destination webhooks and Slack destinations: include `hooks.slack.com` when you use Slack destinations. Webhook urls need a literal scheme and host.

**SSO and sessions**
- `SSO_ALLOWED_ISSUERS` (exact full issuer URLs) replaces host-only approval; `SSO_EGRESS_ALLOW_HOSTS` is the only egress list for the IdP client (empty by default). See "SSO (Go-served)" above, including the review SQL for Python-era bindings.
- SSO sessions are workspace-scoped (`sso_ws`). The scope holds only once every workspace route an SSO user reaches and `/auth/refresh` are 100 % Go.
- `POST /auth/token` for an inactive user answers 401 (no token).
- `POST /api/events/ticket` and `GET /api/events?ticket=` replace `?token=` for SSE. `SSE_QUERY_TOKEN_UNTIL` (default `2027-01-31T00:00:00Z`) ends the legacy window; move `/api/events` to Go at 100 % before it passes.

**First run and compose**
- A fresh install has no users: the UI redirects to `/setup`. `SETUP_TOKEN` (wins) or a token that api-go logs once; `SETUP_TOKEN_DISABLED=1` is the only switch that opens setup without a token (`APP_ENV` does not). Afterwards `POST /auth/setup` answers 409 once the caller presents a valid token (a missing or wrong token gets 401/403 first). On a migrated install that already has users, api-go writes the `setup_complete` marker at start, which closes setup. Set `SETUP_TOKEN` explicitly with more than one api-go replica. The `seed` service (admin/admin) now lives behind the `demo` profile. Setup is served only by api-go, so it must be running.
- `FILES_API_MODE=legacy` was removed; api-go refuses to start with it. Delete it from env files.
- `TRUST_PROXY` was removed (a deprecation warning is logged); `TRUST_PROXY_CIDRS` defaults to loopback only. Compose gives `lead_net` the fixed subnet `LEAD_NET_SUBNET` and defaults `TRUST_PROXY_CIDRS` to it. Set it explicitly outside compose.
- `OUTREACH_PUBLIC_BASE_URL` must be an https URL when `GO_OUTREACH_ROUTES_ENABLED` or `GO_EXECUTOR_OUTREACH_SEND` is on and `APP_ENV` is not a dev value (unset counts as non-dev); otherwise the Go process refuses to start.
- `CHAT_REQUIRE_AUTH` defaults to true; `COPILOT_MAX_ANON_STREAMS` (default 4) caps anonymous chat streams.

**Scheduling and idempotency keys**
- Schedule and poll fire keys are rendered in UTC (`sched:<id>:<UTC +00:00>`, `watch:<id>:<UTC +00:00>`) and next runs are `anchor + n*interval` in absolute time. At deploy a slot already fired under the old key spelling can fire once more.
- The bounce webhook ledger key is now `<kind>|<message_id>`. Rows written by older builds no longer dedupe, so one replay after upgrade may re-apply (the breaker still counts it once).
- Naive timestamps (`older_than`, conditions) are read as UTC; the `TZ`-equal-to-Python requirement is gone.

**Behaviour changes worth a release note**
- Dry run of an automation is side-effect free (no webhook, CRM push or enrichment); free actions count against `max_actions_per_day`.
- Callers without `automations.manage` see webhook header values as `***`.
- Person-intel deep scrape runs in Go; `deep_unavailable` is emitted only when a JavaScript-wall page needs the Python worker and it is unreachable.
- Bounce webhook events other than bounce, hard_bounce, soft_bounce, complaint and spam_report answer 200 `ignored`; suppressions without exactly one `@` are 422.
- Audience PATCH with a null `name`, `description`, `refresh_enabled` or `refresh_interval_minutes` answers 422 (not the duplicate-name 409); `POST /api/events/ticket` is a Go-only route.

## Final fix wave: deploy notes

1. **Schema: run `opengtm-migrate` before api-go starts** (compose `migrate` service, or `make migrate`; see `migrations.md`). api-go/worker/mcp refuse to start below `RequiredGoSchemaVersion`. `legacy/python` commit `86f41c6` is needed **only for upgrading pre-`e7b2c4d6f8a0` databases** (`alembic upgrade e7b2c4d6f8a0`, then `opengtm-migrate` adopts); fresh and `e7b2c4d6f8a0` databases need no Python.
2. **Only Go-served traffic has the fixes.** The post-port security fixes (inactive-user login, bounce-webhook hardening, limiter pools, SSO scoping through Go `/auth/refresh`) are effective only for requests Go serves. At 0 % the edge still sends `/auth/token`, `/auth/refresh` and most `/api/*` to Python. Plan the ramp accordingly: `/auth/token` + `/auth/refresh` to 100 % first.
3. **Canary key.** Client address only (see "Canary ramp"). Public outreach unsubscribe and bounce routes are pinned to Go: enable `GO_OUTREACH_ROUTES_ENABLED` / `PG_LEAD_STORE` first.
4. **UTC session precondition (hard).** Go connections must run with Postgres `timezone = UTC`. Go no longer reads the session TimeZone; schedule/fire keys and naive timestamps are UTC. Set it on the runtime role or in `DATABASE_URL` before the first canary percentage. Go itself is session-zone independent: every naive timestamp it writes (`jobs.next_run_at`, audience, retention, destination and workbook columns) or compares with an aware instant is converted with `AT TIME ZONE 'UTC'`. Python still renders fire keys and `next_run_at` in the session zone, so the two stacks agree only on UTC. During a canary, check for duplicate pending scheduled jobs: more than one pending job per trigger or watch (same id in the payload, different `fire_key`) with `next_run_at` hours apart means a Python and a Go writer disagreed on the zone.
5. **SECRET_KEY.** At least 32 bytes on api, worker and mcp, identical to Python. Only `APP_ENV=test` relaxes it.
6. **SSO.** `SSO_REDIRECT_BASE_URL` is required when SSO is enabled.
7. **First-run token in logs.** The generated setup token is logged once at WARN and stays in log storage after setup closes. It is useless once `setup_complete` is set, but apply your normal log-retention policy, or set `SETUP_TOKEN` yourself.
8. **Oversized run `targets`** now answer 422 instead of being truncated; **dry runs** never hand back to Python and send nothing.
