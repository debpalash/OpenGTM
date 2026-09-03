---
title: Your first workbook
description: Source companies, add an email waterfall, check the spend estimate, run, and push the result to a webhook.
sidebar:
  order: 4
---

A workbook is a spreadsheet whose columns do work. This walkthrough builds one
from scratch with the REST API so every step is reproducible; the web UI does
the same things with buttons.

## 1. Log in

```bash
export OPENGTM=http://localhost:8000
TOKEN=$(curl -s -X POST $OPENGTM/auth/token \
  -d 'username=admin&password=admin' | jq -r .access_token)
AUTH="Authorization: Bearer $TOKEN"
```

The seed admin is `admin` / `admin` unless you changed `SEED_ADMIN_PASSWORD`.
Requests are scoped to your active workspace; add `X-Workspace-Id: <id>` to
target another one you belong to.

## 2. Create a workbook

```bash
WB=$(curl -s -X POST $OPENGTM/api/workbooks -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"name": "IT services — Pune"}' | jq -r .id)
```

## 3. Add a source column

A `source` column materialises new rows from the sourcing engine using an
ideal-customer-profile (ICP) description:

```bash
curl -s -X POST $OPENGTM/api/workbooks/$WB/sources -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{
    "name": "Companies",
    "icp": { "description": "IT services companies in Pune, 20-200 employees" },
    "channels": { "categories": ["directory", "review"] },
    "target_rows": 50
  }'
```

Run it with `POST /api/workbooks/{id}/sources/{column_id}/run`. The run is
a durable job: it pages through sources, checkpoints its cursor, deduplicates
by provider record id, and reports whether the requested target was met.

## 4. Add an email waterfall

```bash
curl -s -X POST $OPENGTM/api/workbooks/$WB/columns -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{
    "name": "Email",
    "type": "waterfall",
    "config": { "target_field": "email" }
  }'
```

Leaving `waterfall` empty applies the default chain for `email`: free scrapers
and search first (`deep_scraper`, `jsonld_firmographics`, `website_scraper`,
`email_harvester`, `ddg_email`, `mailscout`), then paid providers you have keys
for (`hunter_io`, `apollo_io`, `snovio`, `prospeo`). The planner reorders the
chain by learned hit-rate per dollar. See [Waterfalls](/concepts/waterfalls/).

## 5. Check the bill

```bash
curl -s $OPENGTM/api/workbooks/$WB/run/estimate -H "$AUTH" | jq
```

The response prices the run as worst case (every paid provider tried per row)
and best case (cheapest paid hit first), with a per-column breakdown. With no
paid keys configured both numbers are zero. Set a ceiling if you want one:

```bash
curl -s -X PUT $OPENGTM/api/workbooks/$WB/budget -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"max_usd": 5}'
```

## 6. Run

```bash
curl -s -X POST $OPENGTM/api/workbooks/$WB/run -H "$AUTH"
```

The worker fills cells and publishes progress over a workspace-scoped Redis
channel; the UI subscribes through the SSE events endpoint. Every cell keeps a
trace of which providers were tried, at what cost, and why the chain stopped:
`GET /api/workbooks/{id}/rows/{lead_id}/cells/{column_id}/trace`.

## 7. Push rows somewhere

Add an `output` column. It runs once per row after enrichment and is
idempotent:

```bash
curl -s -X POST $OPENGTM/api/workbooks/$WB/columns -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{
    "name": "Notify",
    "type": "output",
    "config": {
      "destination": "webhook",
      "destination_config": {
        "url": "https://hooks.example.com/opengtm",
        "headers": { "X-Source": "opengtm" }
      }
    }
  }'
```

Swap `webhook` for `crm` (HubSpot or Salesforce), `sheets`, `airtable`,
`sequencer`, `instantly` or `smartlead`; see [Outputs](/guides/outputs/).

## What next

- Do the same from chat: "build a list of 50 IT staffing firms in Pune and
  find their founders' emails" drafts a plan you approve before it runs. See
  [Chat and autopilot](/guides/chat/).
- Start from a recipe in the [template gallery](/guides/templates/).
- Add [buying signals](/guides/signals/) and [automations](/guides/automations/)
  so the list keeps itself fresh.
