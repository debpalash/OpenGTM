# Runbook: Go API side-by-side cutover (plan 02)

Go serves a subset of the API next to FastAPI behind the same nginx. Routing is a percentage
split, so ramp and rollback are config edits plus `nginx -s reload`.

## Routing summary

| Path | Upstream |
| --- | --- |
| `= /auth/token`, `= /auth/refresh`, `= /auth/me`, `^~ /api/workspaces`, `^~ /api/mcp/tokens` | Go for the canary percentage, else Python |
| `/auth/sso/*` (OIDC flow, needs the Starlette session cookie) | always Python |
| everything else (`/api/*`, `/health`, SPA) | Python / static, unchanged |
| `/admin/*`, `/scim/*` | **not exposed by nginx** (user decision). Go serves them on its own port only, exactly like Python today. The SPA fallback answers these paths; they never reach either API. |

Files: `infra/nginx/go-split.conf` (http level: `split_clients` -> `$go_bucket`, `map` -> `$auth_upstream`),
`infra/nginx/go-routes.conf` (the five locations, included in `server`), `infra/nginx/default.conf`
(`upstream api_go`, the two includes). Note: the brief put `split_clients` in `go-routes.conf`, but it is
http-level only, so it lives in its own file. Responses on Go-eligible routes carry `X-Upstream: go|py`.

## Prerequisites (do not skip)

1. **Plan 01 cutover is done**: `docs/go-port/runbooks/sqlite-to-postgres.md`. Go reads workspaces and
   members from PostgreSQL; Go must not serve traffic before Python runs on Postgres.
2. **Migrate before starting api-go.** The Go schema gate is an exact match on Alembic head
   `e1f5a9b3d7c2`; api-go refuses to start on any other revision. Run the Python migration first.
3. `SECRET_KEY` identical on `api` and `api-go` (tokens must cross over). api-go refuses to start when
   `SECRET_KEY` is empty, the insecure default, or shorter than 32 bytes, unless `APP_ENV` is EXPLICITLY a
   dev value (`dev`, `development`, `test`, `testing`, `local`). An unset `APP_ENV` counts as non-dev
   (fail closed; Python treats it as dev), so set `APP_ENV` on the `api-go` service.
   `MCP_TOKEN_TTL_DAYS=0` (or negative) is accepted and means MCP tokens never expire, as in Python.
   `DATABASE_URL` must use a non-superuser, NOBYPASSRLS role: api-go queries `pg_roles` at startup and
   refuses to start otherwise (`PG_RLS_REQUIRE_SAFE_ROLE=false` downgrades it to a CRITICAL log, same as
   Python; not recommended).
4. The two nginx mounts below are ALREADY in `docker-compose.yml` (nginx would not start without them, since `default.conf` includes both files). The `api-go` service is not added yet; use this snippet:

```yaml
  nginx:
    volumes:
      - ./infra/nginx/default.conf:/etc/nginx/conf.d/default.conf:ro
      - ./infra/nginx/go-split.conf:/etc/nginx/go-split.conf:ro
      - ./infra/nginx/go-routes.conf:/etc/nginx/go-routes.conf:ro
    depends_on: [api, api-go]   # api-go resolves lazily (`resolve`), but keep ordering explicit

  api-go:
    image: opengtm-api-go:local          # built from apps/backend-go/Dockerfile.api
    environment:
      API_ADDR: ":8000"
      DATABASE_URL: ${DATABASE_URL}      # runtime role: non-superuser NOBYPASSRLS
      SECRET_KEY: ${SECRET_KEY}
      APP_ENV: ${APP_ENV}
    depends_on: *same-as-api             # copy the `api` service's depends_on (db, migrations)
    healthcheck:
      test: ["CMD", "/api", "-healthcheck"]   # distroless: no wget/shell; checks 127.0.0.1:<port of API_ADDR>/healthz
```

## Canary ramp

The percentage lives on the single `go` line in `go-split.conf`, shipped commented out (0%).
Buckets are sticky per `Authorization` header + client address.

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
