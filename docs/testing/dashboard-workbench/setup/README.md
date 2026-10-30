# Reviewed destination setup and home entry

Destination creation now covers all 12 supported types through configure,
review and receipt. Fields have visible labels and connected help. Per-type
values survive switching, failed reads and refused saves. Dirty drafts use the
shared route/workspace/sign-out guard; pending writes protect their context.

Review reads fresh scope, schedule, permission and catalog information. Enabling
future automatic activation and advertising consent require separate explicit
acknowledgements that reset when the review changes. Full endpoint inspection is
available without exposing the path/query by default. A saved configuration is
never presented as proof of connection or delivery. A lost response retains the
draft and reconciles an exact saved match without automatically sending again.

Verification records two distinct embedded builds in [results.json](results.json):

- Destination build: 143 frontend tests / 441 assertions, lint and production
  build passed. There were 120 automated axe states with zero violations or page
  errors: 26 Chromium setup, 26 WebKit setup, 6 inbound/ad trust, 15 destination
  run, 5 audience and 42 route/theme/viewport states. A separate 100-cycle rapid
  reopen check passed after reproducing and removing competing opening focus.
- Later home-entry build: anonymous `/` redirects to `/login`; signed-in `/`
  redirects to `/overview`. Reload and four axe checks across Chromium/WebKit
  passed without page errors or failed assets. The root router previously lacked
  an index child and rendered an empty outlet despite healthy assets/API. A login
  GitHub link accessible-name mismatch found by this check was also repaired.

Raw reports are adjacent JSON files. Screenshots are in [captures](captures).
The 120-state result belongs to the destination build before the later root/login
repair; it is not represented as a full rerun on the later asset snapshot.
Repeatable scripts are `scripts/qa/dashboard_destination_setup.py` and
`scripts/qa/dashboard_entry.py`. Supply credentials through their environment,
and run workspace-changing suites sequentially. Native create checks used
disabled webhook destinations and did not send external deliveries.

The destination work also reproduced a native timestamp defect: naive insert
defaults followed the database session timezone while updates used UTC. Explicit
UTC inserts now pass the three-zone regression, including Asia/Kolkata and
America/New_York. Current-worktree integration checks and an isolated candidate
on the exact PR head used disposable migrated databases, never the runtime DB.
Historical rows were not rewritten. Native logs are retained here.

This increment does not close the remaining manual assistive technology,
performance, route, administration or rollout requirements in the
[completion audit](../completion-audit.md), or establish
[Go PR merge readiness](../go-pr-readiness.md).
