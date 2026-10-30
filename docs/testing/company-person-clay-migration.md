# Local validation: shared records and Clay migration

Validated on 2026-10-29 on macOS ARM64, PostgreSQL on port 5433, Go 1.27.3, and Bun. Branch: `feat/company-person-clay-migration`, based on PR #115 (`9c75310f`). Local application: `http://localhost:8002`.

## Verified behavior

- Separate Companies/People records, independent contacts at one company, source-scoped identity, explicit field ownership, protected accepted edits, observation history, optimistic version conflicts, and employment changes/history.
- Person-only role edits and roles whose employer is not yet known retain their data. Workbook edits after a job change keep the current employer. Company/employment enrichment writeback resolves current references.
- Two linked workbooks share accepted company facts. Explicit workbook edits can deliberately revert an old snapshot value without promoting every projected field to a manual edit.
- Company-only audiences and monitoring/signals work without synthetic Leads. Audience membership refresh, typed destinations, historical delivery deduplication, person enrollment, suppression, and send-job identity are covered by integration tests using local fake delivery ports.
- Company discovery creates linked people and skips duplicates on replay. Provider searches use a fake finder in tests; external provider credentials are still required for real searches.
- Company merge/split preserves accepted facts, source identities, and references through workbooks, people, audiences, monitoring, and activation.
- CSV/JSON mapping, previews, per-row rejection rollback, batch replay, conflicting identities, concurrent identity writers, tenant restrictions, structured values, Unicode, and large numeric identifiers are covered.
- Default company CSV export can be imported again without changing mappings or creating duplicates. Versioned JSON backup/restore preserves record history and source identities in an empty workspace.

## Executed checks

- `go test ./...` and `go vet ./...` passed.
- Eleven focused records integration tests passed under the Go race detector. A separate 10,000-row integration test exercises the restricted runtime role, preview rollback, commit, replay, and final counts.
- The 50,000-row legacy workbook import and memory-bound test passed using the restricted role.
- Focused legacy API tests for Lead writes, watches, and cell execution passed after updating expectations for independent contacts and added canonical reference fields. Destination inbound integration tests passed, including a callback with no Lead.
- `bun run build`, `bun run lint`, and `git diff --check` passed.
- A Playwright browser journey uploaded a Clay-style company CSV, edited a company, uploaded a people JSON file, verified `9007199254740993` remained exact, started an enrichment workbook, ran a free native single cell, downloaded CSV/JSON, and restored the backup in an empty workspace. No page errors were observed.
- A live native provider regression used two people at the same company: scores were **55/warm** and **35/cold**, confirming per-person results rather than a shared-domain tier cache result.

The complete legacy Python response/state fixture suite is not certified unchanged: canonical ownership deliberately changes several data-state expectations, and optional Company/Person references extend responses. The targeted compatibility checks and new record contract tests above are the validation performed for this branch. This is not a claim that every historical Python fixture has passed.

## Measurements

These are observed local runs, not hardware-independent guarantees. The machine also ran other verification jobs. The new record path performs identity resolution, accepted-field ownership, and observation writes; compare equivalent work before comparing it to older raw-row imports.

| Work | Observed elapsed time |
| --- | ---: |
| 1,000-company CSV preview | 0.686 s |
| 1,000-company CSV commit | 0.827 s |
| Identical batch replay | 0.031 s |
| 1,000-company CSV export | 0.039 s |
| 1,000-company JSON backup export | 0.281 s |
| Reimport exported 1,000-company CSV | 3.356 s |
| 1,000-company workbook, 2,000 native qualification cells, including queue/polling | 11.480 s |
| 10,000-company preview on a fresh database under the runtime role | 70.669 s |
| 10,000-company commit | 16.418 s |
| 10,000-company replay | 0.031 s |
| Legacy 50,000-row workbook import | 60.207 s |

The 50,000-row import used **109 MiB peak Go heap** for an **8 MiB request**. The 10,000-row test ran alongside that import, including fresh-database planning and cache costs. The UI intentionally limits Clay imports to 10,000 rows per batch.

Bulk testing found and fixed two PostgreSQL issues: per-record advisory locks exhausted shared lock storage; and an expression index could not fully apply behind the tenant RLS barrier. Identity claims now use unique constraints with rollback-safe conflict resolution, and a maintained normalized name column provides an index condition usable by the restricted role. Tenant checks remain enabled.

## Existing-data migration

A copy of the pre-migration Go database was upgraded from schema 4 to 5. Before/after counts and sorted ID hashes matched for **245 Leads, 3,276 workbook rows, 6,364 enrichment cells, and 33 jobs**. Historical enrollment, send, and delivery tables were empty in this copied dataset; seeded integration tests cover their compatibility and deduplication cases. The original Python and original Go databases were not migrated.

Migration audit CSVs, browser screenshots, and local HTTP benchmark artifacts are under the worktree's ignored `.run/` directory. They contain local test data and are not committed.

## Practical limits

CSV carries values actually exported by Clay, not unavailable object contents, credentials, credits, or executable workflow definitions. Record JSON backup covers shared records and saved record workflows, not a complete application/database backup. Browser backup restore is limited to 100 MB and requires an empty workspace. Paid provider calls, real CRM delivery, and external outreach sending were not performed during this validation.
