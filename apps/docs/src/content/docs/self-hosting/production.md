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
- **Back up the data volume.** PostgreSQL holds the data plane, and the
  `data/` volume holds the workspace control plane (`workspaces.db`, encrypted
  workspace secrets, collection ledgers). Both need to be in the backup.

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
managed key storage for the workspace secret encryption key, SSO and audit
export, restore drills, and an external security review. Offering OpenGTM as a
network service also triggers the AGPL source-availability clause; see
[License](/community/license/).
