# Changelog

All notable changes to OpenGTM are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[SemVer](https://semver.org/). Container images are published to
`ghcr.io/debpalash/opengtm` on every tag.

## [Unreleased]

## [3.0.0] - 2026-09-24

First public release: workbooks, enrichment waterfalls, agentic research/chat,
outputs, outreach, signals, automations, workspace-scoped RLS, MCP, the n8n
node and Chrome extension. The items below were completed for this release.

### Added
- `POST /api/leads/verify-email`, `POST /api/leads/score` and
  `POST /api/leads/tech-stack`: single-record utilities that reuse the MCP
  tool paths, so scripts and the n8n node no longer need a workbook.
- Documentation site (`apps/docs`, Astro 7.3 + Starlight) published at
  https://opengtm.palash.dev, including a generated REST API reference.
- `scripts/export_openapi.py` keeps `apps/docs/openapi/openapi.json` in sync
  with the FastAPI app; CI fails when it drifts.
- Release tooling under `scripts/release/`: a preflight checker and a
  public-snapshot builder that strips maintainer-only paths.
- Tag-driven release workflow (GHCR multi-arch image + draft GitHub release),
  manual docs deploy workflow, Dependabot, and CODEOWNERS.
- A workspace-scoped attention center at `/notifications` with critical failed
  tasks/workbooks and urgent unread high-intent signals, plus a live header
  preview and severity filters.

### Changed
- `uv.lock` and `bun.lock` are now tracked for reproducible installs.
- Maintainer-only planning documents moved under `docs/internal/`.
- Theme selection moved from the top toolbar to the sidebar footer, including
  the collapsed and mobile sidebar menus.
- The standalone **New chat** sidebar link is now an icon on the Chat row.
  It opens an inline prompt; submitting the prompt drafts a new conversation
  without sending it. The icon remains available in the collapsed sidebar.
- Simplified the README into a quick local path and a version-pinned, TLS-ready
  server path; installers prepare a writable bind-mounted data directory.

### Fixed
- Protected deep links now retain query parameters and fragments through sign-in,
  including chat drafts.
- Populated website, email and other typed lead-field cells can be edited by
  double-clicking the cell while retaining their formatted links and badges.
- Docker builds include the docs workspace manifest needed for the frozen Bun
  workspace install.
- The n8n node now calls endpoints that exist (`/api/collect`, `/api/lead` +
  `/api/leads/bulk-enrich`, the new utilities above); five of its seven
  operations previously returned 404.

### Removed
- The legacy document-download pipeline (Scribd, torrent-index and archive
  resolvers, the `/api/queue` router, the `/ws` queue socket, the
  `download_link` job and the Downloads page). It predated the GTM product
  and had no place in a public release.
- Lead exports and scraped fixture snapshots under `data/` are no longer
  tracked; they are regenerated locally and ignored.
- Private submodule pointers (`packages/proxy-manager`, `data_collector`);
  both integrations remain optional and degrade to no-ops when absent.

[Unreleased]: https://github.com/debpalash/OpenGTM/compare/v3.0.0...main
[3.0.0]: https://github.com/debpalash/OpenGTM/releases/tag/v3.0.0
