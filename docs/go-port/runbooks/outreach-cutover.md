# Runbook: outreach cutover to Go (plan 09)

Go serves the 22 `/api/outreach` routes next to FastAPI (grounded drafts, sequences and their actions, SMTP config, suppressions, public unsubscribe, bounce webhook) and claims two job types behind flags:
`send` (SMTP) and `outreach_inbound_poll` (IMAP bounce/complaint intake). **`POST /api/campaigns/generate` is retained on Python** (D1: it calls the LLM per lead; `apps/backend-go/testdata/contract/retained_outreach_routes.txt`).
Deviations: `apps/backend-go/testdata/contract/README.md`, section "Plan 09 (outreach ...)".

## Prerequisites

1. Alembic revision `a7c1e9d3b5f2` (Task 2, `outreach_send_by_message_id`) is applied and the Go schema check passes (`RequiredGoSchemaRevision`). The migration role must bypass RLS (`rolsuper` or `rolbypassrls`): the function is `SECURITY DEFINER` and `outreach_sends` has FORCE RLS. `yupcha_app` only gets `EXECUTE`.
2. `PG_LEAD_STORE=true` on `api-go`. Routes register only when `GO_OUTREACH_ROUTES_ENABLED` **and** `PG_LEAD_STORE` are on; otherwise Go registers nothing, logs `outreach routes not registered`, and a request that reaches it gets 404. nginx cannot see the flags:
   never raise the canary above 0% until the startup log has no such line.
3. `SECRETS_MASTER_KEY` is **identical on both stacks** (and on the Go worker). It encrypts the SMTP/IMAP passwords and derives the unsubscribe-token key; a mismatch makes every link already mailed fail on one stack. Without a usable key Go answers 503 on the public unsubscribe routes and the worker hands `send`/`outreach_inbound_poll` jobs back to Python (error log at startup).
   `SECRET_KEY` and `APP_ENV` must also match (an insecure default `SECRET_KEY` in production is refused).
4. `OUTREACH_PUBLIC_BASE_URL` (no trailing slash) is the URL recipients reach, used in `List-Unsubscribe` and the footer link; same value on both stacks. `OUTREACH_UNSUB_TTL_DAYS` (90) and `OUTREACH_BOUNCE_WEBHOOK_SECRET` (shared with the bounce provider; unset = the webhook answers 503) are the same on both stacks.
5. `AUTOMATIONS_ENABLED` is the master switch for sends: with it off, Go `send` Handle returns without effect (like Python `handle_send`) and the job completes without mailing. The inbound poll also needs `OUTREACH_INBOUND_POLL_ENABLED=true`. Check the value on Python and Go before moving jobs.
6. Executor cohort: Python decides the executor at enqueue. A job goes to Go only when `GO_EXECUTOR_ENABLED=true`, the workspace is in `GO_EXECUTOR_WORKSPACES`, **and** the per-type flag is on (`GO_EXECUTOR_OUTREACH_SEND` / `GO_EXECUTOR_OUTREACH_INBOUND`). Set the same values on both stacks. Sends are non-idempotent, so the cohort (not a global switch) limits the blast radius.
7. **The Python scheduler stays** (D9): `bootstrap_outreach_schedules` and `bootstrap_inbound_schedules` are the only tickers; Go exposes `Tick`/`EnqueueSend` but nothing calls them on a timer. The Python reaper stays too (Go has no reaper; it reconciles dead Go leases by job type). Only the job's `executor` column changes with the flags.
   Python's scheduler also needs the Postgres session time zone to be UTC: `next_run_at` is a naive-UTC column written by both stacks.
8. Paid sends: `OUTREACH_SEND_COST_USD` defaults to 0 (free). If > 0, plan 08 automation caps must be wired (the worker always wires `AutoCaps`); otherwise the job is handed back to Python.
9. `TRUST_PROXY_CIDRS` (public limiter): default `127.0.0.0/8,::1/128,172.16.0.0/12,192.168.0.0/16,fc00::/7` covers loopback and docker bridge networks, and `X-Real-IP` is honoured only from those peers. **The API must not be reachable from a LAN without nginx in front** (LAN peers in these ranges can forge `X-Real-IP`).
   In Kubernetes (pod/service network 10/8) set `TRUST_PROXY_CIDRS` to the ingress or nginx pod CIDR. A warning `proxy headers from untrusted peer` in the API log means nginx connects from outside the list.
10. nginx build contains `# ===== BEGIN outreach (plan 09) =====` in `go-routes.conf`, and `scripts/nginx-routing-check.sh` passes at 0%, 50% and 100%. `$go_bucket` and `$go_ws_bucket` in `go-split.conf` carry the same percentage.
11. Restart `api-go` and the worker after any flag change (config is read at boot).

## Flag order

Flip one at a time, in this order, watching between steps (all default false):

1. `GO_OUTREACH_ROUTES_ENABLED=true` on `api-go` (requires `PG_LEAD_STORE=true`). Routes only; canary stays at 0%. Then raise the nginx canary 5% -> 25% -> 100%.
2. `GO_EXECUTOR_OUTREACH_INBOUND=true` (plus `GO_EXECUTOR_ENABLED` and the cohort) on Python and the Go worker. Inbound is idempotent (ledger-keyed), so it goes before send. Spot-check bounces and complaints (below).
3. `GO_EXECUTOR_OUTREACH_SEND=true` **last**, for one cohort workspace first.

## Routing

- `POST /api/campaigns/generate` is always `api_backend` (retained).
- `/api/outreach/*` routes follow the canary bucket (`X-Upstream` response header shows `py` or `go`). Unknown siblings and trailing-slash variants stay on Python at every percentage.
- `/api/outreach/unsubscribe` (GET landing, POST one-click) and `/api/outreach/webhooks/bounce` are public and sticky to one stack per request source (the bucket key is the client address when no `Authorization` header is sent). They forward `X-Real-IP` and `X-Forwarded-For`, so the public limiter sees the client.
  **Links already mailed keep resolving at every percentage**: tokens are verified with the same derived key on both stacks, so either stack accepts either stack's token (given an identical `SECRETS_MASTER_KEY` and `SECRET_KEY`).
- The limiter (20 requests per 60 s per IP and per token) is per process and per replica.

## SMTP and IMAP: egress and TLS (D2)

- Go dials SMTP/IMAP through the SSRF guard: loopback, private, link-local and metadata addresses are refused at dial time; certificates are verified. Python dials anything. Workspaces pointing at an internal relay break by design.
- **Plaintext AUTH is refused**: `use_tls=false`, a server without STARTTLS, or an IMAP server without STARTTLS fails closed (port 465 is implicit TLS, 587 requires verified STARTTLS). Self-signed relays fail certificate verification; there is no insecure switch. Fix the relay's certificate.
- `EGRESS_ALLOW_HOSTS` (comma-separated host or host:port) is an operator allow-list that lets the guard dial internal names; it is honoured for SMTP and IMAP dials only when `APP_ENV` is `test` or `bench`, and ignored in production. Use it on a bench (`APP_ENV=bench`) to point Go at a private relay, never as a production workaround.
- Transport errors (refused, timeout, DNS, TLS) never hard-bounce or suppress a recipient; they fail the send and the job retries. A 535 (bad credentials) or 552 does not suppress either (parity with Python). Only 550/551/553/554 replies hard-bounce.

## Draining the Go send path

1. Set `GO_EXECUTOR_OUTREACH_SEND=false` on Python (new `send` jobs enqueue as `executor='python'`) and restart it.
2. Wait until `SELECT count(*) FROM jobs WHERE type='send' AND executor='go' AND status IN ('pending','processing')` is 0. A job already claimed by the Go worker finishes (Go's handler commits an `in_flight` row before dialing SMTP, so it is at-most-once).
3. A send row left `in_flight` longer than the job timeout (300 s) is a replay-skip **by design**: the next attempt sees the marker and does not mail again (it could not know whether the first SMTP transaction completed). Inspect such rows by hand; do not reset them to `pending` unless you have confirmed the recipient did not receive the message.
4. Then restart the Go worker (or set `GO_EXECUTOR_OUTREACH_SEND=false` there too: with the flag off it hands every `send` row back to Python untouched). A job cancelled after the `in_flight` marker is finalized `failed`.
5. Inbound: set `GO_EXECUTOR_OUTREACH_INBOUND=false` and wait for `outreach_inbound_poll` jobs with `executor='go'` to leave `pending/processing`; poll ticks are short (job timeout 600 s) and idempotent.

## What to watch

- API log: `outreach routes not registered`, `proxy headers from untrusted peer`, 503s on unsubscribe (master key).
- Worker log: `outreach handlers hand jobs back` (no secret resolver or master key), per-send errors (type only, never an address or password).
- Queue: `send` jobs failing repeatedly (transport/TLS) and sequences with `auto_paused=true` (the bounce/complaint circuit breaker: `OUTREACH_BOUNCE_PAUSE_RATE` 0.05, `OUTREACH_COMPLAINT_PAUSE_RATE` 0.003 after `OUTREACH_CIRCUIT_MIN_SENDS` 20 sends).
- Inbound: `outreach_inbound_schedules` rows with `consecutive_failures` climbing; a schedule disables itself at `OUTREACH_INBOUND_MAX_CONSECUTIVE_FAILURES` (10).
- Webhook: `outreach_send_by_message_id` is a sequential scan of `outreach_sends` per call (no index on `message_id`). If webhook volume or table size grows, add an index on `outreach_sends (message_id)` in a follow-up Alembic revision.

## Bounce and complaint spot checks

1. Webhook: `curl -s -X POST https://<host>/api/outreach/webhooks/bounce -H 'X-Webhook-Secret: <secret>' -H 'Content-Type: application/json' -d '{"message_id":"<a recent Message-ID>","event":"bounce","hard":true}'` answers `{"status":"ok", ...}` and the recipient appears in `GET /api/outreach/suppressions`;
   repeat the call: `duplicate`. A wrong secret is 403, an unset secret 503. Use a throwaway recipient in a canary workspace.
2. Complaint: the same call with `"event":"complaint"` suppresses and **locks** the row (`DELETE /api/outreach/suppressions/{email}` then answers 403 `locked`).
3. Unsubscribe: open a mailed `List-Unsubscribe` link (GET landing shows the escaped address), POST it (`{"status":"unsubscribed"}`); the row is locked. Repeat the check at the current canary percentage and at the other stack (force it with the `X-Upstream` observed per source).
4. Inbound: send a test message to a mailbox that bounces; within one poll interval a ledger row appears in `outreach_inbound_messages` and the recipient is suppressed. IMAP unconfigured disables the schedule.
5. Gmail: suppression keys for `gmail.com`/`googlemail.com` are stored collapsed (no dots, no `+tag`) by Go. Rows written earlier by Python are dotted and keep matching in **one direction only** (the dotted spelling is blocked; the undotted spelling is not). Optional one-time normalisation (maintenance window, after taking a backup):
   re-key `outreach_suppressions.email` with the same collapsed key and drop duplicates, keeping the locked row. Not run by any migration; skipping it is safe (Go suppresses a superset of Python on new writes).

## Rollback

1. `GO_EXECUTOR_OUTREACH_SEND=false` (drain as above), then `GO_EXECUTOR_OUTREACH_INBOUND=false`, on Python and on the Go worker; restart both.
2. Set the nginx percentage back to 0 (comment the `go` lines in `go-split.conf`, `nginx -t`, reload). Unsubscribe and webhook callers return to Python; mailed links still resolve there.
3. `GO_OUTREACH_ROUTES_ENABLED=false` last (routes unregister; Go then answers 404 for them, so only after the canary is 0).
4. No data migration is needed: both stacks use the same tables. Suppressions written by Go are collapsed for Gmail and locked for unsubscribe/complaint; Python reads them fine (its lookup collapses too).
5. The `outreach_send_by_message_id` function and revision `a7c1e9d3b5f2` stay; Python ignores them.

## Deviations to expect (full list in the README)

- Stricter validation: 422 instead of 500 for out-of-range or over-long values; `PUT` cannot activate a sequence (use `start`); `enroll` accepts at most 5000 ids (chunks of 500, partial commit on failure); SMTP host/port validated; `POST /smtp/test` limited to 10/min.
- HTML-escaped unsubscribe landing; secret-before-body on the webhook; a blank suppression email is a 422.
- Unsubscribe and complaint lock existing suppressions; Gmail suppression keys are collapsed on write.
- Wire format differs (quoted-printable bodies, quoted From), no `Date` header (as Python), cohort-based executor routing, rolling 24 h daily cap (as Python).

## Verification commands

```sh
cd apps/backend-go
go vet ./... && go test -race ./internal/api -run 'Outreach' -count=1
TEST_DATABASE_URL=<private clone> go test -race -tags integration ./internal/api ./internal/outreachstore ./internal/outreachsend ./internal/outreachinbound ./cmd/worker
scripts/nginx-routing-check.sh        # from the repo root: 0%, 50% and 100%
```
