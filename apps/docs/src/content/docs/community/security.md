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

| Line | Runtime | Status | Security fixes |
| --- | --- | --- | --- |
| `main` | Go | Development | Yes. Fixes land here first. |
| 4.x | Go | Supported from the v4.0.0 release (not yet published) | Yes |
| 3.x (v3.0.0) | Python | Legacy | Security fixes only, until 90 days after v4.0.0 is published |
| Older than 3.0 | Python | End of life | No |

**Legacy Python policy.** Until v4.0.0 is published, v3.0.0 is the latest
release. After that, the 3.x line gets fixes only for high- and
critical-severity vulnerabilities that affect code present in 3.x, for 90 days.
It gets no feature backports. The v4.0.0 release notes will state the exact
end-of-support date. After that date, 3.x receives no fixes, and the supported
path is the documented upgrade to 4.x.

Security advisories name the affected and fixed versions for each supported
line.

See the [security model](/self-hosting/security/) for what the code enforces
today and what a hosted deployment still needs to add.
