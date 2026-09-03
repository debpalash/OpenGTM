---
title: Automations
description: Trigger rules that re-enrich rows, push to a CRM, or call a webhook when a signal fires, a row changes, or a schedule ticks.
sidebar:
  order: 8
---

Automations are feature-flagged: set `AUTOMATIONS_ENABLED=1`. Every rule is
workspace-scoped, budgeted, idempotent and audited.

## A rule

```json
{
  "name": "Re-enrich on funding",
  "trigger_type": "on_signal",
  "trigger_config": { "signal_types": ["funding"] },
  "condition": "{score} >= 60",
  "actions": [
    { "type": "re_enrich", "config": { "column_ids": ["email", "decision_makers"] } },
    { "type": "push_crm", "config": { "crm_type": "hubspot", "field_map": { "email": "email", "company": "company" } } },
    { "type": "webhook", "config": { "url": "https://hooks.example.com/funded", "header_secret_ref": "HOOK_SECRET" } }
  ],
  "scope_workbook_ids": ["wb_123"],
  "stop_on_error": false,
  "max_spend_usd_per_day": 2.0,
  "max_actions_per_day": 200,
  "enabled": true
}
```

Triggers: `on_signal`, `on_row_changed`, `on_row_added`, `on_schedule`
(`hourly`, `daily`, `weekly`).

Actions: `re_enrich`, `push_crm`, `webhook`. The legacy `sequencer` and
`send_email` actions return `409 legacy_outreach_disabled` unless
`AUTOMATIONS_ALLOW_LEGACY_OUTREACH=1`, and `send_email` additionally needs
configured SMTP and an `OUTREACH_FOOTER` workspace secret.

Validation at create time: `re_enrich` column ids must exist in the scoped
workbooks; webhook URLs must be http(s) without credentials, and
`header_secret_ref` must name a workspace secret; conditions are
parse-checked against an adversarial row. Webhooks execute with DNS pinned at
call time and redirects disabled.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `POST/GET /api/automations/triggers` | Create, list |
| `GET/PATCH/DELETE /api/automations/triggers/{id}` | Read, update, delete |
| `POST /api/automations/triggers/{id}/pause`, `/resume` | Toggle |
| `POST /api/automations/triggers/{id}/preview` with `{row_ids, limit}` | Show which rows match |
| `POST /api/automations/triggers/{id}/run` with `{row_ids, dry_run}` | Run now |
| `GET /api/automations/triggers/{id}/runs`, `GET /api/automations/runs/{run_id}` | Ledger |

## Limits

| Setting | Default |
|---|---|
| `AUTOMATIONS_MAX_RULES_PER_WS` | 50 |
| `AUTOMATIONS_MAX_ACTIONS_PER_RULE` | 10 |
| `AUTOMATIONS_MAX_ROWS_PER_EVAL` | 500 |
| `AUTOMATIONS_GLOBAL_DAILY_USD` | 0 (unlimited) |
| `AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST` | empty (any public host) |

Each action result carries a unique `(workspace_id, idempotency_key)`, so a
retried run never repeats a push. Daily spend and action caps are reserved
before execution.
