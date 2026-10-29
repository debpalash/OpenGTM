# Go PR #115 readiness checkpoint

Checked GitHub on 2026-10-29. [PR #115](https://github.com/debpalash/OpenGTM/pull/115) is open and draft at `9c75310f8926c9adbc4c61fec66a2d8487b9a813`, based on `main`. GitHub reports `MERGEABLE` / `CLEAN`. Go/PostgreSQL/Redis checks, frontend checks, and browser-service checks all succeeded for that commit. There is no recorded review decision.

Recommendation: do not merge the unchanged PR yet. Our local record implementation includes fixes outside that head:

- `internal/providerrun/run.go`: do not cache person-dependent scores under a shared domain; the live same-company regression produced independent results. A corresponding regression is in `internal/providerrun/records_cache_test.go`.
- `internal/vendorcatalog/native_costs.go` and catalog integration: recognize free native providers without declarative manifests instead of leaving their cost metadata unknown.
- The original macOS setup also required platform-specific filesystem/process helpers. These are portability fixes, not a replacement for Linux CI.

Backport the provider fixes in an isolated change and rerun the affected tests before removing draft status. Perform the staging cutover/rollback drill and resolve release image naming from the PR's own before-merge checklist. Existing green CI establishes automated build/test readiness for the head; it does not establish that the rollback drill happened.

Companies/People, migration 5, Clay interchange, and the dashboard workbench are separate local changes. They are not included in PR #115 merely because this worktree is based on it. The UI work should not be bundled into the backend merge-readiness patch.
