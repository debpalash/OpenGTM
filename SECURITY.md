# Security Policy

OpenGTM handles prospect PII, integration credentials, and outbound network
requests on infrastructure our users control. We take security seriously and
appreciate coordinated disclosure.

## Reporting a vulnerability

**Do not open a public issue for a security vulnerability.**

Instead, report it privately via GitHub's **"Report a vulnerability"** button
under the repository's *Security* tab (Private Vulnerability Reporting), or email
the maintainers at **security@yupcha.com**.

Please include:

- A description of the issue and its impact.
- Steps to reproduce (a proof-of-concept is ideal).
- Affected version / commit.

We aim to acknowledge reports within **72 hours** and to provide a remediation
timeline after triage. We will credit reporters who wish to be named once a fix
is released.

## Scope — areas we care most about

- **Tenant isolation.** Cross-workspace data access (Postgres RLS, workspace
  scoping, MCP write gates).
- **Authentication & secrets.** Token handling, the per-workspace encrypted
  secret store, credential leakage between workspaces.
- **SSRF.** The public-URL guard fronts every user-supplied URL (scraper,
  webhooks, HTTP columns, research fetches). Bypasses that reach private/loopback
  /metadata ranges are high severity.
- **Injection.** SQL/SOQL injection, and prompt injection into AI/agent/research
  columns via poisoned row data or fetched web content.

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
