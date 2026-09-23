# Twenty UI build provenance

This recipe builds only the MIT `packages/twenty-ui` library at upstream commit
`e99fd1a7683f37a957852c3f7ef8f6a29732bb38`. It does not incorporate Twenty's CRM,
backend, enterprise code, or branding. The upstream MIT notice is retained here
and in the generated package.

Run from the OpenGTM repository:

```sh
node scripts/vendor/twenty-ui/build.mjs
```

Requires Git, Node, npm, network access, and space for an isolated temporary
checkout. Initially tested with Node 26.7.0 and npm 11.19.0 on Linux. Installation
scripts are disabled. Temporary workspaces are retained for inspection; the
script prints their exact paths. No OpenGTM runtime dependencies are installed
by this command.

`build-package.json` is the upstream manifest plus three standalone build-only
dependencies: Node types, tslib, and Vite. `build-lock.json` locks the entire
build dependency tree. Source files, token generation output, and upstream build
configuration are unchanged. The root TypeScript configuration comes from the
pinned checkout. The build runs upstream type/declaration checks and the optional
editor dependency check before packing.

The script refuses to copy an artifact unless its SHA-256 matches
`5f21ab5f3122251b9a2334df86371d1507888db682e7a7411b2b895e23b19875`.
Output: `apps/web/vendor/twenty-ui-2.42.0-opengtm.tgz`. This is a local build,
not an official Twenty 2.42.0 registry release. Updating the source, lock, or
artifact pin requires rerunning the compatibility and application regression
checks documented in `docs/internal/v2/twenty-ui-compatibility-report.md`.

The application must use a single compatible Router instance (tested: 7.18.4).
Do not install this artifact alongside the old host Router 7.14.2 without an
explicit routing strategy: Twenty links fail when separate contexts are used.

## Reproducible route payload report

After `bun run --cwd apps/web build`, run:

```sh
node --test scripts/vendor/twenty-ui/route-payload.test.mjs
node scripts/vendor/twenty-ui/route-payload.mjs
```

The report follows the production manifest from the application entry and the
workbook editor, counts shared static JS/CSS once, and lists assets by gzip size.
An optional dist path and manifest route key select another build or route.
It excludes fonts, images, API responses and unopened dynamic imports. Gzip bytes
are locally calculated estimates, not measured transfer or interactive latency.
Compare reports generated with this same method; historical browser/gzip totals
may use different compression settings or include additional requests.

## Isolated browser preview

The preview uses fictional records and never invokes providers or application
APIs. It is a separate HTML entry, excluded from the normal production build.

```sh
cd apps/web
bun run build:ui-preview
bunx vite preview --config vite.ui-preview.config.ts --host 127.0.0.1 --port 4399 --strictPort
```

In another terminal at the repository root:

```sh
uv run python scripts/vendor/twenty-ui/check-preview.py
```

The browser check uses `/usr/bin/chromium` by default (`--chromium` overrides it).
Optional `--screenshots /tmp/opengtm-ui-preview-screenshots` captures both themes
at 375/768/1280/1440px. It checks filtered selection scope, cancellation, focus
return, local confirmation, density, clear selection, empty results, and Twenty
navigation using the host router. This is not a substitute for live workbook or
authenticated application regression tests.

## Application theme smoke

Build and serve the normal app (`bun run build`, then `bunx vite preview --host
127.0.0.1 --port 4399 --strictPort` from `apps/web`). From the repository root:

```sh
uv run python scripts/vendor/twenty-ui/check-app-theme.py
bun test apps/web/tests
```

This smoke renders the actual app but intercepts all business APIs with explicit
fixtures. It verifies theme persistence, system and cross-tab changes, command
palette theme selection, and boot without the React bundle. It does not exercise
real provider execution, real authentication, or persisted workspace mutations.

## Workbook UI regression checks

Against the same normal application preview:

```sh
uv run python scripts/vendor/twenty-ui/check-workbooks.py
uv run python scripts/vendor/twenty-ui/check-workbook-selection.py
uv run python scripts/vendor/twenty-ui/check-workbook-run.py
uv run python scripts/vendor/twenty-ui/check-workbook-history.py
```

The first exercises list/create/template/delete flows; the second exercises the
actual virtualized editor, cell edit/cancel, selection across pages, exact row-ID
deletion, all-matching scope, errors/retry, duplicate clicks, focus return, and
legacy-lead deletion rejection. All business APIs and sockets are intercepted.
The editor check accepts `--screenshots /tmp/opengtm-workbook-selection` for
light/dark confirmation dialogs at 375px and 1440px. It does not certify the
remaining editor toolbar's responsive layout, live enrichment, or provider spend.

The run-review check covers scoped estimates, failed-estimate gating, cancellation,
fill-missing and normal run payloads, row-count conflicts, refresh, duplicate-click
guards, focused errors, and stop failure/retry. `--screenshots` captures both themes
at 375×667 and 1440×1000, including persistent action buttons and external-send
warnings. These remain fixture tests, not live job execution or exactly-once proof.

The history check covers failed fetch/retry, cursor pagination, polled queue-state
changes, partial cell outcomes, reload, focus return, and light/dark mobile dialog
layout. The selection check also verifies executable columns stay read-only and
zero/false results remain visible. A separate backend integration test in
`tests/test_workbook_views.py` executes real formulas through route → queue →
worker → persisted outcome → history API, using disposable SQLite and replacing
only the subprocess boundary, Redis, and provider-pool setup. It is not a live
provider or production-environment test.
