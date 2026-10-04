# opengtm server

One Go binary for the OpenGTM platform rewrite
([RFC](../../docs/plans/hybrid-platform-rewrite.md)). It runs alongside the
Python API during the migration: Go owns the routes and job types that have
moved, and proxies everything else to FastAPI.

| Command | Role |
| --- | --- |
| `opengtm serve [--worker]` | Web UI, Go-owned `/api/v2` routes, live events, and a reverse proxy to FastAPI for every other path. `--worker` also runs the queue in-process (the `lite` profile). |
| `opengtm worker` | Durable queue worker for job types routed to Go. Safe to run as many replicas. |
| `opengtm routes list\|set <type> <python\|go>` | Choose which executor claims each job type. Drain in-flight jobs before switching. |
| `opengtm doctor [--json]` | Check configuration, the database role (must be `NOSUPERUSER NOBYPASSRLS`), migrations, routes and the legacy API. |
| `opengtm plugin ...` | Build, test, record, run, sign, pack and install plugins ([guide](../../docs/plugins/README.md)). |
| `opengtm health` | Probe `/healthz` (container healthchecks). |

## Run it

With the existing Compose stack running:

```bash
docker compose --profile server up -d server   # http://127.0.0.1:3080
docker compose exec server opengtm doctor
```

The image is built from the repository root with
`docker build -f apps/server/Dockerfile .`. It embeds the dashboard and the
Rust extraction kernel, bundles first-party plugins and the v1 connectors,
and runs as a non-root distroless container.

For development:

```bash
cd apps/server
go run ./cmd/opengtm serve --worker      # reads DATABASE_URL and the variables below
go test ./...                            # unit tests
OPENGTM_TEST_DATABASE_URL=postgresql://postgres:postgres@127.0.0.1:5432/opengtm_test \
  go test -race ./...                    # plus PostgreSQL integration tests (uses uv for migrations)
```

Integration tests need a disposable database: they migrate it with Alembic and
create their own `NOSUPERUSER NOBYPASSRLS` roles so row-level security is
really enforced.

## Configuration

Environment variables match the Python settings so one `.env` drives both.
An optional `opengtm.yaml` (`--config` or `OPENGTM_CONFIG`) sets the same
values; the environment wins. Invalid values stop startup with a clear error.

| Variable | Default | |
| --- | --- | --- |
| `DATABASE_URL` | required | PostgreSQL; SQLAlchemy URLs such as `postgresql+psycopg://` are accepted. Use the runtime role, never a superuser. |
| `OPENGTM_LISTEN` | `:8080` | |
| `OPENGTM_LEGACY_API_URL` | `http://127.0.0.1:8000` | FastAPI, for proxying and workspace authorization. |
| `OPENGTM_WEB_DIR` | embedded build | Serve the dashboard from a directory instead. |
| `WORKER_CONCURRENCY`, `WORKER_MAX_ACTIVE_PER_WORKSPACE`, `WORKER_SHUTDOWN_GRACE_SECONDS` | 1, 2, 30 | Same meaning and limits as the Python worker. |
| `OPENGTM_PLUGIN_DIRS`, `OPENGTM_CONNECTOR_DIRS`, `CONNECTOR_SIGNATURE_POLICY`, `OPENGTM_PLUGIN_TRUST_STORE`, `OPENGTM_EGRESS_PROXY` | see the [plugin guide](../../docs/plugins/README.md#running-plugins-on-a-server) | |
| `LOG_LEVEL` | `info` | JSON logs on stderr. |

## How it fits with the Python stack

- **One jobs table.** `job_executor_routes` decides which executor may claim
  each job type; unrouted types stay with Python. Both use the same lease
  (`worker_id`, `locked_at`), retry, backoff and cancellation semantics, and
  each reaps only its own types.
- **Authorization** is delegated to FastAPI's
  `/api/auth/workspace-context` (membership, roles, SSO enforcement) and
  cached for 30 seconds per token and workspace.
- **Tenancy.** Every tenant query runs in a transaction bound with
  `set_config('app.workspace_id', ..., true)` under forced row-level security.
- **Progress** uses PostgreSQL `LISTEN/NOTIFY`, delivered on
  `/api/v2/events`. Unlike the Redis stream there is no replay buffer; a
  client that reconnects should refetch state.

## Layout

```text
cmd/opengtm          subcommands (one file each)
internal/config      configuration
internal/db          pgx pool, tenant transactions, test fixtures (dbtest)
internal/queue       jobs queue executor, routing, leases
internal/authz       forward authorization to FastAPI
internal/progress    LISTEN/NOTIFY events
internal/server      HTTP front door, legacy proxy, SPA
internal/egress      guarded outbound HTTP (SSRF, DNS pinning, robots.txt, rate limits)
internal/kernels     Rust kernels (WebAssembly) for extraction and normalization
internal/plugin/...  manifests, templates, signing, bundles, fixtures, runtimes
internal/pluginrun   plugin catalog, plugin_run jobs and /api/v2 plugin routes
```
