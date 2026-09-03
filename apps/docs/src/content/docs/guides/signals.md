---
title: Buying signals and watches
description: The signal feed, the scan job, and scheduled watches that poll funding, hiring, news, tech changes and job changes.
sidebar:
  order: 7
---

## Signal feed

Signals are events attached to leads, each with a type and a weight:

| Type | Weight |
|---|---|
| `funding` | 10 |
| `job_change` | 9 |
| `hiring` | 8 |
| `growth` | 7 |
| `tech_change` | 6 |
| `news` | 5 |
| `website_change` | 4 |
| `social_activity` | 3 |

| Endpoint | Purpose |
|---|---|
| `GET /api/signals?signal_type=&lead_id=&limit=&offset=` | Feed with counts |
| `POST /api/signals/scan` | Admin: enqueue a scan of hot and warm leads on the worker |
| `POST /api/signals/mark-read`, `GET /api/signals/counts` | Inbox housekeeping |

Scan limits: `SIGNAL_SCAN_MAX_HOT_LEADS` (50), `SIGNAL_SCAN_MAX_WARM_LEADS`
(30), `SIGNAL_SCAN_MAX_LEADS_PER_WORKSPACE` (20), and a global cap of 500 per
scan.

## Watches (intent poller)

A watch polls a target on a schedule and emits signals. Enable with
`INTENT_POLLER_ENABLED=1` (requires the PostgreSQL lead store).

| Kind | Target | Emits |
|---|---|---|
| `funding` | company | `company_funded`, `executive_hired` (SEC Form D filings and appointments) |
| `hiring` | company | `hiring_surge`, `new_tech_adopted` (job postings) |
| `feed` | RSS URL | `news` (ETag / Last-Modified / GUID cursor) |
| `company` | company | all of the above |
| `job_change` | list of contacts in `config.contacts` (max 500, weekly) | `job_change` |
| `account_group` | a saved account group | `partnership_hiring`, `leadership_change`, `funding`, `pricing_page_change` |

Intervals are `hourly`, `daily` or `weekly`.

```bash
curl -X POST $OPENGTM/api/watches -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "kind": "company",
  "target": "acme.com",
  "lead_id": 42,
  "interval": "daily",
  "create_webhook_rule": true,
  "webhook_url": "https://hooks.example.com/signals"
}'
```

`create_webhook_rule` also creates an automation that posts each emitted
signal to `webhook_url`. Other endpoints: `GET /api/watches`,
`GET/PATCH/DELETE /api/watches/{id}`, `POST /api/watches/{id}/poll` (poll
now, quota `INTENT_POLLER_POLL_NOW_DAILY_QUOTA`), and
`GET /api/watches/{id}/signals`.

The first poll records state without emitting, unless
`INTENT_POLLER_BACKFILL=1`. Budgets: `INTENT_POLLER_MAX_WATCHES_PER_WS` (200),
`INTENT_POLLER_DAILY_POLL_BUDGET` (0 = unlimited), and a watch is disabled
after `INTENT_POLLER_MAX_CONSECUTIVE_FAILURES` (12).

## Tracking accounts from chat

The `track_account_signals` action creates watches for exact saved accounts
after confirmation, and `add_signal_trigger` wires an automation to react to
them. See [Chat and autopilot](/guides/chat/).
