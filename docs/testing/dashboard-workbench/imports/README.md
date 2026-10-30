# Import mapping and recovery verification

The local dashboard at port 8002 now separates file inspection from transactional
row preview. Duplicate suggested fields remain editable instead of preventing
operators from entering the mapping step. No schema migration is added.

## Implemented behavior

- Company, Person, Employment, Custom and Ignored groups show source samples,
  actual match provenance and destination ownership. Conflicting targets are
  marked on every affected column and block validation until corrected.
- Source samples are bounded and preserve numeric digits. Raw JSON is posted
  without a JavaScript number round-trip; CSV source IDs retain leading zeros.
  Source identifiers and employer-link warnings supplement row-level preview.
- Mapping inputs stay mounted while typing. Explicit regrouping changes their
  layout; Back navigation preserves the file, source and mappings.
- Preview never commits records. Import reports accepted and rejected rows;
  accepted rows may create or update records, so counts do not imply new records.
  The rejection display is bounded to 50 rows, with a complete downloadable report.
- Uncertain writes retain one batch key and lock its source/mapping. A missing
  receipt remains unknown. Reload recovery performs a read and validates the
  receipt before reporting success. Only bounded metadata is stored in session
  storage; contact files and credentials are not stored there.
- Blocked browser storage is disclosed. The open tab can recover its receipt;
  confirmation clears the obsolete storage warning. Pending work protects context
  changes and closing; dirty drafts have an explicit leave decision.
- Results open accepted records and can select their unique IDs for optional
  enrichment. Import starts no enrichment. The existing linked-workbook flow
  separately reviews providers and cost before a run.
- The task has a scrollable body and visible footer at 320px and short laptop
  heights. Touch controls reach 44px, step changes move focus to the heading, and
  Escape preserves the mapping when the operator chooses Keep editing.

The feature logic, mapping view, results and recovery metadata are extracted into
`apps/web/src/features/records/import/`. The dialog composes those modules through
the application-owned primitive boundary.

## Evidence

[results.json](results.json) records exact source fingerprints and the embedded
index hash. Both browser reports identify that same live build:

- [Chromium](chromium/results.json) and [WebKit](webkit/results.json): 13 full-document
  axe scans each, zero violations and zero page errors. Cases include conflicts,
  partial review, unknown writes, lost response plus reload, unavailable receipts,
  blocked storage, short heights, dark mode, 320px reflow, coarse-pointer controls
  and text-spacing overrides. Dark mode is selected through Appearance so both
  theme tokens and portaled component context agree.
- [Native workflow journey](journey/journey-results.json): import Companies/People,
  partial rejection and receipt recovery, provenance/editing, employment, exact
  large identifiers, reviewed native enrichment, CSV/JSON exports and canonical
  overview counts passed. Its ten accessibility states have no violations.
- [Frontend tests](import-tests.log): 169 tests, 565 assertions, zero failures.
  [Lint](import-lint.log) and [production build](import-build.log) passed.
- [Go race unit checks](import-go-unit.log) passed for records and API.
  [Records integration checks](import-records-targeted.log) passed against generated
  disposable databases, with the explicit large-case exclusion described below.

Repeatable browser checks are `scripts/qa/dashboard_import_mapping.py` and
`scripts/qa/dashboard_journey.py`; credentials are supplied through environment
variables. Synthetic workspaces are isolated and the prior active workspace is
restored. Mapping checks run no enrichment or external delivery; the journey runs
the reviewed native scoring workflow.

## Open scale and release requirements

The broad records integration invocation exceeded its six-minute limit in
`TestTenThousandRowImportPreviewCommitAndReplay`. The [failure log](import-records-integration.log)
is retained, and its disposable database was removed after the test process ended.
The follow-up passing suite explicitly excludes this case. This is an unresolved
large-import performance finding, not evidence that the full suite passed or that
10,000-row preview/commit meets a latency target. Earlier native large-collection
evidence is retained in the collection increment; it does not erase this result.

The full restore/reimport matrix, wider role/network/browser coverage, workbook
virtualization/lifecycle checks, manual screen-reader verification, pilot and
deployment gates remain open in the [completion audit](../completion-audit.md).
These dashboard changes are local and separate from Go PR #115.
