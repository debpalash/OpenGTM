---
title: Quickstart (Docker)
description: Bring up the full OpenGTM stack with Docker Compose in a few minutes.
sidebar:
  order: 2
---

The installer generates unique secrets and prepares a writable data directory.
You can explore the zero-key demo before adding any provider keys.

## Requirements

- Git and Docker with the Compose v2 plugin (`docker compose version`)
- 2 CPU and about 4 GB RAM for a comfortable single-node install
- An LLM key only if you want AI, research or agent features; add provider keys
  under **Settings → API Keys** when you need them.

## Laptop

macOS or Linux:

```bash
git clone https://github.com/debpalash/OpenGTM.git
cd OpenGTM
./scripts/install.sh
```

Windows PowerShell with Docker Desktop:

```powershell
git clone https://github.com/debpalash/OpenGTM.git
Set-Location OpenGTM
.\scripts\install.ps1
```

Open **http://localhost:3000** and sign in with the generated admin password
from `.opengtm-initial-credentials`. The file is ignored by Git; don't share it.
Change the password after signing in and delete the credentials file. The
installer leaves an existing `.env` untouched.

## Server

Use a pinned release and serve it behind a TLS reverse proxy. On Linux, point
your domain at the host first, then:

```bash
git clone --depth 1 --branch v3.0.0 https://github.com/debpalash/OpenGTM.git
cd OpenGTM
./scripts/install.sh --no-start
DOMAIN=gtm.example.com                 # replace with your domain
cat >> .env <<EOF
APP_ENV=production
PORT=127.0.0.1:3000
CORS_ORIGINS=https://$DOMAIN
YUPCHA_IMAGE=ghcr.io/debpalash/opengtm:3.0.0
EOF
docker compose pull
docker compose up -d --no-build
```

For Caddy, point the domain to the server and allow ports 80 and 443:

```text
gtm.example.com {
    reverse_proxy 127.0.0.1:3000
}
```

Replace the sample domain in both places. The generated secrets satisfy
production startup checks; the HTTP service listens on loopback so only your
TLS proxy is exposed. Keep `.env`, `data/`, and Docker volumes on upgrades.
For a newer release, fetch and check out its tag, change `YUPCHA_IMAGE` in
`.env` to the same version, then run `docker compose pull` and
`docker compose up -d --no-build`. Back up both storage planes and complete the
[production checklist](/self-hosting/production/) before inviting a team.

## Start a chat from the sidebar

Select the **New chat** icon beside **Chat** to open an inline prompt. Enter
your idea and press Enter to carry it into a new chat's composer; review or edit
it there before sending. Escape closes the prompt. In the collapsed sidebar,
the icon sits directly below Chat and expands the sidebar when selected. The
same prompt works in the mobile sidebar.

## Attention and appearance

The bell in the top toolbar shows recent items that need attention: **critical**
failed tasks or workbooks and **urgent** unread high-intent signals. Open it for
a short preview, or choose **View all notifications** to filter and follow items
in the full attention center. The count covers recent activity, not a lifetime
unread total. Select **Appearance** at the bottom of the left sidebar to switch
between light, dark and system themes; the same control works in the collapsed
or mobile sidebar.

## What just started

| Service | Purpose |
|---|---|
| `postgres` | Primary data plane: users, workbooks, jobs, cells. RLS-protected. |
| `redis` | Live progress channels for the UI and bounded reconnect history. |
| `migrate` | One-shot Alembic `upgrade head` with the schema-owner role. |
| `api` | FastAPI: enqueue and query only; it never runs user work inline. |
| `worker` | Claims jobs from the durable PostgreSQL queue and runs enrichment, imports, refreshes. |
| `scheduler` | Recovers stale jobs and enqueues recurring work. Run exactly one. |
| `nginx` | Serves the React bundle and proxies `/api` on port 3000. |

Scale enrichment throughput with more workers:

```bash
docker compose up --scale worker=4
```

## Using an existing database

Set `DATABASE_URL` in `.env` to point at your own PostgreSQL (the URL must use
the `postgresql+psycopg://` scheme). Compose also supports a separate,
non-superuser `APP_DATABASE_URL` for API and worker traffic so forced
row-level security is effective. SQLite is supported for local development
only; it has no concurrent writers.

## Next

- [Your first workbook](/getting-started/first-workbook/)
- [Configuration reference](/self-hosting/configuration/) for every environment
  variable
- [Production checklist](/self-hosting/production/) before exposing the install
  to a network
