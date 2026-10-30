# Destination workbench verification

This archive records the activation increment tested before the dense workstation
revision. [Results](results.json) retain the observed outcomes; the archive does
not identify a final release build. Current layout evidence is kept separately in
[Dense workstation verification](../workstation/README.md).

Audience sync, retry and cancellation now require a review of scope and target.
Fresh preflight reads verify effective permission and reject a changed review.
Writes are sent once; an uncertain response directs the operator to history.
A confirmed receipt survives failed follow-up reads. Queued, running, cancellation
requested and cancelled outcomes remain distinct. History pages runs and delivery
receipts separately and links deliveries to canonical company/person inspectors.

Membership refresh, resume and interval changes review enabled destinations
because those operations can enqueue delivery. Pausing a schedule does not claim
to cancel existing runs. Inbound token rotation keeps its issued token available
independently of history/clipboard failures and respects effective write access.

The native access route uses the same effective `activation.write` policy as the
mutation routes, including overrides. Delivery responses preserve legacy positive
lead identities and expose canonical company/person identities without inventing
a zero lead ID. No schema migration was introduced for this increment.

Recorded checks passed:

- 131 frontend tests, lint and production build.
- 99 Chromium axe states across destination runs (15), audience members (5),
  inbound tokens (6), context protection (8), inspectors (13), native journey
  (10) and route/theme/viewport matrix (42), with zero recorded violations or page
  errors. Outbound activation mutations use controlled responses; these browser
  checks do not demonstrate live third-party delivery.
- Ten native API integration scenarios with the race detector in a disposable
  migrated database, including role/tenant isolation, effective overrides,
  canonical delivery identity, legacy fixture parity, paging/filter bounds,
  sync/job creation, retry and cancellation. Normal API/store tests and vet passed.

Repeatable scripts are `dashboard_destination_runs.py`, `dashboard_audiences.py`,
`dashboard_activation_trust.py` and `dashboard_context.py` under `scripts/qa/`.
Use `--help`; run workspace-switching checks sequentially. Native contract tests
live in `apps/backend-go/internal/api/destsworkbench_integration_test.go` alongside
the existing destination contract tests.

Destination creation review/drafts, remaining activation routes and manual browser,
assistive-technology and release gates remain in the
[completion audit](../completion-audit.md).

[Confirmed receipt with history unavailable](captures/queued-history-error.png) ·
[320px review](captures/review-320.png) ·
[390px review](captures/review-390.png)
