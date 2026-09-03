---
title: Ingest API
description: Push rows into a workbook from scripts, the Chrome extension or n8n with a per-workbook token.
sidebar:
  order: 10
---

The ingest API accepts rows for a workbook without a user session. It is
feature-flagged: set `INGEST_API_ENABLED=1`.

## Create a token

```bash
curl -X POST $OPENGTM/api/v2/workbooks/$WB/ingest-token -H "$AUTH"
```

Requires the editor or admin role. The response contains the plaintext token
(`wbi_…`) **once**; only its hash is stored. Calling the endpoint again
rotates the token and revokes the previous one in the same transaction, so
exactly one token is live per workbook.

## Push rows

```bash
curl -X POST $OPENGTM/api/v2/workbooks/$WB/rows/ingest \
  -H "Authorization: Bearer wbi_…" \
  -H "Idempotency-Key: capture-2026-09-03-001" \
  -H 'Content-Type: application/json' -d '{
    "rows": [
      { "Company": "Acme", "Website": "acme.com", "Work Email": "jane@acme.com" }
    ],
    "dedupe": true
  }'
```

- The token can also be sent as `X-Ingest-Token`. A normal user session with
  `X-Workspace-Id` works too.
- Keys are matched case-insensitively to the workbook's `lead_field`
  columns; unknown keys are kept and listed in `unmapped_keys`.
- At most 500 rows per request (`413` beyond that).
- `dedupe` merges on normalised website domain, else normalised company name.
- With an `Idempotency-Key`, the first response is stored per workbook and
  replayed verbatim for 7 days with the header `Idempotent-Replay: true`.

The [Chrome extension](/integrations/chrome-extension/) and the
[n8n node](/integrations/n8n/) are clients of this endpoint.
