# Real-backend first-run journey

`first-run.spec.ts` drives the built app in Chromium against a running stack (nginx -> api-go -> Postgres), as a
new user would. Nothing is mocked, which is the point: the a11y suite in `e2e/a11y` mocks every `/api` call, so it
cannot tell you whether the pages work on real response bodies.

It is not part of `bun run test:a11y` (that config only looks in `e2e/a11y`) and it is not run in CI.

What it does, in order:

1. Opens `/`, follows the redirect to `/setup`, creates the admin with the one-time setup token, and records where
   the product lands.
2. Follows the "Get started" checklist: loads the sample workbook, opens it, reviews and starts the run (25 rows,
   one run; free providers make **real outbound requests**), and polls the workbook state until the run settles.
3. Visits every sidebar and admin route (and the sample workbook) in light and dark. Per route it records the
   status of every `/api` and `/auth` response, console errors and warnings, uncaught page errors, visible error
   states and toasts, and horizontal overflow, and saves `<route>-<theme>.png`.
4. Exercises one safe action per area: create a company, create an audience from a filtered list, open and cancel
   the webhook destination dialog, create a workbook from a template, save a Pipeline setting, command palette
   navigation, and import a generated 5-row CSV. A failing action is recorded and the journey continues.

Output goes to `OPENGTM_E2E_OUT`: screenshots and `findings.json` (`findings` and every API call with its status).
The test fails on any 5xx response or uncaught page error, or when a blocking step (setup, sample load, run) fails.
Everything else is in `findings.json` for you to triage.

## Run it against the rehearsal stack

The journey needs a **fresh** stack (no admin yet), because it does first-run setup. Use the same isolation the
release rehearsals use (`scripts/release/rehearsal.compose.yml`, `docs/operations/release-rehearsals.md`): its own
Compose project, free host ports, its own subnet and secrets, no repo `.env`. `rehearse-install.sh` itself creates
the admin, so for this journey start the stack by hand with the same override:

```sh
E=$(mktemp -d ~/.cache/opengtm-e2e.XXXX)            # not /tmp (RAM-backed)
r() { openssl rand -hex "$1"; }
cat > "$E/env" <<EOF
SECRET_KEY=$(r 24)
INTERNAL_SERVICE_TOKEN=$(r 16)
BROWSER_SERVICE_TOKEN=$(r 16)
POSTGRES_PASSWORD=$(r 12)
YUPCHA_RUNTIME_DB_PASSWORD=$(r 12)
PORT=127.0.0.1:23480
API_GO_PORT=23481
POSTGRES_PORT=23482
REDIS_PORT=23483
LEAD_NET_SUBNET=10.177.43.0/24
APP_ENV=dev
EOF
DC="docker compose -p opengtm-e2e --env-file $E/env -f docker-compose.yml -f scripts/release/rehearsal.compose.yml"
$DC up -d --build        # from the repo root; pick ports and a subnet that are free on your machine
TOKEN=$($DC logs api-go 2>&1 | grep -o '"setup_token":"[^"]*"' | head -1 | cut -d'"' -f4)

cd apps/web && bun install --frozen-lockfile && bunx playwright install chromium   # once
OPENGTM_E2E_BASE_URL=http://127.0.0.1:23480 OPENGTM_E2E_SETUP_TOKEN=$TOKEN \
  OPENGTM_E2E_OUT=$PWD/e2e-real-out bun run test:e2e-real

cd ../.. && $DC down -v --remove-orphans             # tear down, volumes included
```

| Variable | Meaning |
|---|---|
| `OPENGTM_E2E_BASE_URL` | required, the nginx URL |
| `OPENGTM_E2E_SETUP_TOKEN` | required, the token `api-go` logs once while no admin exists |
| `OPENGTM_E2E_OUT` | where screenshots and `findings.json` go (default `./e2e-real-out`, git-ignored) |
| `OPENGTM_E2E_SKIP_RUN=1` | skip starting the enrichment run (no outbound provider traffic), for a quick route sweep |

The sample run takes a few minutes; the whole journey about seven. To repeat it, reset the stack first
(`$DC down -v`, then `up -d`), since the admin and sample workbook persist.

## Keeping the a11y fixtures honest

When this finds a page whose real response body differs from `e2e/a11y/fixtures`, copy the real shape into
`e2e/a11y/fixtures/real-shapes.ts` and register it in `fixtures/routes.ts`. The a11y suite fails any page that
shows "temporarily unavailable", "returned an invalid response" or "could not be verified", because that means a
fixture is missing (unmatched endpoints answer `[]`).
