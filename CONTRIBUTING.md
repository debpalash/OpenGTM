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

2. **Sign the Contributor License Agreement (CLA).** Before your first pull
   request is merged, sign the [Individual CLA](./CLA.md) once. The CLA check
   on your pull request links to the signing page. If you contribute as part
   of your job, your employer must also sign the
   [Corporate CLA](./CLA-CORPORATE.md).

   You keep the copyright in your work. The CLA gives Yupcha Softwares Private
   Limited the right to distribute your contribution under the AGPL and under
   other licences, including commercial ones. Yupcha commits that whatever it
   includes in the public source release stays available under the AGPL or
   another OSI-approved licence.

3. **Be excellent to each other.** See [`CODE_OF_CONDUCT.md`](./CODE_OF_CONDUCT.md).

4. **License.** OpenGTM is [AGPLv3](./LICENSE). Your contribution is
   distributed under the AGPL, and under the additional rights you grant in
   the CLA. See [`NOTICE`](./NOTICE).

## Development setup

```bash
# Go API + worker, with the web UI embedded (needs Go and Bun)
make build          # bin/opengtm-api (UI embedded), worker, mcp, scheduler, seed, migrate
make migrate        # apply the schema (DATABASE_URL = owner credentials)
make run            # start api + worker in the background; make kill stops them

# Frontend with hot reload (each app has its own bun.lock; there is no root workspace)
make dev            # Vite in apps/web, proxying to VITE_API_TARGET (default nginx on :3000)
make kill
# or: cd apps/web && bun install && bun run dev:app

# Internal browser service (the only Python; Python 3.13 + uv)
cd apps/browser-service && uv sync && uv run pytest
```

The whole stack also runs with `make up` / `docker compose up` (Go API, worker and
scheduler, browser service, Postgres, Redis, nginx; `make down` stops it). See the [README](./README.md) for details.

## Before you open a PR

- **Tests:** `make test` (go vet, go test, web lint and build);
  `go test -tags integration ./...` in `apps/backend-go` for SQL changes;
  `uv run pytest` in `apps/browser-service` for the browser service.
- **Scope:** keep PRs focused. One provider, one fix, one feature.
- **Security-sensitive paths** (auth, tenancy/RLS, SSRF url guard, secrets,
  outbound fetches, LLM prompt handling): call it out in the PR description so a
  reviewer looks closely. Do **not** disclose an exploitable vulnerability in a
  public PR/issue — see [`SECURITY.md`](./SECURITY.md).

## Adding an enrichment provider

The fastest way to contribute high-value data coverage is a **declarative YAML
manifest** — no code required for most REST providers. Look at the existing
manifests under `apps/backend-go/internal/manifest/builtin/manifests/` and
`internal/manifest` for the schema. A provider that hits a real endpoint should
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
