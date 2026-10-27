---
title: Quickstart (Docker)
description: Install OpenGTM locally or on a server with Docker Compose. The installer creates secrets; server steps pin a GHCR image and add TLS.
sidebar:
  order: 2
---

The installer generates unique secrets (database, signing key, service tokens).
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

Open **http://localhost:3000**. The first visit goes to `/setup`, where you
create the admin account. Enter the one-time setup token the API logs at start
(`docker compose logs api-go | grep -i 'setup token'`), or set `SETUP_TOKEN` in
`.env` first. There is no default admin account, and setup closes once an admin
exists. The installer leaves an existing `.env` untouched.

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
EOF
docker compose up -d --build
```

For Caddy, point the domain to the server and allow ports 80 and 443:

```text
gtm.example.com {
    reverse_proxy 127.0.0.1:3000
}
```

Replace the sample domain in both places. The generated secrets satisfy
production startup checks; the HTTP service listens on loopback so only your
TLS proxy is exposed. Keep `.env` and the Docker volumes on upgrades.
For a newer release, fetch and check out its tag, then run
`docker compose up -d --build` (the `migrate` service applies schema changes first). Back up both storage planes and complete the
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
| `migrate` | One-shot Go schema runner with the schema-owner role. |
| `api-go` | Go API and the embedded web UI: enqueue and query only; it never runs user work inline. |
| `worker-go` | Claims jobs from the durable PostgreSQL queue and runs enrichment, imports, refreshes. |
| `scheduler-go` | Enqueues recurring work. Extra replicas idle (advisory lock). |
| `browser` | Internal browser/scraper service, reachable only from the Go services. |
| `nginx` | Public entrypoint on port 3000; proxies everything to `api-go`. |

Scale enrichment throughput with more workers:

```bash
docker compose up -d --scale worker-go=4
```

## Using an existing database

Set `DATABASE_URL` in `.env` to the owner URL of your own PostgreSQL (used by
`migrate`) and `GO_DATABASE_URL` to a non-superuser runtime login for the Go
services, so forced row-level security is effective. Both are plain
`postgresql://` URLs. PostgreSQL is required.

## Next

- [Your first workbook](/getting-started/first-workbook/)
- [Configuration reference](/self-hosting/configuration/) for every environment
  variable
- [Production checklist](/self-hosting/production/) before exposing the install
  to a network
