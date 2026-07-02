# Security Policy

Yupcha handles prospect PII, integration credentials, and outbound network
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

Yupcha is pre-1.0 and moving fast. Security fixes land on `main`; run a recent
build. Once tagged releases stabilize, this section will list supported lines.
