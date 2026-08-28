# OpenGTM G1–G7 validation report

Date: 2026-08-28

Release decision: **do not publish yet**

## Outcome

| Gate | Result |
| --- | --- |
| Machine score | 100/100 |
| Category floors | 6/6 passed |
| Workflow coverage | G1–G7 passed |
| Hard failures | 0 |
| Local-native streak | 10/10 |
| Controlled-live streak | 0/10 |
| Release eligible | No |

The implementation clears the 95% target in native local validation. Publishing remains blocked only by the controlled-live streak. Recorded provider responses are not represented as production evidence.

## What the ten local runs exercised

Each run started with fresh databases and executed production action code through approval, persistence, readback, and scoring:

| Workflow | Native evidence exercised |
| --- | --- |
| G1 account discovery | Structured brief, approval, idempotent workbook action, saved account rows, exact source-run counts, and retry reuse |
| G2 people research | Canonical company identity, stable person IDs, exact company/function evidence, and normalized result state |
| G3 contact enrichment | Exact saved selection, finder attempt, independent verifier attempt, normalized address status, and retry reuse |
| G4 verification | Independent claims, public evidence, observation time, confidence, contradictions, and summary counts |
| G5 workbook creation | Exact selected people, approval, persisted readback, correct link, no duplicate rows, and idempotent retry |
| G6 signal tracking | Exact saved account IDs, four collectors, one durable schedule, collector health, recovery metadata, and retry reuse |
| G7 outreach draft | Exact saved contact, verifier eligibility, sentence-level evidence, persisted draft readback, no send row, and retry reuse |

Provider network responses were deterministic recordings. Everything after that seam used the application’s native code and persisted state.

Final repository verification also passed: 1,205 backend tests with 119 skipped, frontend lint, frontend production build, clean-database migration to the single head, and an Alembic schema-drift check.

## Release-gate hardening

The scorer no longer trusts `production_like: true` by itself. A controlled-live run counts only when it records all of the following:

- a unique run ID, build SHA, finish time, and dedicated workspace;
- `mode: live` and `validation_tier: controlled_live`;
- score at least 95 with every category floor met;
- G1–G7 all passed;
- no hard failure or unresolved P0/P1 issue;
- external sends blocked; and
- provider health recorded.

The current artifact must itself be the latest controlled-live run and match that record's run ID and build SHA. Duplicate run IDs or an incomplete latest record break the consecutive streak. Local-native history has a separate counter and cannot make a release eligible.

## Local environment audit

- Frontend is running at `http://127.0.0.1:4099`.
- API health is green at `http://127.0.0.1:8000/health`.
- The background worker is running.
- The development SQLite database is at Alembic head `0b1c2d3e4f50`.
- Authenticated `GET /api/outreach/drafts` returns 200.
- Intent Watches are disabled in the current runtime.
- The required exact contact finder/verifier chain is not configured for a full live G3/G7 pass.

Those last two points make a genuine full controlled-live run impossible on this machine today. No synthetic flag was used to bypass them.

## UI verification

The grounded-draft receipt was reviewed at 1440×1000 and 375×812 in Chromium against the running app. Both viewports rendered the exact recipient, subject, body, draft-only state, personalized claim, observation time, confidence, and public source without horizontal overflow. The mobile shell now hides nonessential usage metrics instead of compressing them into unreadable columns. The temporary review draft was deleted after inspection.

## Commands

```bash
.venv/bin/pytest -q tests/test_gtm_gauntlet_all_workflows.py
.venv/bin/pytest -q
npm --prefix apps/web run lint
npm --prefix apps/web run build
```

Migration verification uses a fresh temporary SQLite database:

```bash
DATABASE_URL=sqlite:////tmp/opengtm-validation.db .venv/bin/alembic upgrade head
DATABASE_URL=sqlite:////tmp/opengtm-validation.db .venv/bin/alembic check
```

## Checkpoints

- G6 exact signal tracking: `3cccb72`, tag `checkpoint/opengtm-g6-signals-20260828`
- G7 grounded drafts: `9e82a4a`, tag `checkpoint/opengtm-g7-drafts-20260828`

The final validation checkpoint is recorded after the complete suite and worktree audit pass.

## Remaining publication gate

1. Provision a dedicated `gtm-release-eval` workspace.
2. Enable and verify the Postgres-backed Intent Watches runtime.
3. Configure the exact contact finder and independent verifier used by G3/G7.
4. Keep all external sends blocked.
5. Run the complete live G1–G7 gauntlet ten consecutive times with unique records.
6. Publish only if every run scores at least 95, every workflow and category floor passes, and no hard failure or P0/P1 issue appears.
