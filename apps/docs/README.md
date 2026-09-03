# OpenGTM docs site

Astro 7.3 + [Starlight](https://starlight.astro.build) + `starlight-openapi`.
Published at **https://opengtm.palash.dev** as a Cloudflare Worker with static
assets (see `wrangler.jsonc`).

```bash
bun install                 # from the repo root (workspace)
bun run --cwd apps/docs dev # http://localhost:4321
bun run --cwd apps/docs build
bun run --cwd apps/docs preview            # add --ignore-lock to run several previews at once (Astro 7.3)
```

## REST API reference

`openapi/openapi.json` is generated from the FastAPI app and rendered under
`/api/`. Regenerate it whenever a router changes:

```bash
uv run python scripts/export_openapi.py          # rewrite the spec
uv run python scripts/export_openapi.py --check  # CI: fail if stale
```

## Deploy

```bash
bunx wrangler login
bun run --cwd apps/docs build
bun run --cwd apps/docs cf:deploy
```

The `custom_domain` route in `wrangler.jsonc` binds `opengtm.palash.dev` on
the first deploy. CI can do the same via `.github/workflows/docs.yml` once the
`CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` repository secrets exist.
