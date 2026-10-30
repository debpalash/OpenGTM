# Dashboard integration with main

Prepared 2026-10-30 following the request to make the latest dashboard the main UI.

The dashboard branch incorporates the Go rewrite and the merged Firecrawl and
Spicrawl implementations. It brings migration 5, canonical Companies and People,
Clay/CSV interchange, dashboard navigation, record inspection, activation review,
adaptive UI scaling, and local source-logo assets into the same source build.

The account menu contains radio choices for Light, Dark, and System, plus the
existing UI-scale choices in a submenu. The separate Appearance footer row is
removed. Recent chats starts expanded. Fine-pointer desktop navigation uses 28px
rows with space between groups. The account popup has padded items and a single
row of theme choices; touch layouts retain their previous target sizes.

The development server deduplicates React and allows the repository's shared
node_modules assets, avoiding duplicate-hook failures and blocked bundled fonts.

## Data compatibility

Legacy Lead APIs and untyped workbooks retain their source observations, response
shapes, and JSON field order. Companies, People, and typed workbooks read accepted
shared facts. Accepting a new fact does not rewrite the original observation.
The legacy create endpoint retains its company/city duplicate behavior; independent
people at a company are created through the People API.

Import identities are removed with deleted records. Historical invalid scores,
email addresses, and employee counts remain observations without becoming accepted
facts. Shared role mailboxes identify the company rather than merging people, and
social-profile websites do not become company-domain identity keys.

Schema 5 requires stopping older writers during the upgrade; see the
[migration runbook](../../go-port/runbooks/migrations.md#upgrading-to-schema-5).

## Verification

- Frontend lint, unit tests, and production build pass.
- Combined Go vet and unit tests pass locally.
- Chromium and WebKit checks pass at 1280×832 desktop and 390×844 touch sizes:
  account-menu themes, keyboard selection, preference persistence, live system
  changes, visible recent conversations, and no horizontal overflow. Desktop
  scans report zero axe violations and both engines report zero page errors.
- Fresh migration and race integration checks run against a disposable database,
  separately from the saved local dashboard database.
- PR checks must pass on the final head before merge.
- The local runtime is refreshed from main after merge; prior feature work is
  preserved in its worktree and checkpoint commits.

The existing screenshots and expanded README demos use fictional records. A
source merge does not publish a release or certify a production upgrade; see
[release readiness](https://github.com/debpalash/OpenGTM/issues/120) and the
[security audit](https://github.com/debpalash/OpenGTM/issues/114).
