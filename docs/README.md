# OpenGTM documentation

User-facing documentation lives on the docs site, **https://opengtm.palash.dev**,
whose source is `apps/docs/` (Astro + Starlight, with the REST API reference
generated from the FastAPI OpenAPI schema).

This directory holds the engineering material that backs it.

| Path | Purpose |
| --- | --- |
| [`architecture.md`](architecture.md) | Current system architecture, process/data ownership, queue semantics, tenant boundary, scale limits |
| [`specs/`](specs/) | Build-ready feature and hardening specifications |
| [`plans/`](plans/) | Public roadmap and Clay-parity work items |
| [`research/`](research/) | Source audits, competitive research, OSS ecosystem notes |
| `internal/` | Maintainer-only plans and reports (not shipped in the public snapshot) |

## Placement rule

Keep deployable code and operational configuration at the repository root. Put
prose under `docs/`, external research inputs under `reference/`, maintenance
utilities under `scripts/`, and generated or runtime data under `data/`. Do not
add new standalone planning or research files at the root.

Anything that must not be public goes under `docs/internal/` (or another path
listed in `scripts/release/public-exclude.txt`).
