# Provider settings: reviewed outcomes and compact administration

Provider updates previously committed fields independently. A database trigger
rejecting the model reproduced a failed API response after the key and endpoint
had already changed. Provider updates now encrypt all secret values before one
transaction writes the submitted key, endpoint, model and default selection. A
failed write rolls back every field. The API response shape and existing
settings/integrations fixtures remain unchanged. No migration is required.

The UI distinguishes refused requests from unknown outcomes. Server errors,
lost responses and malformed or foreign receipts retain the draft and never
claim that nothing changed. **Check saved settings** reads current values without
another write. A matching model/default can be reconciled; a masked key cannot
prove which key was saved. Users can explicitly replace it or discard the draft.
Discarding never claims to undo a saved change.

Confirmed receipts settle drafts independently of a failed follow-up read. Only
an explicit success receipt for the requested provider is accepted. Blank model
input writes the displayed default explicitly, avoiding an assumption about
environment fallback. Tests use saved settings, disclose possible provider usage,
and remain disabled while drafts or unknown writes need attention. Editing clears
an earlier connection result. Pending operations protect context, duplicate
requests are blocked, and completion/recovery/discard preserve keyboard focus.

Provider editing and its client contract now live in feature-owned modules.
Search and configuration filters preserve hidden drafts; default and configured
providers appear first. Settings uses the shared compact page heading. Actions
wrap on narrow screens, mobile/touch inputs are at least 16px, reveal buttons and
form controls have 44px touch targets, and section tabs grow for touch input.

## Verification

[results.json](results.json) records the exact embedded index and source hashes;
raw browser reports, native logs and screenshots are adjacent. Frontend tests:
150 passed / 476 assertions; lint, TypeScript/production build and embedded Go
API build passed. The served index matches the production build byte for byte.

- Provider workflows pass in Chromium and WebKit, with 12 axe states per engine
  and zero page errors/violations. They cover partial/unknown failures, failed
  recovery reads, lost committed model/default responses, wrong receipts,
  validation refusals, filter/draft ownership, explicit discard, view-only access,
  connection-result validation, and real touch contexts at 320/390/768px.
- The surrounding workspace/session/draft suite passes with eight axe states.
  Root entry/reload checks pass signed in and signed out in both engines (four
  more axe states). The 42-state route/theme/viewport matrix also passes with
  zero violations or page errors: 78 automated axe states in this increment.
- Native race tests pass for `settings`, `secrets` and `api`; vet passes for these
  packages. Five API integration scenarios, including recorded settings fixtures
  and encryption checks, pass against a disposable migrated database. The
  transaction failure regression was observed red before the fix.
- Both backend-only patches were applied to exact PR #115 head in an isolated
  worktree. Race unit tests and two native integration scenarios (atomic failure
  and recorded settings/integrations contracts) pass there using schema version
  4. Database fixtures never ran against the runtime database.

Provider browser writes/tests were intercepted with synthetic values; no real
credentials or external calls changed. A local compiler-cache space limit
interrupted extra candidate checks; terminal failed processes were checked and
the successful serial reruns above are retained as the evidence.

This increment closes the provider multi-field write defect. Other administration
forms, manual assistive technology, performance and rollout remain open in the
[completion audit](../completion-audit.md). The backend fixes are local;
[PR readiness](../go-pr-readiness.md) still requires fresh remote CI and cutover
evidence.
