---
title: n8n community node
description: Source, enrich, verify, score and dedupe leads from n8n workflows.
sidebar:
  order: 2
---

`packages/n8n-nodes-yupcha` ships an [n8n](https://n8n.io) community node
(`n8n-nodes-opengtm`) that calls your self-hosted OpenGTM instance. It is MIT
licensed so it can be published to the n8n registry; the rest of OpenGTM is
AGPLv3.

## Install

From the package directory:

```bash
cd packages/n8n-nodes-yupcha
bun install
bun run build          # compiles to dist/
```

Then install it into n8n as a community node (**Settings → Community nodes →
Install**) pointing at the built package, or link it into your n8n custom
nodes directory. Restart n8n.

## Credentials

Create an **OpenGTM API** credential with:

| Field | Value |
|---|---|
| Instance URL | Your OpenGTM base URL, e.g. `https://opengtm.example.com` (default `http://localhost:8000`) |
| API Key | A bearer token from **Settings → API Keys** in OpenGTM |

The node sends `Authorization: Bearer <API Key>` on every request.

## Operations

| Operation | Endpoint | Inputs | Returns |
|---|---|---|---|
| Source Leads | `POST /api/collect` | Search query | A durable collection job (`job_id`); poll `GET /api/jobs/{id}` and read `GET /api/jobs/{id}/leads` |
| Enrich Company | `POST /api/lead` then `POST /api/leads/bulk-enrich` | Company name, domain | `lead_id` plus the background enrichment `job_id` |
| Verify Email | `POST /api/leads/verify-email` | Email address | `status` (`valid`, `invalid`, `catch_all`, `unknown`), confidence, source |
| Score Lead | `POST /api/leads/score` | Lead record as JSON | `score` 0-100 and `tier` |
| Get Tech Stack | `POST /api/leads/tech-stack` | Domain | Detected technologies; `409` until `TECH_STACK_WEBSITE_FETCH_ENABLED=1` |
| Domain Intelligence | `POST /api/leads/domain-intel` | Domain | RDAP and DNS analysis |
| Find Duplicates | `POST /api/leads/dedup` | none | Duplicate groups |

Each incoming item runs one request; the JSON response becomes the output item.
With **Continue on fail** enabled, errors are emitted as `{ error }` items
instead of stopping the workflow.

The exact request and response shapes for every endpoint are in the
[REST API reference](/api/).

## Beyond the node

Anything the node does not cover is a plain HTTP Request node away: every
workbook, outreach, signal and automation endpoint is documented in the
[API reference](/api/) and works with the same bearer token. Inbound rows can
also be pushed with a per-workbook ingest token; see
[Ingest API](/guides/ingest-api/).
