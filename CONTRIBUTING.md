# Contributing to OpenGTM

Thanks for helping build the open-source, self-hostable alternative to Clay.com.
Contributions of every size are welcome — a typo fix, a new enrichment provider,
a bug report, or a whole feature.

## Ground rules

1. **Sign off your commits (DCO).** Every commit must carry a `Signed-off-by`
   line certifying you have the right to submit it under the project license.
   Just add `-s` to your commit:

   ```bash
   git commit -s -m "feat(enrichment): add Acme email-finder manifest"
   ```

   This appends `Signed-off-by: Your Name <you@example.com>` and constitutes
   your agreement to the [Developer Certificate of Origin](./DCO). PRs whose
   commits are not signed off will be asked to amend.

2. **Be excellent to each other.** See [`CODE_OF_CONDUCT.md`](./CODE_OF_CONDUCT.md).

3. **License.** OpenGTM is [AGPLv3](./LICENSE). By contributing you agree your
   contribution is licensed under the same terms.

## Development setup

```bash
# Backend (Python 3.11+, uv)
uv sync
uv run uvicorn apps.api.main:app --reload --port 8000

# Frontend (Bun)
cd apps/web && bun install && bun run dev

# Background enrichment worker
python -m apps.api.worker
```

The whole stack also runs with `docker compose up` (API + worker + Postgres +
Redis + nginx). See the [README](./README.md) for details.

## Before you open a PR

- **Tests:** `uv run pytest` (backend) and `bun run build` / `bunx tsc -b`
  (frontend typecheck) should pass.
- **Scope:** keep PRs focused. One provider, one fix, one feature.
- **Security-sensitive paths** (auth, tenancy/RLS, SSRF url guard, secrets,
  outbound fetches, LLM prompt handling): call it out in the PR description so a
  reviewer looks closely. Do **not** disclose an exploitable vulnerability in a
  public PR/issue — see [`SECURITY.md`](./SECURITY.md).

## Adding an enrichment provider

The fastest way to contribute high-value data coverage is a **declarative YAML
manifest** — no Python required for most REST providers. Look at the existing
manifests under `apps/api/services/leadgen/enrichment/declarative/` and the
manifest compiler for the schema. A provider that hits a real endpoint should
ship with a golden fixture test so we can rank it for accuracy, not just
presence.

Please make sure any new source respects the target's Terms of Service and does
not scrape paywalled, pirated, or login-gated content. Providers that require
credentials must be **opt-in and inert by default** (see how `staffspy` is gated
behind `STAFFSPY_ENABLED` for the pattern).

## Commit message convention

We use Conventional Commits: `type(scope): summary`, e.g.
`feat(mcp): add read_cells tool`, `fix(worker): stop double-claiming jobs`,
`docs: clarify BYOK setup`. Common types: `feat`, `fix`, `docs`, `test`,
`refactor`, `perf`, `chore`.

## Where to start

Check the issues labeled **`good first issue`** and **`provider request`**. If
you want to propose something larger, open an issue first so we can align on
approach before you invest the time.
