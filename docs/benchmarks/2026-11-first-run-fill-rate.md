# First-run fill rate, November 2026

Question: on a clean install of the Go runtime with no paid keys and no LLM key, what share of the demo
workbook's cells fill, per column, and why does each unfilled cell stay empty? The last measurement was 31% in
July on the Python version. The target is 80%.

Short answer: **53.5% (107/200 enrichment cells) in the first full run, 44.0% (88/200) in an independent
second install.** Neither reaches 80%. About half of the empty cells are two columns that cannot fill on a
zero-key install as seeded; the other half are cells that a free provider could have filled but did not,
because one 10 second timeout puts that provider on a 3 minute cooldown for the whole workspace.

## After the fixes (branch `fix/first-run-fill-rate`, builds on PR #175 and PR #178)

Seven fixes (one commit each, test first): a process-wide browser limiter sized from `BROWSER_MAX_CONCURRENCY`
whose wait does not count against the provider timeout; a cooldown that keeps 300 s for a rate limit but needs 3
consecutive timeouts for a timeout; a failed re-run keeps the old value; Email Verify reads the row's Email cell
and stores the verifier's answer; specific empty-cell reasons; an honest seed description; and three separate
numbers in the script. Measured on two fresh installs of the fixed code (`scripts/bench/first-run-fill-rate.sh`).

| Number (200 enrichment cells) | Before, install 1 | Before, install 2 | After, install 1 | After, install 2 |
| --- | --- | --- | --- | --- |
| All filled | 107 (53.5%) | 88 (44.0%) | 121 (60.5%) | 134 (67.0%) |
| Excluding guessed `ddg_email` role addresses | 89 (44.5%) | not computed | 115 (57.5%) | 131 (65.5%) |
| Excluding the key-requiring column (HQ Location, 175 cells) | 107/175 (61.1%) | 88/175 (50.3%) | 121/175 (69.1%) | 134/175 (76.6%) |

The three numbers are never combined. **Neither install reaches 80% excluding key columns (69.1% and 76.6%).**
The spread between the two after-installs is 7.5 points on the same code, so quote a range. Two installs are not
a confidence interval. The before-install columns were computed from the phase-1 data with the same rule.

Per column, filled out of 25 (before is the phase-1 install 1 / install 2):

| Column | Before | After install 1 | After install 2 | Empty reasons after (install 2) |
| --- | --- | --- | --- | --- |
| Description | 25 / 25 | 25 | 25 | none |
| LinkedIn | 23 / 22 | 22 | 23 | timeout x2 |
| Email | 24 / 13 | 19 | 21 | timeout x4 (3 of them skipped by cooldown) |
| Hiring Signals | 17 / 15 | 15 | 15 | `no_data` with cooldown skip x9, timeout x1 |
| Size | 11 / 11 | 11 | 17 | timeout x5, `no_data` x3 |
| Phone | 7 / 2 | 10 | 12 | timeout x12 (cooldown skip), `no_data` x1 |
| Email Verify | 0 / 0 | 19 | 21 | `upstream_dependency_failed` x4 (no Email to verify) |
| HQ Location (research) | 0 / 0 | 0 | 0 | "No LLM provider is configured: add a key in Settings" x25 |

What changed and what did not:

- Email Verify went from 0 to 19 and 21 cells. **All 40 are the verdict `unknown`** (the cascade answered, no
  SMTP verdict could be reached from this host; port 25 reachability was not measured separately). They count as
  filled in all three numbers because the verifier did answer; without them install 1 is 102/175 (58.3%) and
  install 2 is 113/175 (64.6%). A guessed address is no longer reported as verified.
- A re-run in install 1 lost 0 cells and gained 16 (137/200), against 18 lost before.
- Empty cells now carry a reason: every HQ Location cell says to add an LLM key; skipped providers say
  "cooling down"; a cell whose Email is empty says so for Email Verify.
- The run got slower, 551 s and 583 s against 296 s and 169 s, because browser calls now queue on the limiter
  instead of failing early. Peak RAM was 772 and 708 MiB, comparable to before.

### Remaining causes of the gap to 80% (evidence)

1. **Real provider work still exceeds the 10 s timeout.** The worker logged 137 (install 1) and 144 (install 2)
   `provider timed out` warnings, more than the 80 before, mostly `deep_scraper` (36 and 37) and
   `website_scraper` (34 and 34), which make sequential browser calls inside one provider call. The limiter wait
   is excluded from the clock, so these are not queueing; this is inferred from the call structure and not
   proven by a per-call trace. Cooldown skips remain: Phone is 12 of 13 empty cells `timeout (provider cooldown
   skip)` in install 2, so 3 consecutive timeouts is still reached because a whole column of rows runs
   concurrently. Not measured: the result with a 30 s provider timeout (not changed here because the task did
   not ask for it, and doing it to pass the benchmark would be tuning).
2. **Search engines block the free providers.** Browser log, install 2: `search.brave.com` 119 fetches,
   `www.mojeek.com` 117 fetches (install 1: 105 and 101, of which 105 x 429 and 87 x 403 are the blocked
   answers); `search.yahoo.com` answers. Phase 1 showed the same pattern.
3. **Email Verify has no SMTP verdict.** 40 of 40 filled verify cells are `unknown`; the 4 and 6 empty cells are
   `upstream_dependency_failed` because Email stayed empty (timeouts).
4. **Hiring Signals** is capped by cooldowns on `ats_hiring` and `jobspy` (9 and 8 `no_data` with cooldown skip).

The ceiling from this data is the number above; it is not tuned. Fix 1 and fix 2 removed the first-run data
loss and the single-timeout cascade, and raised the first-run rate by 7 to 23 points, but the remaining timeouts
need a separate decision (provider timeout per provider, or fewer sequential browser calls in `deep_scraper`).

## Round 2 (branch `fix/first-run-fill-rate` at `baa6b664`): 82.0% and 82.5% all filled

Round 1 (above) is kept as measured. Round 2 adds, on top of it: a 30 s budget for target-site providers and
one site walk per run for `deep_scraper` and `website_scraper`; the cell and run outcome model (`filled`,
`no_data`, `needs_setup`, `error`; run status `complete`, `completed_with_errors`, `failed`); search backends that
cool down after a 429 or 403; the Email Verify root-cause fix (below); and a stricter filled rule and a fourth
number in the script. Measured with `scripts/bench/first-run-fill-rate.sh` on two fresh installs run one after
the other (2026-11-19 08:33 to 08:43 host clock), commit `baa6b66470e94312f4585625002cd71cbc36d430`, no paid keys, no
LLM key, both runs `completed` with 0 re-runs. Work files: not kept in the repo.

### Four rates (200 enrichment cells each)

| Rate | Original (phase 1) install 1 / 2 | Round 1 install 1 / 2 | Round 2 install 1 | Round 2 install 2 |
| --- | --- | --- | --- | --- |
| All filled | 107 (53.5%) / 88 (44.0%) | 121 (60.5%) / 134 (67.0%) | **164 (82.0%)** | **165 (82.5%)** |
| Excluding guessed `ddg_email` role addresses | 89 (44.5%) / not computed | 115 (57.5%) / 131 (65.5%) | 154 (77.0%), 10 removed | 154 (77.0%), 11 removed |
| Excluding the key-requiring column (HQ Location, 175 cells) | 107/175 (61.1%) / 88/175 (50.3%) | 121/175 (69.1%) / 134/175 (76.6%) | 164/175 (93.7%) | 165/175 (94.3%) |
| Excluding environment-limited columns | not computed | not computed | 164/200 (82.0%), none left out | 165/200 (82.5%), none left out |

The four numbers are never combined. The rule for "filled" changed between rounds (commit `baa6b664`): a cell is
filled only when it carries an informative value. The round 1 numbers used the earlier rule, in which the 40
Email Verify cells whose verdict was `unknown` counted as filled; under the round 2 rule they would not, so the
round 1 and round 2 columns are not strictly like for like. Round 2 has 0 `unknown` verdicts counted.

The two installs differ by 0.5 points (1 cell) on the same code; round 1 differed by 7.5 points and the original
by 9.5. Two installs are not a confidence interval and both ran in the same 10 minute window from the same
network, so quote 82% as "about 82% on this host, this day", not as a guaranteed figure. Excluding the 19
`catch_all` verify cells in each install (the domain accepts any mailbox, so the cell does not confirm the
address) would give 145/200 (72.5%) and 146/200 (73.0%); that is computed from the saved cell files, not a
script number, and the script counts `catch_all` as filled.

### Per column, filled out of 25

| Column | Original (1 / 2) | Round 1 (1 / 2) | Round 2 install 1 | Round 2 install 2 |
| --- | --- | --- | --- | --- |
| Description | 25 / 25 | 25 / 25 | 25 | 25 |
| LinkedIn | 23 / 22 | 22 / 23 | 25 | 25 |
| Email | 24 / 13 | 19 / 21 | 25 | 25 |
| Hiring Signals | 17 / 15 | 15 / 15 | 24 | 23 |
| Size | 11 / 11 | 11 / 17 | 23 | 23 |
| Phone | 7 / 2 | 10 / 12 | 19 | 20 |
| Email Verify | 0 / 0 | 19 / 21 (all `unknown`) | 23 | 24 |
| HQ Location (research) | 0 / 0 | 0 / 0 | 0 | 0 |
| **Total** | 107 / 88 | 121 / 134 | **164** | **165** |

### Email Verify now returns real statuses

Root cause of the zero in the original and the all-`unknown` in round 1: the cascade's SMTP verifier was built
with the empty `Deps` of `NewDeps`, so no SMTP probe was ever sent (fix `9efeb946`). Statuses stored in
`cell_metadata.verify.status`, counted from `run1_cells.psv` of each install:

| Status | Install 1 | Install 2 |
| --- | --- | --- |
| `valid` | 3 | 5 |
| `catch_all` | 19 | 19 |
| `invalid` | 1 | 0 |
| `unknown` (cell empty, `no_mx`, reason shown) | 2 | 1 |

So SMTP port 25 was reachable from this host for most addresses (previously "not measured"; it is now measured
for this host only). Most results are `catch_all` (19 of 25), which says the domain accepts anything. The 3 empty
cells are `email_inconclusive` with `why: no_mx`; one of them is `info@notion.so` (a guessed role address) in
install 1, while install 2 verified a different Notion address as `valid`.

### Time and memory

| Measure | Install 1 | Install 2 |
| --- | --- | --- |
| `up` returns | 28 s | 28 s |
| Run duration | 192 s | 229 s |
| `up` to run complete | 222 s | 259 s |
| Image build (separate; install 2 reused cached layers) | 66 s | 11 s |
| Peak RAM, sum of containers (sampled every 10 s) | 714 MiB | 698 MiB |
| Browser container peak | 518 MiB | 498 MiB |

### Remaining causes of empty cells (round 2)

Empty cells excluding HQ Location: 11 in install 1, 10 in install 2 (all `status=error` with a reason).

1. **HQ Location needs an LLM key, by design: 25 cells in each install (25 of the 36 and of the 35 empty cells).**
   Reason on every cell: "No LLM provider is configured: add a key in Settings". Not recoverable without a key.
2. **Phone: 6 empty in install 1, 5 in install 2.** Reasons install 1: `jsonld_firmographics` response body
   exceeds 1048576 bytes x3, `ddg_company` no new data x1, `deep_scraper` no data from 0 pages x1,
   `local_business` no local business data x1. Install 2: `local_business` no data x3, `deep_scraper` x1,
   `jsonld_firmographics` body over 1 MiB x1. The 1 MiB cap on a response body is a product limit that hides
   a page from `jsonld_firmographics`; whether the phone is on those pages was not checked.
3. **Size: 2 empty in each install**: `deep_scraper` "No data extracted from 0 pages" x1 (both), plus
   `people_data_labs` "not configured" x1 (install 1, a key-requiring provider) or `company_intel` "no company
   intelligence found" x1 (install 2).
4. **Hiring: 1 empty (install 1), 2 (install 2)**: `jobspy` `no_jobs_found`, i.e. the source has no jobs for the
   company; probably a genuine absence, not checked.
5. **Email Verify: 2 and 1 empty**: SMTP `no_mx` (see above).
6. **Timeouts are rare now.** Worker log `provider timed out`: 3 in install 1 (`ats_hiring` 2, `email_harvester` 1)
   and 7 in install 2 (`ats_hiring` 3, `email_harvester` 2, `ddg_company` 1, `deep_scraper` 1), against 137 and 144
   in round 1 and 80 originally. No empty cell reports a timeout or a cooldown skip. This removes the cause that
   was largest in round 1.
7. **External search blocking is still there but no longer costs cells.** Browser log (fetches, install 1 / 2):
   `search.brave.com` 429 x1 / x1, `www.mojeek.com` 403 x2 / x1, `search.yahoo.com` 200 x113 / x124,
   `www.startpage.com` 200 x88 / x78. Each of those blocks produced a `backend ... cooling down for 300s` line
   (7 per install), so the backends are left alone instead of being retried 100 times as in phase 1
   (brave 105 x 429, mojeek 94 x 403). Which searches the cooldown prevented was not measured.

Guessed addresses: 10 (install 1) and 11 (install 2) of the 25 Email cells are `info@<domain>`-style guesses
from `ddg_email` with `email_confidence=generic`; they are the reason the second rate is 77.0%.

### What round 2 did not measure

- Any run with an LLM key, paid keys or a proxy; any second host or network; any re-run in round 2.
- Whether the SMTP results change on another egress (many networks block port 25).
- Correctness of filled values beyond the guessed-email and `catch_all` checks (no accuracy audit).
- Which unfilled phone pages hold a phone behind the 1 MiB body cap.
- Cold image build time.

## Before the fixes: the phase-1 measurement

Everything below is the measurement of origin/main before the fixes, kept as the baseline.

## Identity

| Field | Value |
| --- | --- |
| Date | Run 1 and re-run: 2026-11-18 23:33 to 23:46 UTC (container clock). Script run: 2026-11-19 05:15 to 05:18 (host clock) |
| Commit | `02e76239861006ac21893d729c1149fa3469159a` (origin/main, merge of PR #165) |
| Host | Linux 6.12, 44 GB RAM (other stacks running), Docker Compose v5.8.0 |
| Images | built from this checkout; layers were cached, so build time is not a cold-build number |
| Network | real outbound requests to free public sources from a residential/office egress, no proxy |

## Method

`scripts/bench/first-run-fill-rate.sh` reproduces everything below. It generates an env file and a compose
override outside the repo, brings up the stack under its own compose project (`opengtm-fillrate`) with the
`demo` profile, completes first-run setup through `POST /auth/setup` with the token from the api-go log,
lets `seed-go` seed the 25-row demo workbook and start the run through the API, polls the `run_workbook` job
in Postgres until it leaves `pending`/`processing`, reads `workbook_enrichments`, `provider_stats`, worker and
browser logs, and tears everything down in a trap. Only the secrets the stack refuses to start without are set;
no provider key, no LLM key, `APP_ENV=dev`.

Counting rule: the demo workbook has 10 columns. Two are `lead_field` columns (Company, Website) that are
seeded, so they are 50/50 by construction and excluded from the headline. The 8 enrichment columns are 25 rows
each = 200 cells. A cell counts as filled when `workbook_enrichments.value` is non-empty.

Runs:

1. Manual run on the stack, then one forced re-run (`SEED_FORCE_RUN=1`). Numbers in the main table.
2. A fresh install through the script (`FILLRATE_RERUNS=0`), run later the same day, as a variance sample.
   This is a second install, not a re-run; the task budget (one run plus two re-runs) was one run, one
   re-run, and this verification run of the script.

## Result: per column

| Column (target field) | Run 1 filled | Run 1 rate | Script run filled | Re-run of run 1 filled | Why the empty cells are empty |
| --- | --- | --- | --- | --- | --- |
| Description (`description`) | 25/25 | 100% | 25/25 | 25/25 | nothing empty |
| LinkedIn (`linkedin_url`) | 23/25 | 92% | 22/25 | 22/25 | 2 `no_data` with provider cooldown skips |
| Email (`email`) | 24/25 | 96% | 13/25 | 13/25 | 1 timeout (run 1); 12 `no_data` with cooldown skips (script run) |
| Hiring Signals (`hiring_signals`) | 17/25 | 68% | 15/25 | 18/25 | 8 cells: `ats_hiring` and `jobspy` both in cooldown |
| Size (`company_size`) | 11/25 | 44% | 11/25 | 11/25 | 13 `no_data` + 1 timeout, 14 of 14 with cooldown skips |
| Phone (`phone`) | 7/25 | 28% | 2/25 | 1/25 | 15 `no_data` + 3 timeout, 18 of 18 with cooldown skips |
| Email Verify (`email_verify`) | 0/25 | 0% | 0/25 | 0/25 | `mailscout` finds no email on the lead (see cause 2) |
| HQ Location (`research`) | 0/25 | 0% | 0/25 | 0/25 | `no_answer`: no LLM key |
| **All 8 enrichment columns** | **107/200** | **53.5%** | **88/200** | **90/200** | |
| Including the 2 lead-field columns | 157/250 | 62.8% | 138/250 | n/a | |

The run-1 and script-run numbers are the same code on the same host a few hours apart; the 9.5 point spread is
the run-to-run noise from live public sources, so quote the range, not a single number. Not enough runs were
done to give a confidence interval.

Which providers produced values (run 1, cells filled):

| Provider | Cells | Columns |
| --- | --- | --- |
| `wikidata` | 26 | description 17, company_size 3, linkedin 6 |
| `ddg_email` | 20 | email 20 (18 of them are the generic `info@domain` fallback, see cause 4) |
| `deep_scraper` | 27 | description 8, company_size 8, email 3, phone 1, linkedin 7 |
| `ats_hiring` | 12 | hiring 12 |
| `jsonld_firmographics` | 6 | linkedin 6 |
| `local_business` | 6 | phone 6 |
| `jobspy` | 5 | hiring 5 |
| `website_scraper` | 3 | linkedin 2, email 1 |
| `social_finder` | 2 | linkedin 2 |

Hit rates from `provider_stats` (cumulative over run 1 plus the re-run, so attempts include re-run attempts):
`wikidata` 100% on description/size/linkedin (17/17, 31/31, 15/15), `ddg_email` 20/21, `ats_hiring` 12/22,
`jobspy` 5/16, `deep_scraper` 6/13 email and 12/19 phone, `local_business` 10/23 phone, `ddg_company` 2/20 phone,
`facebook_pages` 0/19, `email_harvester` 0/9, `mailscout` 0/100 on `email_verify`.

## Time and memory

| Measure | Run 1 (manual) | Script run |
| --- | --- | --- |
| `docker compose up -d` returns (images cached) | 28 s | 31 s |
| Run duration (job created to `completed`) | 296 s | 169 s |
| `up` to run complete | 339 s | 203 s |
| Re-run duration | 157 s | not run |
| Peak RAM, sum of containers (sampled `docker stats --no-stream`, 28 samples run 1) | 765 MiB | 733 MiB |

Peak per container (run 1): browser 549 MiB, postgres 116 MiB, worker-go 66 MiB, nginx 16 MiB, api-go 15 MiB,
scheduler-go 11 MiB, redis 8 MiB. The 2 GB `BROWSER_MEM_LIMIT` was nowhere near reached. A cold image build was
not measured (layers were cached: 13 s).

## Ranked causes of empty cells (run 1, 93 empty cells)

Every empty cell in run 1 is accounted for by exactly one of causes 1 to 3 plus cause 5's timeouts: 50 cells
(causes 1 and 2) + 43 cells whose `cell_metadata.skipped_providers` records a cooldown (cause 3).

### 1. HQ Location (research): 25 cells, needs a key by design, but the demo claims it does not

All 25 cells: `status=error, error=no_answer, provider=NULL`, `cell_traces` has 0 rows. The column is
`type: research` and calls the LLM in `internal/aicol/research.go`; with no LLM key the call fails and the error
is swallowed into `no_answer` (lines 431 to 453), so the cell never says "no LLM key". The workbook description
(`internal/demoseed/demo.go`) says "Every column uses FREE, zero-key providers". Tags: needs a key by design,
plus a misleading seed description and a diagnostics gap (the reason is not recorded).

### 2. Email Verify: 25 cells, engine or configuration bug (probable, one step inferred)

All 25: `error=no_data`, empty `cell_metadata`. The default waterfall for `email_verify` is `mailscout`,
`abstract_api`, `debounce` (`internal/workbook/defaults.go`); the last two need keys. `provider_stats` shows
`mailscout`/`email_verify` attempts 50 (100 after re-run), hits 0, total latency 1.08 ms for the 50 attempts, so
it never reached the network. The code (`internal/providers/scrape/email.go`, `mailScout.Enrich`) returns
`no_email_to_verify` when `lead.email` is empty. After the run `SELECT count(*) FROM leads WHERE email<>''` is
0 even though 24 Email cells were filled: the run writes Email into `workbook_enrichments` but the lead the
next column reads has no email. So Email Verify cannot fill in the same run as Email. Inferred, not directly
observed: the `no_email_to_verify` string is not recorded in the cell. Not measured: whether SMTP port 25 is
reachable from this host, so even with the lead fixed it might return `smtp_unverified`. Tag: engine or
configuration bug.

### 3. One timeout cools a provider down for the whole workspace: 43 cells, engine or configuration bug

Per-provider timeout is 10 s (`defaultTimeout`, `internal/providerrun/run.go:53`); a timeout writes
`cooldown_until = now + 180 s` (`timeoutCooldownSeconds`, line 58, applied in `record`) to `provider_stats` for
that provider and field, and every later cell skips it. Evidence, run 1:

- Empty cells with a recorded cooldown skip: company_size 14 of 14 empty, phone 18 of 18, hiring 8 of 8,
  linkedin 2 of 2, email 1 of 1. Example `cell_metadata`:
  `{"skipped_providers":[{"provider":"deep_scraper","reason":"cooldown"},{"provider":"website_scraper","reason":"cooldown"},{"provider":"company_intel","reason":"cooldown"}]}`.
- Worker log: 80 `provider timed out` warnings in run 1 (deep_scraper 20, website_scraper 16, jobspy 11,
  social_finder 9, email_harvester 9, company_intel 8, facebook_pages 3). First burst at 23:34:33 on
  `deep_scraper` rows 3, 9, 10, 11, 12 within 20 ms of each other.
- `provider_stats` rows: `deep_scraper`/`company_size` cooldown set 23:39:20, `ats_hiring` 23:37:39, `jobspy`
  23:37:36, i.e. one timeout parks a provider that was succeeding (`ats_hiring` hit 12 of 22).
- Re-run: Email 24 to 13, Phone 7 to 1: cells filled in run 1 were emptied in the re-run (see cause 7).

A suspected contributor, not proven: the worker runs up to 12 rows concurrently while `BROWSER_MAX_CONCURRENCY`
is 4, so browser-service requests queue and exceed the 10 s client timeout. The simultaneous first timeouts are
consistent with that. A timeout is queueing delay, not a sign that the provider is down, yet it is treated as one.
Tag: engine or configuration bug. This is the largest recoverable cause (43 of 93 empty cells; the script run
shows the same pattern, 62 of 112).

### 4. 18 of the 24 filled Email cells are a guessed `info@<domain>`: quality, inflates the rate

`ddg_email` falls back to `info@<domain>` with confidence 0.2, `email_confidence=generic`, when search finds
nothing (`email.go`, `ddgEmail.Enrich`, "searched, not verified" branch). Run 1: 18 `info@` values all from
`ddg_email`; only 6 Email cells are real addresses found on a page or in search (`deep_scraper` 3, `website_scraper` 1,
`ddg_email` 2: customercare@figma.com, sforshee@digitalocean.com). Cell metadata is `{"verify":{"confidence":0,"source":"smtp","status":"unknown"}}`.
Excluding guesses, run 1 is 89/200 = 44.5%. Tag: engine or configuration bug (a guess counted as a fill).

### 5. External sites blocking the free search providers

Browser-service log, run 1 (counts of fetches by host and status): `search.brave.com` 105 x HTTP 429,
`www.mojeek.com` 94 x 403 (13 x 200), `html.duckduckgo.com` 81 x 202 (no results page), while
`search.yahoo.com` (111) and `www.startpage.com` (84) answered 200, `en.wikipedia.org` 49, `grokipedia.com` 49.
These degrade `ddg_company`, `ddg_email`, `local_business`, `social_finder`, and feed the timeouts in cause 3.
The script run shows the same shape (brave 24 x 429, mojeek 13 x 403). Tag: external site blocking. Not
measured: which of these blocks directly produced a given empty cell.

### 6. Phone has no free source that works at 80%

`phone` filled 7/25 (28%) in run 1 and 2/25 in the script run. Of 18 empty cells all had cooldown skips (cause
3), so the free ceiling is not known; the providers that did run hit 10/23 (`local_business`), 2/20
(`ddg_company`), 0/19 (`facebook_pages`). Tag: free provider failing (partly caused by cause 3, partly by
cause 5).

### 7. A re-run can empty cells that were filled

Run 1 to re-run: 26 changed, 18 cells lost their value (find_email 11, find_phone 6, linkedin 1), 1 gained
(hiring), 7 values changed. All 18 lost cells carry `skipped_providers: cooldown` and `error` of `timeout` or
`no_data`. The re-run overwrote the stored value with an error result instead of keeping it. Tag: engine or
configuration bug (data loss), outside the fill-rate question but found in the same measurement.

### 8. Missing input in the seed data

None found. All 25 rows have Company and Website (25/25 each), so no cell stayed empty for a missing input.

## Collisions when running a second stack of this repo on one host (product bug list)

Found while bringing the project up next to another stack from the same compose file:

1. `lead_net` is pinned to `172.29.250.0/24` (`docker-compose.yml`, `LEAD_NET_SUBNET` default). With another
   project using that subnet, `docker network create --subnet 172.29.250.0/24` fails with
   `Pool overlaps with other one on this address space` (reproduced). `LEAD_NET_SUBNET` exists and fixes it,
   but the default collides, and `TRUST_PROXY_CIDRS` defaults to the same value.
2. Fixed `container_name` on `postgres` (`lead-data-postgres`), `redis` (`lead-data-redis`), `nginx`
   (`lead-data-web`) and `reacher` (`lead-data-reacher`). A second project cannot start while those exist
   (the names were present on this host). Only a compose override that resets them works.
3. Fixed `image:` tags (`opengtm-api-go:local`, `opengtm-worker-go:local`, `opengtm-scheduler-go:local`,
   `opengtm-migrate-go:local`, `opengtm-browser:local`). A build from a second checkout retags images under
   a running stack of the first. Not triggered here (the override renames them); reasoned from the file.
4. Default host ports 3000 (nginx, bound on all interfaces), 5432, 6379, 8001 are configurable via env, but the
   nginx port is published on `0.0.0.0` unlike the others.
5. The `!reset` tag used in the override needs Docker Compose 2.24 or newer.

## What was not measured

- A cold image build time (layers were cached).
- Whether SMTP port 25 is reachable, so what Email Verify would give if the lead carried the email.
- The reason `mailscout` failed per cell (inferred from code and latency, not recorded by the product).
- Any run with an LLM key or paid keys, or with a proxy (`PROXY_LIST`).
- Any variance estimate beyond two installs; any run of the old Python version (31% is taken from July as given).
- Fill rate with the cooldown disabled or the browser concurrency raised: the 75% ceiling below is arithmetic
  on run 1 (150 of 200 cells not in causes 1 and 2), not a measurement.
- Correctness of filled values beyond the email guess check in cause 4 (no accuracy audit).

## Recommended order of fixes (cells gained per unit of effort)

1. Do not cool a provider down for a timeout, or scope the cooldown to the row, or only after N consecutive
   timeouts; keep the 180 s cooldown for real 429s. Small change in `providerrun.record`. Up to 43 cells in run 1
   (21 points) and the main source of run-to-run variance.
2. Raise `BROWSER_MAX_CONCURRENCY` to match the worker's row concurrency, or lower the row concurrency; or
   count queue wait outside the 10 s provider timeout. Small, config-level, targets the likely cause of the
   timeouts. Verify by re-measuring.
3. Make a re-run keep an existing value when the new result is an error. Small; does not raise a first-run
   number but removes the data loss in cause 7.
4. Make Email Verify read the email of the same run (feed the Email cell into the lead before the verify
   column, or order the columns). Medium; up to 25 cells (12.5 points) if SMTP is reachable, otherwise
   `smtp_unverified` results. Needs the port 25 check first.
5. Fix the demo: either drop or relabel the HQ Location research column (and the "zero-key" claim), or give it a
   free path. Trivial to relabel; filling it needs an LLM key. Relabel now.
6. Stop counting `info@<domain>` guesses as filled, or mark them low confidence in the cell, so the rate is
   honest. Small; lowers the headline by 9 points in run 1 but stops overstating it.
7. Record the real reason in the cell (no LLM key, no email to verify, which providers were tried and what each
   returned): a tried-and-missed provider is not currently recorded, only skipped ones.

With fixes 1, 2 and 4 together the ceiling on this workbook without an LLM key is 175 of 200 (87.5%), and with
fix 1 alone it is 150 of 200 (75%); both are upper bounds from this data, not measurements. Reaching 80% needs
Email Verify to work or the research column to have a key.
