---
title: Email outreach
description: Multi-step SMTP sequences with rate limits, suppression, one-click unsubscribe, bounce and complaint handling, and evidence-grounded drafts.
sidebar:
  order: 6
---

Outreach sends from **your** SMTP account. OpenGTM adds sequencing, per-lead
state, send windows, suppression, unsubscribe links, and a circuit breaker
that pauses a sequence before your domain reputation suffers.

## Sequences

A sequence is a list of steps, each with a subject, HTML body and delay:

```json
{
  "name": "Founder intro",
  "steps": [
    { "step_number": 1, "subject": "Quick question about {Company}", "body_html": "…", "delay_hours": 0 },
    { "step_number": 2, "subject": "Re: quick question", "body_html": "…", "delay_hours": 72 }
  ],
  "daily_limit": 50,
  "send_window_start": 9,
  "send_window_end": 18,
  "send_window_tz": "Asia/Kolkata",
  "consent_basis": "legitimate interest — B2B"
}
```

| Endpoint | Role |
|---|---|
| `GET/POST /api/outreach/sequences` | member / admin |
| `GET/PUT/DELETE /api/outreach/sequences/{id}` | member / admin |
| `POST /api/outreach/sequences/{id}/start`, `/pause` | admin |
| `POST /api/outreach/sequences/{id}/enroll` with `{lead_ids, consent_source}` | admin |
| `POST /api/outreach/sequences/{id}/execute` | admin (run a tick now) |
| `GET /api/outreach/sequences/{id}/stats`, `/sends` | member |

## SMTP

`PUT /api/outreach/smtp/config` stores per-workspace `smtp_host`, `smtp_port`,
`smtp_email`, `smtp_password`, `smtp_from_name`, `smtp_max_per_hour` and
`smtp_use_tls` (encrypted). `POST /api/outreach/smtp/test` sends a test
message; `GET /api/outreach/smtp/status` reports connectivity.

## Sending and safety

- A ticker runs every `OUTREACH_TICK_INTERVAL` seconds (900) and enqueues at
  most `OUTREACH_TICK_MAX_ENQUEUE` sends (200) per tick onto the durable queue.
- Every message carries a one-click unsubscribe link signed with an HMAC token
  valid for `OUTREACH_UNSUB_TTL_DAYS` (90), built from
  `OUTREACH_PUBLIC_BASE_URL`. The public unsubscribe endpoints are rate
  limited.
- Suppressions (`/api/outreach/suppressions`) are checked at enrol and send
  time.
- Circuit breaker: once a sequence has sent at least 20 messages, a bounce rate
  above 5% or a complaint rate above 0.3% pauses it automatically.
- Soft bounces suppress and terminate an enrollment after 3 occurrences.

## Bounces and complaints

Two ingestion paths, both idempotent:

- **Provider webhook**: `POST /api/outreach/webhooks/bounce`, authenticated
  with `OUTREACH_BOUNCE_WEBHOOK_SECRET`; the workspace is derived from the
  verified payload only.
- **IMAP polling**: with BYO SMTP, delivery status notifications (RFC 3464)
  and feedback-loop complaints arrive as ordinary mail. Enable
  `OUTREACH_INBOUND_POLL_ENABLED=1` and configure the workspace mailbox;
  messages are matched back to the originating send by Message-ID.

## Evidence-grounded drafts

Chat's `draft_grounded_outreach` action writes a draft for one exact saved
contact, with each sentence tied to evidence from the account's saved sources.
Drafts are inspected at `GET /api/outreach/drafts` and
`GET /api/outreach/drafts/{id}?include_evidence=true`; they include the
recipient's contact status, whether the address is a generic inbox or role
address, and the sentence-level evidence. **There is no send endpoint for
drafts.** Sending remains an explicit sequence action by an admin.

## MCP and automations

The MCP `send_email` and `enroll_leads` tools need the `outreach:send` and
`sequences:enroll` capabilities respectively; `send_email` is admin-only and
goes through the same suppression, rate-limit and circuit-breaker path. The
legacy `sequencer` and `send_email` automation actions are disabled unless
`AUTOMATIONS_ALLOW_LEGACY_OUTREACH=1`.
