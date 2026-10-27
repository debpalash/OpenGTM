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
2. **Sign the Contributor License Agreement.** Sign the
   [Individual CLA](https://github.com/debpalash/opengtm/blob/main/CLA.md)
   once, before your first pull request is merged; the CLA check on the pull
   request links to the signing page. If you contribute as part of your job,
   your employer also signs the
   [Corporate CLA](https://github.com/debpalash/opengtm/blob/main/CLA-CORPORATE.md).
   You keep your copyright; the CLA lets Yupcha distribute your contribution
   under the AGPL and under commercial licences.
3. **Be excellent to each other.** The project follows the
   [Contributor Covenant](/community/code-of-conduct/).
4. **Licence.** OpenGTM is AGPLv3. Your contribution is distributed under the
   AGPL, and under the additional rights you grant in the CLA.

## Before you open a pull request

- `make test` passes (Go vet and tests, web lint and build); run the Go
  integration tests (`go test -tags integration ./...` in `apps/backend-go`)
  if you touched SQL, and `uv run pytest` in `apps/browser-service` if you
  touched the browser service.
- New schema changes are a migration under
  `apps/backend-go/internal/migrate/migrations/` (see
  [migrations](/self-hosting/migrations/)).
- Keep pull requests focused: one provider, one fix, one feature.
- Call out **security-sensitive paths** (auth, tenancy/RLS, the SSRF URL guard,
  secrets, outbound fetches, LLM prompt handling) in the description so a
  reviewer looks closely. Never disclose an exploitable vulnerability in a
  public PR or issue; see [Security](/community/security/).

## Adding an enrichment provider

The fastest way to add data coverage is a **declarative YAML manifest** under
`apps/backend-go/internal/manifest/builtin/manifests/`. Most REST providers need
no code at all. A provider that hits a real endpoint should ship with a golden
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
