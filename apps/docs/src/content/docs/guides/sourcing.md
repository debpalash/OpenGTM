---
title: Sourcing leads
description: The 90-source registry, ICP queries, durable connector runs, and source health.
sidebar:
  order: 1
---

A `source` column turns an ideal-customer-profile (ICP) description into rows.

## The registry

The registry holds about 90 discovery sources, each with a region
(`global`, `india`, `us`, `eu`), a category, query templates and a priority.
Categories: `directory`, `startup`, `saas_directory`, `review`, `social`,
`news`, `jobs`, `government`, `freelance`, `developer`, `ecommerce`.
Representative ids: `indiamart`, `justdial`, `tradeindia`, `ambitionbox`,
`naukri`, `zaubacorp`, `clutch`, `g2`, `crunchbase`, `angellist`,
`linkedin_companies`, `indeed`, `yelp`, `thomasnet`, `capterra`,
`producthunt`, `github_orgs`, `hackernews`, `trustpilot`, `upwork`,
`europages`, `kompass`.

The summary (totals by region and category) is available from the settings
API and shown in **Settings → Sources**.

## Source column config

```json
{
  "icp": {
    "description": "IT services companies in Pune, 20-200 employees",
    "industry": "IT services",
    "geo": "Pune",
    "keywords_any": ["staffing", "consulting"],
    "exclude": ["freelancer"]
  },
  "channels": {
    "categories": ["directory", "review"],
    "regions": ["india"],
    "explicit_sources": ["clutch", "justdial"]
  },
  "target_rows": 50
}
```

The free-text `description` wins when present; otherwise the query is built
from industry, keywords and geo. `target_rows` of 0 means unlimited.

Two nested kinds reroute the column:

- `source.kind: crm_import` pulls from a connected CRM (gated by
  `CRM_IMPORT_ENABLED`).
- `source.kind: people_search` sources people instead of companies (gated by
  `PEOPLE_SEARCH_SOURCE_ENABLED`).

## Endpoints

| Endpoint | Purpose |
|---|---|
| `POST /api/workbooks/{id}/sources` | Add a source column |
| `POST /api/workbooks/{id}/sources/{column_id}/run` | Start a durable run (20/min) |
| `GET /api/workbooks/{id}/sources/{column_id}/preview` | Preview without writing rows |

## Durable connector runs

A run pages through sources, normalises records, and commits each page with its
cursor. Rows carry a stable provider + record identity so replays are
idempotent, and only one active run per workbook/connector pair is allowed.
The run record distinguishes requested, fetched, added, updated, skipped,
target-met, exhausted and failed, so "100 requested" is never reported as done
after 50 arrived.

## Other ways in

- **CSV import** with schema-aware column mapping
  (`POST /api/workbooks/{id}/import`).
- **Ingest API** for pushes from scripts, the Chrome extension or n8n. See
  [Ingest API](/guides/ingest-api/).
- **Chat**: "find 50 D2C brands on Shopify in Bangalore" creates the source
  workbook for you after confirmation.

## Reliability and health (opt-in)

- `SOURCE_RELIABILITY_RANKING=1` lets learned yield swing a source's priority
  by up to ±4 points after 5 samples.
- `SOURCE_HEALTH_ENABLED=1` probes sources on a schedule and marks them
  degraded after 3 failures, disabled after 6, recovered after 2 successes;
  `SOURCE_HEALTH_ENFORCE=1` actually skips disabled sources.

All fetches go through the SSRF guard and, where enabled, a proxy list or
proxy manager (`PROXY_LIST`, `PROXY_MANAGER_URL`).
