---
title: Template gallery
description: Nineteen validated workbook recipes you can instantiate in one call, plus reusable column chains.
sidebar:
  order: 11
---

## Recipes

Recipes are complete workbook definitions (sources, waterfalls, AI and research
columns, outputs) validated at load time against the live column types,
provider registry, source registry and lead fields. A malformed recipe fails
the test suite, not your workbook.

| Endpoint | Purpose |
|---|---|
| `GET /api/templates/gallery?category=` | Summaries and categories |
| `GET /api/templates/gallery/{slug}` | Full definition |
| `POST /api/templates/gallery/{slug}/instantiate` with optional `{name}` | Create a workbook (editor or admin) |

Categories: `list-building`, `crm-hygiene`, `outbound`, `research`, `signals`.

Shipped recipes: `backfill-missing-fields`, `cold-email-personalization`,
`competitor-battlecards`, `crm-record-enrichment`, `d2c-brands-shopify`,
`domain-normalize-dedupe`, `email-verify-cleanup`, `enrich-and-sync-hubspot`,
`founder-outreach`, `funding-round-watch`, `hiring-signals-radar`,
`india-startup-market-map`, `it-services-india-directory`,
`job-board-intent-india`, `pre-call-account-briefs`,
`saas-buyers-review-sites`, `saas-founders-india`, `tech-stack-mapping`,
`webhook-export-pipeline`.

Recipes may only use the `webhook`, `crm` and `sequencer` output destinations.

## Legacy templates

`GET /api/templates` and `POST /api/templates/{id}/create` build a workbook
from a lead-field view (categories `sales`, `recruiting`, `research`,
`agency`, `signals`), copying any enrichment columns so the result is
immediately runnable.

## Functions

A function is a reusable column chain. Global admins create them
(`POST /api/functions`) and anyone with the editor role applies one to a
workbook (`POST /api/functions/{id}/apply` with `{workbook_id}`). The catalog
is global rather than per workspace, one of the few remaining admin-only
utilities noted on the roadmap.
