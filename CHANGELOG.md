# Changelog

All notable changes to OpenGTM are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[SemVer](https://semver.org/). Container images are published to
`ghcr.io/debpalash/opengtm` on every tag.

## [Unreleased]

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

### Changed
- `uv.lock` and `bun.lock` are now tracked for reproducible installs.
- Maintainer-only planning documents moved under `docs/internal/`.

### Fixed
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

## 3.0.0 — baseline (not yet tagged)

`pyproject.toml` carries version 3.0.0. The first public tag will be cut from
this baseline: workbooks, enrichment waterfalls, research and agent columns,
agentic chat, output destinations, outreach, signals, automations,
multi-tenant RLS, MCP server, n8n node and Chrome extension. Pushing a
`v3.0.0` tag triggers `.github/workflows/release.yml`.

[Unreleased]: https://github.com/debpalash/opengtm/commits/main
