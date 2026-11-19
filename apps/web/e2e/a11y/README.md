# Accessibility gate (Playwright + axe)

Deterministic a11y check that needs no Go backend or database. It serves the built
`dist` with `vite preview`, mocks every `/api/**` and `/auth/**` request
(`fixtures/`), blocks all non-localhost traffic, and runs `axe-core` (injected from
`node_modules/axe-core/axe.min.js`) on each route.

Matrix: 19 routes in light and dark at 1440x1000, plus `/companies`, `/chat`,
`/workbooks/<id>`, `/settings` at 390x844 (46 cases). `dialogs.spec.ts` covers the in-app
confirm dialog (focus in, Escape cancels, focus returns, no native dialog). Theme is set through
`localStorage["theme"]` before load; animations use `reducedMotion: "reduce"`.

## Run

```sh
cd apps/web
bun run build
bunx playwright install chromium   # once
bun run test:a11y
```

## Ratchet baseline

`baseline.json` records, per `route | theme | viewport`, the known serious/critical
violations as `{ruleId: nodeCount}`. The test fails when a serious/critical rule is
not in the baseline or its node count is higher. When counts drop it logs
`[ratchet] ... improved` without failing. Moderate/minor violations are only logged.

Regenerate (runs serially, rewrites the whole file):

```sh
A11Y_UPDATE_BASELINE=1 bun run test:a11y
```

Only tighten the baseline in normal work. Baseline entries are debt: each one should be
removed by fixing the UI, not by raising the number. Relevant audit IDs live in
`docs/audits/dashboard-ui-ux-2026-10-29/README.md` (A06 target-size, A02 contrast, A03/A04
unnamed controls and A07 name mismatches are the likely regressions). The baseline is
currently empty.

Target size: the shell no longer uses CSS `zoom` (`scale.spec.ts` fails if any element does),
so rendered pixels equal CSS pixels. Use `var(--gtm-target-min)` (24px) for minimum control
sizes and the `--gtm-control-*` tokens for toolbar controls. The per-user UI scale changes the
root font-size only.
Axe runs the `wcag2a/2aa/21a/21aa/22aa` and `best-practice` tags (not experimental).
Pages with data-driven lists (e.g. Sources switches, A03) only show those controls
once fixtures for them are added.

## Adding a route or endpoint

Add the route to `a11y.spec.ts`. If the page needs data, add a fixture in
`fixtures/data.ts` and map it in `fixtures/routes.ts`; unmatched endpoints return `200 []`.
Set `A11Y_LOG_UNMATCHED=1` to print which endpoints fell through to the default.
