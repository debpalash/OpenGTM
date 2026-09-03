---
title: Introduction
description: What OpenGTM is, who it is for, and the promises it makes.
sidebar:
  order: 1
---

OpenGTM is a free, open-source, self-hostable alternative to
[Clay](https://clay.com), licensed under the AGPLv3.

It is a spreadsheet-shaped enrichment engine (**workbooks**) plus an agentic
layer that can build and run those workbooks for you. You source leads, run
enrichment **waterfalls** across providers, research rows with AI agents that
browse the web and cite sources, and push the results to a CRM, Google Sheets,
Airtable, a webhook, or an email sequencer.

Everything runs on your own infrastructure with your own provider keys, and the
bill is shown to you **before** a run executes.

## Who it is for

- **Founders and small sales teams** who want Clay-style enrichment without
  seat pricing or credit markups.
- **RevOps and growth engineers** who automate from a terminal, n8n, or an
  agent, and need an API that is never plan-gated.
- **Agencies and privacy-conscious teams** who cannot send prospect data to a
  third-party SaaS.

## Promises, in writing

These are structural commitments, not marketing:

- **The REST API, webhooks, and MCP tools are never plan-gated.** Automating
  OpenGTM is a first-class use, not an upsell.
- **BYOK at direct cost, zero markup.** You pay the LLM and enrichment vendors
  directly with your own keys.
- **See the bill before you run.** The spend estimate and per-provider cost
  ledger are core features.
- **Self-host is fully functional, forever.** No feature is held back to force a
  cloud upgrade.

## What is in the box

| Capability | Summary |
|---|---|
| Lead sourcing | ~90 discovery sources plus typed connectors with durable, resumable runs |
| Enrichment waterfalls | Cost-ordered provider chains with caching and confidence early-exit |
| Providers | ~35 enrichment providers (email find/verify, phone, firmographics, decision-makers, tech stack, hiring, IP/domain, scoring) |
| AI, research and agent columns | LLM transforms, web-browsing research with citations, goal-directed agents with reasoning traces |
| Agentic chat / autopilot | Turn a goal into a plan and a running workbook |
| Outputs | Webhook, HubSpot, Salesforce, Google Sheets, Airtable, email sequencer |
| Outreach | Multi-step SMTP sequences with bounce and complaint ingestion |
| Buying signals | Hiring, funding, tech change, website change, news, growth, social |
| Multi-tenancy | Workspace roles plus fail-closed PostgreSQL row-level security |
| Integrations | MCP server, n8n community node, Chrome capture extension |

## Where to go next

- [Quickstart](/getting-started/quickstart/) — the whole stack with one
  `docker compose up`.
- [Your first workbook](/getting-started/first-workbook/) — source, enrich and
  push in ten minutes.
- [REST API reference](/api/) — every endpoint, generated from the running app.
