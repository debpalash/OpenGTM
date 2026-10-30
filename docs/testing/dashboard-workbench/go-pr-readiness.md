# Go PR #115 readiness checkpoint

Checked on 2026-10-29. [PR #115](https://github.com/debpalash/OpenGTM/pull/115) is **merged** into main at [`e31afa9f8bb4292c70b198806ea0a685241c2e96`](https://github.com/debpalash/OpenGTM/commit/e31afa9f8bb4292c70b198806ea0a685241c2e96), merged at 15:47:47 UTC. The backend fixes are `f6abd818`; README screenshots/GIFs are `0d973725`. [All final-head CI checks passed](https://github.com/debpalash/OpenGTM/actions/runs/40900774967). The public main README and demo assets were verified after merging. Production cutover and deployed Docker staging remain separate prerequisites; no release tag or deployment was performed.

## Completed merge preparation

The isolated 18-file backend commit fixes person-dependent provider cache collisions, known zero-cost native-provider metadata, Darwin/Linux filesystem block-size portability, UTC destination inserts, and atomic encrypted provider-settings updates. It adds regression coverage and updates the cutover and image-naming runbooks. No dashboard redesign, canonical-record schema, Clay interchange or migration 5 is included.

[Fresh full CI](https://github.com/debpalash/OpenGTM/actions/runs/40882215931) completed successfully on this exact SHA at 2026-10-29 13:50:31 UTC:

- Go vet, tests and race integration tests with PostgreSQL and Redis: passed.
- Frontend lint, tests and build: passed.
- Browser-service lint and tests: passed.

Targeted local race tests passed for `providerrun`, `vendorcatalog`, `sysfiles`, `settings`, `secrets`, `api`, `deststore` and `destvalidate`; generated SQL matches its queries (`go tool sqlc diff`). Disposable schema-version-4 integration checks cover destination CRUD, tenant/role boundaries, UTC/Asia-Kolkata/America-New-York timestamps, provider-setting rollback after an injected write failure, and recorded settings/integration contracts. See [setup evidence](setup/README.md) and [provider evidence](providers/README.md).

Release image naming is documented in the pushed runbook: publish explicit `ghcr.io/debpalash/opengtm-{api,worker,mcp,migrate,scheduler,browser}` images, pin releases, and preserve the legacy plain `ghcr.io/debpalash/opengtm` alias. No images or release tags were published during preparation.

## Native cutover/restore evidence and deployment gate

A local native compatibility rehearsal now passes. Its report is `.run/go-merge-rehearsal/results.json`, and the repeatable local driver is `.run/go-merge-rehearsal/run.py`.

The fixture uses the PR's versioned `e7b2c4d6f8a0` reference schema and tracked Python source `b8bfc57e`, which has PostgreSQL workspace metadata support. It does **not** represent upgrading an older Alembic database to that revision. No production migration history was altered.

The rehearsal verified Python login and workbook-row creation; producer shutdown with no processing Python jobs; verified PostgreSQL and configured data-directory backups; Go adoption through schema 4; the cutover runbook's tenant/executor backfill; legacy Python JWT authentication in Go; completion of native Go workbook jobs; and backup restoration followed by Python login/workbook access. Restored jobs returned to their pre-cutover Python executor and NULL tenant, and the restored database had no Goose version table. The configured data directory contained one synthetic marker file; its restored digest matched. All owned scratch databases, runtime role and service processes were cleaned up. Fixture restore and Python restart took 1.83 seconds; this is not a production recovery estimate.

A deployed Docker staging rehearsal with the installation's actual data and previous Python release remains a **production cutover prerequisite**. No Docker/Podman CLI or configured deployed staging environment is available here. The runbook's historical Python commit `86f41c6` and matching latest Alembic tooling remain unavailable in this clone. Browser/container topology and upgrading older Alembic heads were not validated by the native fixture.

The code/README merge is complete after final-head CI passed. Production deployment, image publication and a release tag are outside this action. The README explicitly records upgrade preconditions and the staging/backup requirement. The dashboard goal is paused at the user's request; the README's two GIFs and theme-aware screenshots are clearly labeled as development previews, and no local dashboard implementation or schema 5 is bundled into PR #115.

The older local patch exports, [known fixes](go-pr115-known-fixes.patch) and [provider atomicity](go-pr115-provider-atomic.patch), are historical preparation artifacts. The pushed commit is authoritative and additionally includes cost-regression tests and runbook updates.

Companies/People, migration 5, Clay interchange and the dashboard workbench remain separate local work. The live local dashboard and its database were not used as destructive rollback fixtures.
