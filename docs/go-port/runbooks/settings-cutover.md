# Runbook: settings, secrets and billing cutover to Go (plan 03)

Go serves `/api/settings/*` (except `sources*`) and `/api/billing/*` next to FastAPI, using the same
percentage split as plan 02 (`$auth_upstream`, `infra/nginx/go-split.conf`). Ramp and rollback are config
edits plus `nginx -s reload`. Global secret keys (`app_settings`) are stored as envelope ciphertext, so
the order below is not optional.

## Routing summary

| Path | Upstream |
| --- | --- |
| `^~ /api/settings/providers`, `^~ /api/settings/enrichment-perf`, `= /api/settings/system`, `^~ /api/settings/models/`, `^~ /api/settings/enrichment-providers`, `= /api/settings/llm-usage`, `^~ /api/settings/integrations`, `^~ /api/settings/workspace-integrations`, `^~ /api/billing/` | Go for the canary percentage, else Python |
| `/api/settings/sources*` (5 routes: list, health, PUT, health reset, registry) | always Python (reads the Python leadgen registry; stays until plan 13) |
| `/admin/*`, `/scim/*` | still **not exposed** by nginx |

Each location carries the same proxy directives as `location /api/` (HTTP/1.1, WebSocket headers, 300 s
timeouts, buffering off). `scripts/nginx-routing-check.sh` asserts this plus 0% -> py, 100% -> go and
`sources*` -> py at both ends. Responses carry `X-Upstream: go|py`. The Go binary answers 404 for the
`sources*` routes (`TestSourcesRoutesNotRegistered`), so a mis-routed request is visible.

## Prerequisites (do not skip)

1. Plan 01 cutover is complete (`SQLITE_STORES_BACKEND=postgres`): Python reads `app_settings` and
   `workspace_settings` from PostgreSQL. See `sqlite-to-postgres.md`.
2. Plan 02 is deployed: the Go API (`api-go`) runs behind nginx with `go-split.conf` / `go-routes.conf`
   (`api-cutover.md`), at least at 0%.
3. `SECRETS_MASTER_KEY` (or `SECRET_KEY` when unset) is identical on `api`, the Python worker and `api-go`.
   Do not change it from step 1 until the canary is finished (see "Master key freeze").
4. Stripe SDK: Go uses `github.com/stripe/stripe-go/v87` **v87.0.0** (exact pin in `go.mod`, imported only
   from `internal/billing`). No action on deploy; recorded for audit.

## Ordering (encryption of global keys)

Go writes provider keys, enrichment keys and secret integration fields into `app_settings` as
`enc:v1:...` / `enc:v2:vault:...`. A Python process that still reads that column raw would send
`enc:v1:...` to the LLM vendor as the API key (and into vendor logs). So:

1. **Deploy the Python Task 3 reader everywhere** (`api`, worker, every replica). It reads plaintext and
   ciphertext. Confirm no old Python process remains.
2. Run the one-time encryption script from the Python image, dry run first, then for real:
   ```sh
   python -m apps.api.scripts.encrypt_app_settings --dry-run   # counts only, writes nothing
   python -m apps.api.scripts.encrypt_app_settings
   ```
   It is idempotent, never logs values and never re-encrypts an already-prefixed value. Confirm the
   counts: the real run's "encrypted" equals the dry run's "would encrypt", and a second dry run reports 0.
3. Only now deploy Go (`api-go`) with `CREDENTIALS_SOURCE=http` (the worker keeps fetching credentials
   through the API).
4. Canary the settings routes: raise the split percentage (`1%` -> `10%` -> `50%` -> `100%`), reloading
   nginx each time. Watch `X-Upstream`, 4xx/5xx rates and `audit_log`. Verify in the UI that provider keys
   still show as configured and masked, and that a key saved via Go works through Python (`POST
   /api/settings/providers/{id}/test`).
5. Flip `CREDENTIALS_SOURCE=local` on **one** worker, check a run, then roll it to the rest.

## Stripe

- Set `STRIPE_WEBHOOK_SECRET` (and `STRIPE_SECRET_KEY`, `BILLING_ENABLED=true`) on `api-go` **before**
  routing webhooks to Go. With an empty webhook secret or no secret key, Go **fails closed** and answers
  `503 Stripe is not configured` (it never accepts unsigned events); with `BILLING_ENABLED` unset it answers
  `200 ignored` so Stripe does not retry.
- During the canary add a **second** webhook endpoint in the Stripe dashboard pointing at the Go route
  (`POST /api/billing/webhook`), then disable the old endpoint once traffic is confirmed. Do not leave both
  enabled while `/api/billing/` is split between Python and Go. Idempotency
  (`idempotency_key = "stripe:<session_id>"`, unique index) makes duplicates safe, but two endpoints double
  Stripe's retries.
- Events from an account pinned to another API version are accepted (`IgnoreAPIVersionMismatch`), as in
  Python. The signature is verified over the raw body with the default 300 s tolerance.
- Python `check_and_debit` stays in Python until plan 05 moves enrichment; both stores share
  `workspace_credits` / `credit_ledger_entries`.

## Master key freeze

From step 1 until the canary reaches 100% and soaks, do **not** change `SECRETS_MASTER_KEY` or
`SECRET_KEY`, and do not run `rotate_encrypted_secrets`. Mixed readers on different keys would make
stored secrets undecryptable in one of the two APIs. Rotate afterwards with the Python CLI, which also
re-encrypts the `app_settings` rows.

## Rollback

1. Set the split percentage to `0` and `nginx -s reload`. Python serves everything again.
2. Set `CREDENTIALS_SOURCE=http` on the workers.
3. No data rollback: secrets written by Go decrypt in Python (`decrypt_value`), and Python still reads any
   remaining plaintext.
4. **Never roll Python back past the Task 3 reader** once any global key is encrypted: an older build would
   read ciphertext as the API key. If an old build must run, decrypt the rows first (restore plaintext from
   your secret store) and re-run the script later.
5. Stripe: re-enable the old webhook endpoint and disable the Go one.

## Verification

- `scripts/nginx-routing-check.sh` (0% and 100%).
- `cd apps/backend-go && make vet test` and `make test-integration` (settings/billing/integrations fixtures
  replayed in `internal/api/settings_contract_integration_test.go`).
- Fixtures under `apps/backend-go/testdata/contract/{settings,integrations,billing}` are recorded from
  FastAPI (`legacy/python/scripts/contract_cases/`); see `testdata/contract/README.md`.
