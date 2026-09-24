---
title: How OpenGTM works
description: OpenGTM combines self-hosted lead sourcing, provider waterfalls, cited research, and spend limits. See what self-hosting does and does not cover.
sidebar:
  order: 1
---

OpenGTM is an open-source workspace for building prospect lists on infrastructure
you operate. A workbook holds sourced rows, enrichment columns, research
citations, and output actions. See [OpenGTM vs. Clay](/compare/clay-alternative/)
for the managed-service trade-offs.

## Who uses it

- **GTM operators:** Source and enrich companies and people in a workbook.
- **RevOps engineers:** Connect workbooks to the REST API, webhooks, n8n, or MCP.
- **Teams running their own software:** Host the app and manage their own
  deployment, data, and keys.

## What you control

- **Spend:** Review best- and worst-case estimates before a run. Set a workbook
  ceiling for paid provider calls. See [spend transparency](/concepts/spend/).
- **Provider access:** Add your own keys per workspace. External enrichment and
  AI providers still receive the fields you send them.
- **Deployment:** You manage upgrades, backups, and TLS. Work through the
  [production checklist](/self-hosting/production/) before serving a team.

## What you can do

| Task | In OpenGTM |
|---|---|
| Build a list | Source companies and people, import CSV, or ingest rows through the API |
| Fill missing fields | Run cost-aware provider waterfalls across workbook columns |
| Research a company | Store cited answers and the URLs fetched for each row |
| Act on results | Send rows to a CRM, webhook, Sheets, Airtable, or sequence |
| Automate repeat work | Use scheduled watches, signals, Chat, or the API |

## Start

- [Install with Docker Compose](/getting-started/quickstart/) to create credentials
  and open the seeded demo.
- [Build your first workbook](/getting-started/first-workbook/) with sources,
  enrichment, and an output.
- [Use the REST API](/api/) from a script or integration.
