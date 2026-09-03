---
title: Contributing
description: How to contribute code, providers and docs to OpenGTM.
sidebar:
  order: 1
---

Contributions of every size are welcome: a typo fix, a new enrichment
provider, a bug report, or a whole feature. The canonical text is
[`CONTRIBUTING.md`](https://github.com/debpalash/opengtm/blob/main/CONTRIBUTING.md)
in the repository; this page summarises it.

## Ground rules

1. **Sign off your commits (DCO).** Add `-s` to `git commit`. The
   `Signed-off-by` line certifies you have the right to submit the work under
   the project licence, per the
   [Developer Certificate of Origin](https://github.com/debpalash/opengtm/blob/main/DCO).
2. **Be excellent to each other.** The project follows the
   [Contributor Covenant](/community/code-of-conduct/).
3. **Licence.** OpenGTM is AGPLv3. By contributing you agree your contribution
   is licensed under the same terms.

## Before you open a pull request

- `uv run pytest` and `bun run build` pass.
- `uv run alembic check` passes if you touched models.
- `uv run python scripts/export_openapi.py` was re-run if you touched a router,
  so the [API reference](/api/) stays current.
- Keep pull requests focused: one provider, one fix, one feature.
- Call out **security-sensitive paths** (auth, tenancy/RLS, the SSRF URL guard,
  secrets, outbound fetches, LLM prompt handling) in the description so a
  reviewer looks closely. Never disclose an exploitable vulnerability in a
  public PR or issue; see [Security](/community/security/).

## Adding an enrichment provider

The fastest way to add data coverage is a **declarative YAML manifest** under
`apps/api/services/leadgen/enrichment/declarative/`. Most REST providers need no
Python at all. A provider that hits a real endpoint should ship with a golden
fixture so it can be ranked for accuracy, not just presence.

New sources must respect the target's terms of service and must not scrape
paywalled, pirated, or login-gated content. Providers that need credentials must
be **opt-in and inert by default**.

## Commit messages

Conventional Commits: `type(scope): summary`, for example
`feat(mcp): add read_cells tool`, `fix(worker): stop double-claiming jobs`,
`docs: clarify BYOK setup`.

## Where to start

Issues labelled **good first issue** and **provider request**. For anything
larger, open an issue first so the approach can be agreed before you invest the
time.
