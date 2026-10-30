# Dashboard integration with main

Prepared 2026-10-30 following the request to make the latest dashboard the main UI.

The dashboard branch incorporates the Go rewrite and the merged Firecrawl and
Spicrawl implementations. It brings migration 5, canonical Companies and People,
Clay/CSV interchange, dashboard navigation, record inspection, activation review,
adaptive UI scaling, and local source-logo assets into the same source build.

The account menu contains radio choices for Light, Dark, and System, plus the
existing UI-scale choices. The separate Appearance footer row is removed.
Recent chats starts expanded. Fine-pointer desktop navigation uses 24px rows
and smaller group spacing; touch layouts retain their previous target sizes.

The development server deduplicates React and allows the repository's shared
node_modules assets, avoiding duplicate-hook failures and blocked bundled fonts.

## Verification

- Frontend lint, unit tests, and production build pass.
- Combined Go vet and unit tests pass locally.
- Fresh migration and race integration checks run against a disposable database,
  separately from the saved local dashboard database.
- PR checks must pass on the final head before merge.
- The local runtime is refreshed from main after merge; prior feature work is
  preserved in its worktree and checkpoint commits.

The existing screenshots and expanded README demos use fictional records. A
source merge does not publish a release or certify a production upgrade; see
[release readiness](https://github.com/debpalash/OpenGTM/issues/120) and the
[security audit](https://github.com/debpalash/OpenGTM/issues/114).
