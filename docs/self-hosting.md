# Self-hosting OpenGTM

This guide covers installing, configuring and verifying a self-hosted OpenGTM
with the `opengtm` binary. For upgrades, backups and rollback, see the
[runbook](operations/upgrade-backup-rollback.md). The design is in the
[platform RFC](plans/hybrid-platform-rewrite.md#deployment-and-self-hosting).

## Quickstart

You need Docker with Compose v2.20 or newer. Nothing else.

```bash
curl -fsSL https://github.com/debpalash/OpenGTM/releases/latest/download/install.sh | bash -s -- --quickstart
```

That installs the `opengtm` binary (after verifying it, see
[Verifying a release](#verifying-a-release)), then runs
`opengtm init --profile lite --yes --start` in `./opengtm`. When it finishes,
open <http://localhost:3000> and sign in with the password in
`opengtm/.opengtm-initial-credentials`. Change it and delete the file.

The same thing step by step:

```bash
opengtm init --dir ~/opengtm --profile lite     # writes .env, opengtm.yaml, compose.yml
cd ~/opengtm
docker compose up -d                            # or: opengtm init ... --start
opengtm doctor --dir .                          # checks Docker, containers, database role, schema
```

## Profiles

One `compose.yml` serves every profile; `COMPOSE_PROFILES` in `.env` selects
the optional services.

| Profile | Services | Use it for |
| --- | --- | --- |
| `lite` | PostgreSQL, one-shot `migrate`, Python `api` (runs jobs in-process), one-shot `seed` (demo), Go `server` | A laptop, a trial, a small team |
| `standard` | `lite` + Redis, `worker` (scalable), `scheduler`; the API stops running jobs itself | A team: concurrent enrichment, recurring jobs, live workbook updates |
| `full` | `standard` + SearXNG, Reacher, OpenTelemetry Collector | Self-hosted search and email verification, a telemetry sink |

What each profile does *not* give you yet, so nobody is surprised:

- **`lite` is not Python-free.** The RFC's lite profile is a single Go binary and
  PostgreSQL. Today the Go server serves the web UI, the Go-owned `/api/v2`
  routes and the plugin runner, but authentication, workbooks and most routes
  are still Python (milestone M6 moves them). `lite` therefore runs the Python
  API, without Redis, a separate worker or a scheduler.
- **Workbook live cell updates over WebSocket need Redis** (the Python API
  publishes and subscribes through it). `lite` has none; use `standard` if you
  want live updates, or reload the page.
- **Recurring jobs need the scheduler**, which `lite` does not run.
- **There is no browser pool** in `standard` yet; it arrives with the Python
  specialist workers (M4).
- **`full`'s collector receives OTLP but nothing first-party exports to it
  yet.** The endpoint (`OTEL_EXPORTER_OTLP_ENDPOINT`) is injected so
  OpenTelemetry-aware code picks it up; the collector logs what it receives.
  Replace the `debug` exporter in `otel-collector.yaml` to keep the data.
- **Reacher is amd64 only** upstream; on arm64 Docker runs it under emulation.
  Real SMTP probing also needs outbound port 25, which most clouds block.

## `opengtm init`

```text
opengtm init [--dir .] [--profile lite|standard|full] [--yes] [--force]
             [--public-url https://gtm.example.com] [--bind 127.0.0.1] [--port 3000]
             [--project opengtm] [--version X.Y.Z] [--app-image REF] [--server-image REF]
             [--rotate-secrets] [--start]
```

- **Interactive** on a terminal: it asks for the profile, port and public URL.
  `--yes` accepts the defaults and never prompts (for scripts and CI).
- **Generates** the SECRET_KEY, the secrets master key (a Fernet key), the owner
  and runtime database passwords, and the admin password. The runtime login is a
  separate `NOSUPERUSER NOBYPASSRLS` role, so row-level security applies to
  every service.
- **Never overwrites without confirmation.** If `.env`, `opengtm.yaml` or
  `compose.yml` exist it stops with a message and changes nothing. `--force`
  (or answering yes at the prompt) overwrites them after keeping `.bak-<time>`
  copies, and **reuses the existing secrets**, because the PostgreSQL volume
  keeps the password it was created with. `--rotate-secrets` generates new
  ones and warns about exactly that.
- `--public-url` sets `APP_ENV=production` and `CORS_ORIGINS`. The listener stays
  on `127.0.0.1` for your TLS proxy (see the Caddy block in the README). Use
  `--bind 0.0.0.0` only behind a firewall.
- `--project` gives the Compose project a distinct name so several installs
  can share one Docker host.

### Files in the install directory

| File | Purpose |
| --- | --- |
| `.env` (0600) | Secrets, image tags, ports, profile. Edit freely; `opengtm upgrade` changes only the three image lines. |
| `opengtm.yaml` | Non-secret server configuration, validated at startup; environment variables override it. |
| `compose.yml` | The stack. `opengtm upgrade` refreshes it to the version shipped with the new binary unless you edited it (then it writes `compose.yml.new`). |
| `data/` | Bind-mounted application data (workspace metadata, files). Included in backups. |
| `backups/` | Backups. Copy them off the host. |
| `.opengtm/state.json` | Installed release and the upgrade journal. |

### Configuration reference (`opengtm.yaml`)

Every key has an environment override (the same names the Python stack uses),
and unknown keys are an error.

| Key | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `database_url` | `DATABASE_URL` | none (set in `.env`) | `postgresql://` runtime role URL; SQLAlchemy `+psycopg` suffixes are accepted |
| `listen` | `OPENGTM_LISTEN` | `:8080` | Listen address inside the container |
| `legacy_api_url` | `OPENGTM_LEGACY_API_URL` | `http://127.0.0.1:8000` | Python API the front door proxies |
| `web_dir` | `OPENGTM_WEB_DIR` | embedded | Serve the dashboard from a directory |
| `log_level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `worker.concurrency` | `WORKER_CONCURRENCY` | 1 (init writes 2) | Parallel job slots, 1 to 64 |
| `worker.max_active_per_workspace` | `WORKER_MAX_ACTIVE_PER_WORKSPACE` | 2 | 0 disables the cap |
| `worker.shutdown_grace_seconds` | `WORKER_SHUTDOWN_GRACE_SECONDS` | 30 | Drain time on shutdown |
| `plugins.dirs`, `plugins.connector_dirs` | `OPENGTM_PLUGIN_DIRS`, `OPENGTM_CONNECTOR_DIRS` | image defaults | Where plugins and v1 connectors live |
| `plugins.signature_policy` | `CONNECTOR_SIGNATURE_POLICY` | `optional` | `required` loads only signed plugins |
| `plugins.trust_store` | `OPENGTM_PLUGIN_TRUST_STORE` | image default | Trusted publishers file |
| `plugins.egress_proxy` | `OPENGTM_EGRESS_PROXY` | none | HTTP proxy for plugin traffic |

Provider keys, SMTP and the like go in `.env` (see `.env.example` for names) or
in the app's Settings.

## Migrations: who owns the schema

`opengtm migrate` is the single owner of schema changes. The engine is still
Alembic, run from the release's own Python image, and that is a deliberate
decision, not a gap: the schema is 60 Python revisions, and rewriting them in Go
before the Go API owns the schema would add risk and no benefit. What Go owns
today is everything around the engine:

- a **PostgreSQL advisory lock** plus the install lock, so only one migration
  runs at a time across hosts and operators;
- an explicit **plan** (`opengtm migrate --plan`) that changes nothing;
- a refusal to run a release against a database **newer than the release**
  (Alembic would also fail, but after startup work has begun; this fails first
  and says what to do);
- **verification** that the database reached the release head afterwards;
- choosing where the engine runs: the install's `migrate` service
  (`--executor compose`, the default for an install directory) or a local
  checkout or image (`--executor python --database-url ...`).

When the Go API owns the schema, only the engine changes; the commands, locks
and checks stay. Alembic remains the engine for the Python-owned tables until
then. A Python-free `lite` profile is therefore blocked on M6, not on packaging.

```bash
opengtm migrate --dir ~/opengtm --plan          # what would change
opengtm migrate --dir ~/opengtm                 # apply
opengtm migrate --executor python --python "uv run --frozen python" \
  --database-url postgresql://owner:pw@host/db  # from a checkout
```

## Diagnostics

`opengtm doctor --dir ~/opengtm` checks, from the host: Docker and Compose
versions, `.env` permissions, the Compose file, container health, the database
connection, that the runtime role cannot bypass row-level security, the schema
revision, the front door (`/readyz`, and the legacy API through it), and the
age of the newest backup. Inside a container, `opengtm doctor` (no `--dir`)
checks the configuration, database role and schema it is running with.

## Putting it on the internet

Keep the bundled listener on loopback and terminate TLS in front of it, as in
the README's Caddy example. `opengtm init --public-url https://gtm.example.com`
sets production mode. PostgreSQL and Redis are never published on the host. See
the [production checklist](https://opengtm.palash.dev/self-hosting/production/).

## Kubernetes

[`deploy/kubernetes/`](../deploy/kubernetes/) is a Helm-free example (plain
manifests plus a `kustomization.yaml`) for the `lite` and `standard` profiles.
It is validated against the Kubernetes schemas in CI; it has not been run on a
cluster as part of M7, so treat it as a starting point. The constraints that
shape it are architectural: until M8 the API, worker and scheduler share one
ReadWriteOnce data volume and must run on one node, the API stays at one
replica, and there is one scheduler. The Go front door scales freely. Roll out
a release by changing the tags in `kustomization.yaml`, deleting the old
`opengtm-migrate` Job and applying again; take the backup first (see the
runbook, which also explains why rollback is a restore).

## Verifying a release

Every release carries, signed with keyless [Sigstore cosign](https://docs.sigstore.dev/)
from the release workflow's GitHub identity:

- binaries for linux, macOS and Windows on amd64 and arm64, and `checksums.txt`;
- a signature bundle for the checksums (`checksums.txt.sigstore.json`);
- one SPDX SBOM per archive;
- multi-arch images `ghcr.io/debpalash/opengtm` (Python app) and
  `ghcr.io/debpalash/opengtm-server` (Go server), signed by digest, with SBOM
  and provenance attestations;
- `release-manifest.json` (every image digest and archive checksum), also signed.

`scripts/install.sh` always checks the archive against `checksums.txt`, and,
if `cosign` is installed, the signature on `checksums.txt` (`--require-signature`
makes that mandatory). To check by hand:

```bash
V=3.1.0
ID='^https://github.com/debpalash/OpenGTM/\.github/workflows/release\.yml@refs/tags/v'
ISS=https://token.actions.githubusercontent.com
BASE=https://github.com/debpalash/OpenGTM/releases/download/v$V

curl -fsSLO $BASE/checksums.txt -O $BASE/checksums.txt.sigstore.json
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp "$ID" --certificate-oidc-issuer $ISS checksums.txt
sha256sum --ignore-missing -c checksums.txt          # after downloading an archive

cosign verify --certificate-identity-regexp "$ID" --certificate-oidc-issuer $ISS \
  ghcr.io/debpalash/opengtm-server:$V
docker buildx imagetools inspect ghcr.io/debpalash/opengtm-server:$V --format '{{json .SBOM}}'
```

The checksum alone proves the download matches a file from the same origin;
the signature proves it was built by this repository's release workflow on a
version tag.

## Command reference

| Command | Purpose |
| --- | --- |
| `opengtm init` | Create a new install |
| `opengtm upgrade --to X.Y.Z` | Back up, migrate, switch release, health-check; offers rollback |
| `opengtm rollback` | Restore the pre-upgrade backup and the previous release |
| `opengtm backup [create \| verify \| list]` | Back up the database, data directory and config |
| `opengtm restore BACKUP` | Restore a backup (into an empty database, or `--wipe`) |
| `opengtm migrate` | Apply (or `--plan`) schema migrations |
| `opengtm doctor [--dir DIR]` | Diagnose an install or a running container |
| `opengtm serve \| worker \| routes \| plugin` | Runtime roles and plugin tooling (see `apps/server/README.md`) |
