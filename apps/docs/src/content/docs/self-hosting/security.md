---
title: Security model
description: What the code enforces today — SSRF guard, fail-closed RLS, encrypted workspace secrets, token rules — and what a hosted service still needs.
sidebar:
  order: 5
---

Report vulnerabilities privately; see the [security policy](/community/security/).

## Authentication

- `POST /auth/token` issues a short-lived access token (30 minutes) and a
  refresh token (14 days). `POST /auth/refresh` rotates both.
- Refresh tokens carry a `type: refresh` claim and are rejected wherever an
  access token is expected, including the `?token=` query parameter used by
  SSE and WebSocket clients.
- `SECRET_KEY` on its insecure default aborts startup unless `APP_ENV` is
  `dev`, `test` or `local`. The check runs when settings are first loaded, so
  no code path can run a real deployment on the default key.

## Tenant isolation

Described in [Workspaces and tenancy](/concepts/workspaces/): application
checks first, PostgreSQL row-level security last, fail-closed. Every workbook
child table carries a `NOT NULL workspace_id` so `WITH CHECK` policies bind
on insert. The runtime role must not bypass RLS; the app refuses to boot
otherwise. Cross-tenant ids return 404.

## Outbound requests (SSRF)

Every user-supplied URL (scraper, HTTP columns, webhooks, research fetches,
website enrichment) passes the URL guard, which rejects non-HTTP schemes,
credentials in URLs, loopback, private, link-local, unique-local, multicast
and reserved ranges in IPv4 and IPv6 (including IPv4-mapped addresses), the
cloud metadata endpoints, and numeric, octal, hex and dotted-decimal encodings
that smuggle a private host past string checks. Redirects are followed
manually and each hop is re-checked. Automation webhooks pin DNS at call time
with redirects disabled. Browser-tier fetches check navigations and
subresources.

What this does not cover: DNS rebinding between validation and connect for
every third-party TLS stack. A controlled egress proxy is the right final
boundary for a public hosted service.

## Secrets

Per-workspace keys are Fernet-encrypted (AES-128-CBC with HMAC-SHA256) under
`SECRETS_MASTER_KEY`, derived from `SECRET_KEY` when unset. Ciphertext is
prefixed `enc:v1:` so key versions can be migrated. In a non-development
environment the app refuses to encrypt under the shipped default key.
Credentials resolve per workspace at execution time and are never placed in
queue payloads. MCP and ingest tokens are stored as SHA-256 hashes and shown
once.

## Prompt injection

Fetched web content is wrapped as untrusted data with an explicit system
instruction before it reaches a model. Research columns use in-house
`search`/`fetch` tools so no vendor-side browsing bypasses the guard. Chat
mutations require a confirmation round-trip.

## Rate limits

Keyed by workspace, then bearer-token fingerprint, then IP. Workbook runs and
source runs are limited to 20 per minute, single-cell runs to 120 per minute,
and public unsubscribe endpoints have their own limiter.

## Before hosting strangers

See the [production checklist](/self-hosting/production/) and the
[architecture roadmap](/reference/architecture/#remaining-path-to-a-hosted-multi-node-service):
egress proxy, managed KMS, SSO and audit export, restore drills, external
review.
