---
title: Security policy
description: How to report a vulnerability in OpenGTM privately.
sidebar:
  order: 2
---

OpenGTM handles prospect PII, integration credentials, and outbound network
requests on infrastructure you control. Coordinated disclosure is appreciated.

## Reporting a vulnerability

**Do not open a public issue for a security vulnerability.**

Report it privately through GitHub's
[Report a vulnerability](https://github.com/debpalash/opengtm/security/advisories/new)
form, or email **security@yupcha.com**. Include a description of the issue and
its impact, steps to reproduce (a proof of concept is ideal), and the affected
version or commit.

Reports are acknowledged within **72 hours**, with a remediation timeline after
triage. Reporters who wish to be named are credited when a fix ships.

## Scope

- **Tenant isolation.** Cross-workspace data access (PostgreSQL RLS, workspace
  scoping, MCP write gates).
- **Authentication and secrets.** Token handling, the per-workspace encrypted
  secret store, credential leakage between workspaces.
- **SSRF.** The public-URL guard fronts every user-supplied URL (scraper,
  webhooks, HTTP columns, research fetches). Bypasses that reach private,
  loopback or metadata ranges are high severity.
- **Injection.** SQL/SOQL injection, and prompt injection into AI, agent and
  research columns through poisoned row data or fetched web content.

## Supported versions

OpenGTM is pre-1.0 and moving fast. Security fixes land on `main`; run a recent
build. Once tagged releases stabilise this section will list supported lines.

See the [security model](/self-hosting/security/) for what the code enforces
today and what a hosted deployment still needs to add.
