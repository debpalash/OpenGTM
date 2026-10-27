# Docker Compose stack (Go)

Plan 15 removed the Python services. The stack builds from a clean checkout: nothing is read from
`legacy/python`. Python survives only inside the internal browser service (`apps/browser-service`).

## Services

| Service | Image / build | Profile | Notes |
| --- | --- | --- | --- |
| `postgres`, `redis` | upstream images | default | healthchecked; host ports bound to 127.0.0.1 |
| `migrate` | `apps/backend-go/Dockerfile` target `migrate` | default | one-shot Go runner (goose baseline, Alembic adoption, runtime role); everything else waits on it. See `migrations.md` |
| `api-go` | target `api` | default | API + embedded web UI (Bun stage builds `additional_contexts.web` = `./apps/web`); distroless, `/api -healthcheck`; host port `127.0.0.1:${API_GO_PORT:-8001}` |
| `worker-go` | target `worker` | default | durable job worker; scale with `--scale worker-go=N` |
| `scheduler-go` | target `scheduler` | default | recurring-job reconciler; advisory lock keeps one instance active |
| `browser` | `apps/browser-service/Dockerfile` | default | internal browser service: `browser_net` (internal) + `browser_egress`; no DB, no `env_file`, no host port. See `browser-service.md` |
| `nginx` | `nginx:alpine` + `infra/nginx/default.conf` | default | public entrypoint (`PORT`, default 3000); everything proxies to `api-go` |
| `seed-go` | target `scheduler` (`/opengtm-seed`) | `demo` | seeds the demo workbook into the workspace `/setup` created and starts its run; never creates a user |
| `mcp` | target `mcp` | `mcp` | SSE on `mcp:3100`, internal only; stdio: `docker compose --profile mcp run --rm -T --no-deps mcp stdio` |
| `reacher`, `providersim` | upstream / Go | `reacher` / `bench` | optional |

The Go Dockerfile pins `golang:1.27.3` (matches `toolchain` in `apps/backend-go/go.mod`), builds with
`CGO_ENABLED=0 -trimpath`, and runs as `nonroot` on `gcr.io/distroless/static-debian12`.

## Required secrets

`SECRET_KEY` (>= 32 bytes), `INTERNAL_SERVICE_TOKEN` (>= 32 chars) and `BROWSER_SERVICE_TOKEN` (>= 32 chars) in
`.env`; `scripts/install.sh` generates all three plus the database passwords. The Go services refuse to start
without them (`APP_ENV=test` relaxes only `SECRET_KEY`), and the browser service exits without its token.

## Commands

```bash
./scripts/install.sh --no-start            # or: cp .env.example .env and fill the three secrets
docker compose up -d --build
docker compose logs api-go | grep -i "setup token"
docker compose --profile demo up seed-go   # optional demo workbook, after /setup
```

## First run

1. `docker compose up -d`, open `http://localhost:${PORT:-3000}`; with no users it redirects to `/setup`.
2. Enter the setup token: `SETUP_TOKEN` from `.env`, or the one `api-go` logs once per start while no admin exists.
3. Choose username and password (12+ characters) and an optional workspace name.

Only `SETUP_TOKEN_DISABLED=1` opens setup without a token; `APP_ENV` never does. After setup `POST /auth/setup`
answers 409.

## Go env wiring

- All Go services share one env block (`x-go-env` in `docker-compose.yml`): runtime-role `DATABASE_URL` (built from
  `YUPCHA_RUNTIME_DB_USER`/`YUPCHA_RUNTIME_DB_PASSWORD`/`POSTGRES_DB`, or `GO_DATABASE_URL`), `REDIS_URL`,
  `SECRET_KEY`, `SECRETS_*`, `INTERNAL_SERVICE_TOKEN`, `CREDENTIALS_SOURCE=local`, `PG_LEAD_STORE=true`,
  `BROWSER_SERVICE_URL`/`BROWSER_SERVICE_TOKEN`.
- `GO_ALL_NATIVE=true`: every `GO_*` route/executor flag (including `GO_SCHEDULER_ENABLED` and
  `GO_TRAILING_SLASH_COMPAT`) is on unless set explicitly in `.env`. `COPILOT_GO_ENABLED` and `GO_AI_BATCH_ENABLED`
  are set to `true` directly (not covered by the switch).
- `api-go`, `worker-go`, `scheduler-go` and `mcp` also read `.env` (`env_file`): provider and LLM keys,
  `GOOGLE_API_KEY`/`GOOGLE_CSE_ID`/`GOOGLE_BOOKS_API_KEY`, `LINKEDIN_LI_AT_COOKIE`, SMTP, SSO. The browser service
  does not.
- `lead_net` has a fixed subnet (`LEAD_NET_SUBNET`, default `172.29.250.0/24`); `TRUST_PROXY_CIDRS` defaults to it.
- `OUTREACH_PUBLIC_BASE_URL` must be https once outreach is used (`APP_ENV=dev` allows http).
- After upgrading from a Python install, run `suppressnorm` once:
  `docker compose run --rm --entrypoint /suppressnorm worker-go --dry-run`, then without `--dry-run`.

## Upgrading from the Python stack

Stop the old stack (`docker compose down`, keeping volumes), check out the new release, then
`docker compose up -d --build`. `migrate` adopts an Alembic database at the final revision. Pending jobs that were
enqueued for the Python worker are reassigned by `15-cutover.md`. The old `api`, `worker`, `scheduler` and `seed`
services, the root `Dockerfile`, `infra/nginx/go-split.conf` and `go-routes.conf` no longer exist; the per-plan
`*-cutover.md` runbooks describe the side-by-side period and are kept as history.

## Workspace

There is no root Bun workspace: `apps/web`, `apps/docs`, `packages/chrome-extension` and
`packages/n8n-nodes-yupcha` each have their own `package.json` and `bun.lock`. The root `Makefile` drives local
builds: `make build` (web UI embedded into `bin/opengtm-api`, plus worker, mcp, scheduler, seed, migrate),
`make run`, `make dev`, `make kill`, `make up`, `make down`, `make test`.

## Verified (plan 15 WP-RETIRE-INFRA)

`docker compose config` for the default and every profile. On a clean `git archive` checkout (no `legacy/`),
`docker compose -p opengtm-retire up -d --wait` with overridden host ports: `migrate` exited 0 (baseline, runtime
login), every service healthy or running, nginx served `/`, `/health`, `/auth/setup/status`; setup with the logged
token, then `/auth/token`, `/auth/me`, `/api/workspaces` returned 200; `worker-go` and `scheduler-go` (lease held)
started; `browser` answered `/v1/health` (`browser: ready`, sandbox on) only with the bearer token, and
`browser_net` had no internet route; `seed-go` seeded and started a run that the Go worker claimed.
`scripts/nginx-routing-check.sh` passes.
