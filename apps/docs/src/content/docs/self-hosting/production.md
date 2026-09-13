---
title: Production checklist
description: What to change before exposing an OpenGTM install to a network you do not fully control.
sidebar:
  order: 3
---

The Compose defaults are tuned for a laptop. Work through this list before an
install serves a team.

## Must do

- **Set `SECRET_KEY`.** The JWT signing key. The app refuses to boot on the
  insecure default whenever `APP_ENV` is anything other than `dev`, `test` or
  `local`. Generate one:

  ```bash
  python -c "import secrets; print(secrets.token_urlsafe(48))"
  ```

- **Set `APP_ENV=production`.** This turns the safety checks above into hard
  failures and disables development fallbacks such as `create_all` schema
  initialisation.
- **Change the seed admin password.** `SEED_ADMIN_PASSWORD` defaults to
  `admin`. Set it before the first boot or reset it immediately afterwards.
- **Change the database passwords.** `POSTGRES_PASSWORD` and
  `YUPCHA_RUNTIME_DB_PASSWORD` default to well-known values. Keep the schema
  owner (`DATABASE_URL`) and the runtime role (`APP_DATABASE_URL`) separate so
  forced row-level security applies to API and worker traffic.
- **Put TLS in front.** The bundled nginx listens on plain HTTP on port 3000.
  Terminate TLS with your reverse proxy of choice and forward to it.
- **Back up both storage planes.** PostgreSQL holds the data plane, and the
  `data/` volume holds the workspace control plane (`workspaces.db`, encrypted
  workspace secrets, collection ledgers). The built-in backup command captures
  both, snapshots live SQLite files through SQLite's backup API, and writes a
  SHA-256 inventory:

  ```bash
  uv run python cli.py backup --output /secure/opengtm-$(date +%F).tar.gz
  uv run python cli.py backup-verify /secure/opengtm-$(date +%F).tar.gz
  ```

  The command invokes `pg_dump`, which must be installed and compatible with
  the PostgreSQL server. The archive includes encrypted workspace secrets but
  is not itself encrypted: store it in encrypted, access-controlled storage.

## Restore drill

Run this against a disposable PostgreSQL database and a nonexistent or empty
data directory. `pg_restore --clean --if-exists` replaces objects in the target
database, so the exact confirmation phrase is mandatory:

```bash
uv run python cli.py restore /secure/opengtm-2026-09-13.tar.gz \
  --database-url postgresql://restore_user:password@restore-db/opengtm_drill \
  --data-dir /tmp/opengtm-restore-drill \
  --confirm "RESTORE OPENGTM BACKUP"
```

After restoration, point a temporary API instance at the restored database and
data directory, run migrations only if restoring into a newer application
version, then verify login, workspace membership, workbook row counts, and one
read-only export. Record the archive SHA-256 printed by `backup-verify`, the
application version, elapsed recovery time, and drill date in your operations
log. Never run the drill against production credentials.

## Should do

- **Run exactly one scheduler.** Scale `worker` freely; do not scale
  `scheduler` until leadership election exists.
- **Keep PostgreSQL and Redis private.** Compose binds them to loopback for
  host administration; do not publish them on a LAN interface.
- **Pin an image tag.** Deploy from a tagged GHCR image
  (`ghcr.io/debpalash/opengtm:<version>`) rather than `latest`, and run
  migrations once with the owner role before starting the runtime services.
- **Rotate provider keys per workspace.** Keys entered in
  **Settings → API Keys** are encrypted per workspace and never placed in queue
  payloads; prefer them over global `.env` keys when more than one team shares
  the install.
- **Set a spend ceiling** on workbooks that use paid providers, so a runaway
  refresh cannot exhaust a vendor budget.

## Before hosting mutually untrusted tenants

OpenGTM's tenant isolation is PostgreSQL RLS plus application-level checks,
which is a strong boundary for one organisation's workspaces. Before offering
it to strangers as a service, the [architecture notes](/reference/architecture/)
list what still has to move: a controlled egress proxy for outbound fetches,
managed key storage for the workspace secret encryption key, controlled-live
SSO validation, scheduled restore drills, and an external security review. Offering OpenGTM as a
network service also triggers the AGPL source-availability clause; see
[License](/community/license/).
