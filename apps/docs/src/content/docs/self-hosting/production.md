---
title: Production checklist
description: What to change before exposing an OpenGTM install to a network you do not fully control.
sidebar:
  order: 3
---

The Compose defaults are tuned for a laptop. Work through this list before an
install serves a team.

## Must do

- **Set `SECRET_KEY`, `INTERNAL_SERVICE_TOKEN` and `BROWSER_SERVICE_TOKEN`.**
  `SECRET_KEY` is the JWT signing key (at least 32 bytes); the Go services
  refuse to boot without it unless `APP_ENV=test`. The two tokens (at least 32
  characters) authenticate service-to-service calls and the internal browser
  service. `scripts/install.sh` generates all three; by hand:

  ```bash
  openssl rand -hex 48   # SECRET_KEY
  openssl rand -hex 32   # INTERNAL_SERVICE_TOKEN, BROWSER_SERVICE_TOKEN
  ```

- **Set `APP_ENV=production`.** This turns the safety checks above into hard
  failures and disables development fallbacks.
- **Create the admin through `/setup`.** The default stack has no seeded admin.
  Open `/setup` right after the first start, using the setup token from
  `docker compose logs api-go | grep -i 'setup token'` (or a `SETUP_TOKEN` you
  set).
- **Change the database passwords.** `POSTGRES_PASSWORD` and
  `YUPCHA_RUNTIME_DB_PASSWORD` default to well-known values. Keep the schema
  owner (`DATABASE_URL`, used only by `migrate`) and the runtime role (used by
  the Go services) separate so forced row-level security applies to all runtime
  traffic.
- **Put TLS in front.** The bundled nginx listens on plain HTTP on port 3000.
  Terminate TLS with your reverse proxy of choice and forward to it.
- **Back up PostgreSQL.** All application state, including encrypted
  workspace secrets, lives in PostgreSQL (Redis holds only live progress). Use
  `pg_dump` with the owner role and store the dump in encrypted,
  access-controlled storage:

  ```bash
  docker compose exec -T postgres pg_dump -U yupcha -Fc yupcha > /secure/opengtm-$(date +%F).dump
  ```

  Keep `SECRET_KEY` and `SECRETS_MASTER_KEY` (if set) with the backup: without
  them the workspace secrets cannot be decrypted.

## Restore drill

Restore into a disposable database, never with production credentials:

```bash
pg_restore --clean --if-exists --no-owner \
  -d postgresql://restore_user:password@restore-db/opengtm_drill /secure/opengtm-2026-09-13.dump
```

Point a temporary stack at the restored database (run `migrate` only when
restoring into a newer application version), then verify login, workspace
membership, workbook row counts and one read-only export. Record the dump's
SHA-256, the application version, elapsed recovery time and drill date in your
operations log.

## Should do

- **Scale workers, not the scheduler.** Scale `worker-go` freely. Extra
  `scheduler-go` replicas are safe (a Postgres advisory lock keeps one active)
  but add nothing.
- **Keep PostgreSQL and Redis private.** Compose binds them to loopback for
  host administration; do not publish them on a LAN interface.
- **Pin a release.** Deploy from a release tag (or the tagged
  `ghcr.io/debpalash/opengtm-*` images) rather than `main` or `latest`; the
  `migrate` service applies schema changes with the owner role before the
  runtime services start.
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
controlled-live Vault Transit and SSO validation, scheduled restore drills, and
an external security review. Offering OpenGTM as a
network service also triggers the AGPL source-availability clause; see
[License](/community/license/).
