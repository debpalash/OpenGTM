# Cross-plan test-hygiene sweep (Go integration suite)

Commit: 3bfd21ee `test(go): drop leftover workbook_rows 9001 from worker refresh e2e; sweep orphan SCIM users`

## Fixes (root cause -> fix)

1. cmd/worker TestRefreshJobEndToEnd (main_integration_test.go): inserted `workbook_rows` id 9001 (ws `wsr`) and never deleted it.
   internal/entities seeds 9001 in `ws-vec` -> `duplicate key workbook_rows_pkey` (10 entities tests failed on a fresh clone after cmd/worker ran; reproduced in run 1).
   Fix: let the sequence assign the id; t.Cleanup deletes the row and workbook.
   The residue row (9001 / wbr / wsr / `{}`) was removed from opengtm_test (clearly this test's residue; the `wbr` workbook row was left).
2. internal/api TestRefreshPolicyBodyLimitAndReadError (duplicate workbook_rows_pkey in a broad run): same root cause. With a leftover 9001 and
   `workbook_rows_id_seq` just below it, seedEnrich's serial inserts collide; reproduced deterministically (leftover 9001 + setval 8999 -> pkey error).
   Fixed by (1); no change to that test.
3. internal/api SCIM TestUsersMatchFixtures (order/reuse dependence): `POST /scim/v2/{slug}/Users` inserts the global `users` row before the
   workspace-scoped checks (Python parity), so a 409 externalId collision leaves an unlinked user (`frank`, `t13-other`) that per-workspace cleanup
   (which finds users via workspace_scim_users) never removes. Observed as leftovers after a full internal/api run. Fix: newSCIMEnv snapshots users ids
   and its cleanup deletes any users row created by the test (safe: NewAppPool's cross-package advisory lock serialises these tests).
   The test passed in the clean run-1 full api run, so the original failure could not be re-observed; the leftover-user leak is the evidenced cause.
   Not changed in production (Python-compatible orphan user on 409 is existing behaviour; flag if the owner wants it in the tx).

## Static audit (explicit-id INSERTs)
Scanned all non-plan-14 *_test.go for `INSERT INTO <serial table> (id,...)`. Tests that mix explicit ids with code-under-test serial inserts already
advance the sequence (setval GREATEST(...)) after earlier fixes. Remaining explicit-id seeds without setval (small ids 1-9, 9001-9113, 800xxxx, 900000+)
either clean up after themselves or never share a table with serial inserts in the same test; none failed on a fresh or reused clone in three full
runs, so they were left unchanged. Tests that deliberately rewind (`setval(..., 1, false)`: entitiesroutes_write, destsinbound, leads_contract,
jobsread) are fixture-id tests; they pass on fresh and reused DBs.

## Clone prerequisites discovered
- A schema-only clone of opengtm_test (`pg_dump -s`) must also carry the single `alembic_version` row (`d3f5a7b9c1e2`); cmd/mcp and the worker queue
  refuse to start without it (`queue: read alembic_version: no rows` -> 4 cmd/mcp failures in run 1). Not a test bug; document in clone recipes.
- Snapshot (`git archive HEAD`, GOFLAGS=-buildvcs=false) needs `legacy/` copied in (untracked, read by fernet/catalog/providers/schema_revision tests,
  `uv run` in legacy/python). docs/, infra/, scripts/ are tracked, so they are already in the archive. No test required .git of the snapshot.
- Redis: TEST_REDIS_URL=redis://127.0.0.1:56380/11 (db 11/12 used by this sweep).

## Final results (fresh clone hyg_sweep_c, snapshot of 3bfd21ee, `-race -tags integration`, TZ=UTC)
| Run | Command | Result |
|---|---|---|
| A (fresh) | `go test -race -tags integration -p 1 ./...` | 114 ok, 0 FAIL, 6 no test files, exit 0 |
| B (reused same DB) | `go test -race -tags integration ./...` (default parallelism) | 114 ok, 0 FAIL, exit 0 |

Earlier baseline run 1 (HEAD before fixes, `-p 1`, fresh clone): 111 ok; failures: cmd/mcp (missing alembic_version row, clone issue),
internal/entities (9001 leftover, fixed), internal/leadprogress (plan 14 uncommitted work in progress at the time; passed in A/B at 3bfd21ee).

## Remaining failures
None. Note for plan 14 owner: leadprogress progress_integration_test.go had an uncommitted modification in the shared tree; it passed from HEAD.
