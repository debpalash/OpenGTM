# Workspace and activation trust verification

Tested locally on 2026-10-29 against the production UI embedded in the Go API at
`http://localhost:8002`. The served index matches the frontend build byte for
byte. [Results and fingerprints](results.json) identify the tested working tree,
workflow outcomes and build assets. All records are synthetic.

Delivered behavior:

- Record/provider drafts share route, reload, workspace and sign-out protection.
  Provider section changes request a decision before losing a form. Registered
  saves cannot be discarded while pending. Explicit discard releases the old
  form; cancelled decisions retain it.
- Workspace headers remain bound to the tab's displayed workspace. Switching
  another tab does not redirect an existing tab's requests. Denied switches keep
  the current workspace and data. The workspace manager uses the same auth switch
  and cache reset as the shell.
- Explicit sign-out is guarded. A 401 or cross-tab session change clears private
  views and cancels outstanding leave decisions immediately.
- Instance-provider controls distinguish global admin from workspace role.
  Failed reads, malformed responses and failed saves remain recoverable.
  Confirmed saves settle their draft baseline even if the follow-up read fails.
  Agents settings link to saved workflow configuration instead of fake switches.
- Token rotation requires review. A successful token remains available if the
  independent history read fails. Clipboard failure never claims success; the
  operator can retry or acknowledge manual storage. Unsaved tokens are guarded
  before closing or leaving. Secrets stay in memory.
- Advertising destinations require an unchecked, explicit consent attestation
  scoped to the selected audience and destination type.

Verification includes 120 passing frontend tests, lint, TypeScript/production
build and the Go API build with embedded assets. The browser reports cover the
new context and activation states plus the full route matrix, inspector,
import/enrich/export journey and canonical audience navigation. 84 automated scans returned zero violations. Scans
are recorded per state without blanket exclusions; they do not certify manual
assistive-technology or full browser conformance.

The new scripts are `scripts/qa/dashboard_context.py` and
`scripts/qa/dashboard_activation_trust.py`; run `--help` for inputs. They use native
Go workspaces and records. Provider writes/tests and token rotation responses are
intercepted with synthetic responses, including controlled errors and delays;
no external delivery or real provider credential changes occur. Callback reads
are intercepted to exercise unavailable, malformed and successful responses.
Workspace changes use the native API; prior active workspace is restored in
`finally`. Run these scripts sequentially with other workspace-switching checks.

External sync/retry/cancel review and receipts, destination creation redesign,
remaining administration forms and the broader permission/browser/state matrix
remain open in the [completion audit](../completion-audit.md). Manual AT, actual
browser zoom, performance budgets, operator pilot and staged rollout also remain
unproven. The full transformation remains active.

[Token retained after a history error](captures/token-history-error.png) ·
[Mobile draft guard](captures/mobile-guard.png)
