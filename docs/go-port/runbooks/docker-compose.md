# Docker Compose stack (Python + Go side by side)

## Prerequisite: `legacy/python` must be present

The Python backend lives in `legacy/python/`. It is its **own git repo** and is **gitignored** by this repo
(Python stays untracked by decision), so a fresh clone of OpenGTM does not contain it. Clone it into place
before building any Python image:

```bash
git clone <legacy-python-repo-url> legacy/python    # or copy your existing checkout there
```

The Python services `migrate`, `api`, `seed`, `worker`, `scheduler` and `frontend_assets` all use the root
`Dockerfile` (build context `.`), whose first stage reads `legacy/python`. If it is missing the build stops with:

```
ERROR: legacy/python is missing from the build context.
```

CI and remote builds from a clean checkout must provide `legacy/python` the same way (clone it into the
workspace before `docker compose build`). The Go images need nothing from `legacy/`.

## Services

| Service | Image / build | Profile | Notes |
| --- | --- | --- | --- |
| `postgres`, `redis` | upstream images | default | healthchecked; host ports bound to 127.0.0.1 |
| `migrate` | root `Dockerfile` (Python) | default | one-shot Alembic + runtime role; everything else waits on it |
| `seed` | root `Dockerfile` (Python) | `demo` | opt-in demo data + `admin`/`admin`; not started by default |
| `api`, `worker`, `scheduler`, `frontend_assets` | root `Dockerfile` (Python) | default | built from `legacy/python`; web UI built from `apps/web` |
| `nginx` | `nginx:alpine` + `infra/nginx/*.conf` | default | public entrypoint (`PORT`, default 3000); canary split |
| `api-go` | `apps/backend-go/Dockerfile` target `api` | `go` | static binary, distroless nonroot, `/api -healthcheck` |
| `worker-go` | same Dockerfile, target `worker` | `go` | idle until `GO_EXECUTOR_*` flags are on; needs `INTERNAL_SERVICE_TOKEN` |
| `mcp` | same Dockerfile, target `mcp` | `mcp` | SSE on `mcp:3100`, internal only |
| `reacher`, `providersim` | upstream / Go | `reacher` / `bench` | optional |

The Go Dockerfile pins `golang:1.27.3` (matches `toolchain` in `apps/backend-go/go.mod`), builds with
`CGO_ENABLED=0 -trimpath`, and runs as `nonroot` on `gcr.io/distroless/static-debian12`.

## Commands

```bash
cp .env.example .env                       # optional for dev: every variable has a dev default
docker compose build                       # Python stack (needs legacy/python)
docker compose up -d                       # Python only: behaves exactly as before
docker compose --profile go build api-go worker-go
docker compose --profile go up -d api-go worker-go   # Go side, canary still 0%
```

Go-only build check (no Python needed): `docker compose --profile go --profile mcp build api-go worker-go mcp`.

## First run

A default `docker compose up` creates no user. The first admin is created by you in the UI:

1. Start the stack (`api-go` is part of the default stack and serves the setup endpoints; the Python API has none):
   `docker compose up -d`.
2. Open the UI (`http://localhost:${PORT:-3000}`). With no users it redirects to `/setup`.
3. Enter the setup token. Either set `SETUP_TOKEN` in `.env` beforehand, or read the one `api-go` generated:
   `docker compose logs api-go | grep -i "setup token"` (printed once per start while no admin exists).
4. Choose username and password (12+ characters) and an optional workspace name. You are signed in directly.

Token rules: `SETUP_TOKEN` wins; only `SETUP_TOKEN_DISABLED=1` opens setup without a
token; `APP_ENV` never does (so a dev-default install still demands the logged token). After setup
`POST /auth/setup` returns 409 permanently. nginx routes `/auth/setup` and `/auth/setup/status` to `api-go`
at every canary percentage, so `api-go` must be running for first run.

The old `seed` service (admin/admin plus a demo workbook) is behind the `demo` profile:
`docker compose --profile demo up`. Do not use it on a reachable host.

## Go env wiring and safe defaults

`api-go` / `worker-go` / `mcp` take plain `postgresql://` URLs (`GO_DATABASE_URL`, default is the `yupcha_runtime`
role the `migrate` service provisions), `REDIS_URL=redis://redis:6379`, `PY_WORKER_URL=http://api:8000`, and the
same `SECRET_KEY`, `SECRETS_PROVIDER`, `SECRETS_MASTER_KEY`, `INTERNAL_SERVICE_TOKEN` as Python (all from `.env`;
no secret is in `docker-compose.yml`). `lead_net` has a fixed subnet (`LEAD_NET_SUBNET`, default `172.29.250.0/24`) and `TRUST_PROXY_CIDRS` defaults to
it (the code default is loopback only; `TRUST_PROXY` is gone). Change both if the subnet collides. When
`GO_OUTREACH_ROUTES_ENABLED` or `GO_EXECUTOR_OUTREACH_SEND` is on, `OUTREACH_PUBLIC_BASE_URL` must be an https URL
(or `APP_ENV=dev`), otherwise the Go process refuses to start. After upgrading, run `suppressnorm --dry-run` then
`suppressnorm` once (`docker compose run --rm --entrypoint /suppressnorm worker-go --dry-run`; needs
`DATABASE_URL`, already set on `worker-go`). Schema revision `e7b2c4d6f8a0` is required (`migrate` applies it). Every `GO_*_ENABLED` route/executor flag and `COPILOT_GO_ENABLED`
defaults to `false`; with them off, and `go-split.conf` at 0%, the stack behaves like Python even with the `go`
profile running. Production: set `APP_ENV` and a `SECRET_KEY` of at least 32 bytes (api-go refuses to start
otherwise), identical on `api` and `api-go`.

## Canary

nginx always mounts `default.conf`, `go-split.conf`, `go-routes.conf`. The `api_go` upstream resolves lazily, so
nginx runs without `api-go`. To ramp, edit the `go` lines in `go-split.conf` (both knobs), then
`docker compose exec nginx nginx -t && docker compose exec nginx nginx -s reload`. Check `X-Upstream` on
`/auth/me`. See `api-cutover.md`.

## Workspace

Root `bun` workspaces are `apps/*` and `packages/*`. The Python API package (`@opengtm/api`) was removed from the
workspace and `bun.lock` because its directory now lives in the untracked `legacy/python`; run the Python API from
`legacy/python/apps/api` (`uv run ...`) rather than through turbo. `apps/backend-go` has no `package.json`, so
bun/turbo ignore it.

## Verified

`docker compose config` (with and without `--profile go --profile mcp --profile bench`), builds of `api-go`,
`worker-go`, `mcp`, `api`, `worker`, `scheduler`, and a boot of `postgres redis api-go nginx` (+ `migrate`,
`api`, `frontend_assets`) in an isolated project: nginx `/health` 200, `/auth/me` -> `X-Upstream: py`,
`api-go /healthz` ok from inside the network.
