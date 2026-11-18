# Security closure evidence for issue 114

This matrix tracks [issue 114](https://github.com/debpalash/OpenGTM/issues/114)
against source candidate `e64504ba33e66cb2c4d7d558633367409db443a0`.
It records controls and specific regression evidence, not a security certification
or an affected/fixed release statement. A passing test covers its asserted
boundary; it does not establish all callers or deployed configurations are safe.

## Original report

Paths below are relative to the repository root. “Passed” refers only to the
focused commands recorded below. “Mapped” means source/test evidence was
located but has not been executed as part of this review.

| Finding | Current runtime control and regression evidence | Remaining verification |
| --- | --- | --- |
| Stripe webhook accepts an empty secret | `apps/backend-go/internal/billing/stripe_test.go`, `TestSignatureEmptySecretRejected`, passed: a payload signed with an empty key is rejected. | Exercise the HTTP webhook with missing operator configuration and verify no workspace credit is committed. Determine affected/fixed released versions. |
| Declarative templates can read undeclared process environment | `internal/manifest/execute_test.go` under `apps/backend-go`: `TestExecuteEnvAllowlist` and `TestExecuteEnvDenyAndUndeclared`, passed. | Verify tenant credential resolution and every execution entry point use the same restricted resolver. |
| Outbound clients inherit ambient proxies/netrc | `apps/backend-go/internal/egress/transport.go` sets `Proxy: nil`; browser `engines.py` and `scrapers/staffspy_roster.py` use `trust_env=False`. Go egress address/rebinding tests passed. | Audit operator-configured clients and retained-browser engine routing; execute browser egress tests in its supported environment. |
| Declarative result loses provider attribution | Manifest execution success regression passed, but its mapped `src` response field is not proof of outer provider attribution. Traced manifest result -> `providerrun.runManifest` -> `rpclocal.Caller` (outer result named from the call) -> workbook waterfall cell -> `workbook_enrichments` -> row cell. `internal/rpclocal/manifest_attribution_test.go` `TestDeclarativeProviderOuterResultCarriesProviderName` (success and vendor failure, vendor-claimed `provider`/`src` ignored) and `internal/workbook/manifestprovider_attribution_integration_test.go` `TestDeclarativeProviderAttributionSurvivesWaterfallEvidenceAndRowPresentation` (persisted evidence and row cell), both passed. | API-level workbook rendering beyond the stored row cell was not exercised. |
| Eleven Python providers import missing settings helper | Native runtime no longer imports those Python providers. Settings masking/encryption regression subset passed. Saved-key path verified in `internal/api/settings_savedkey_workspace_integration_test.go`: `TestSavedGlobalProviderKeyReachesEveryWorkspaceNextRun` (PUT enrichment-providers is the operator key in `app_settings`; the next `providerrun` run in every workspace uses the updated key) and `TestSavedWorkspaceKeyIsolatedAndUpdatedPerWorkspace` (PUT workspace-integrations; per-workspace update, no leak to another workspace, blank save keeps the secret, ciphertext at rest), both passed with no process-env write-through. | Per-provider mapping of the eleven providers to resolver keys was not done. No settings API route saves a workspace-level key for an enrichment provider (only integration fields); the workspace-scoped test uses a `HUBSPOT_TOKEN`-keyed probe provider. |
| Unauthenticated files/stats and unsafe deletion | `apps/backend-go/internal/api/systemroutes_test.go`: `TestNoUnauthenticatedRouteInAnyMode` and `TestHardenedHTTPTraversalNeverTouchesOutside`, passed. | Exercise authorization roles, symlinks, deletion behavior and data-root configuration in supported deployment. |
| Empty stored password accepted; long bcrypt password returns 500 | `apps/backend-go/internal/auth/auth_test.go`: `TestVerifyPasswordEmptyIsFalse` and `TestVerifyPasswordLongAndNonASCII`, passed. | Exercise login HTTP responses and ensure failures do not disclose credentials. |

## Follow-up comments

| Report | Current evidence | Remaining verification |
| --- | --- | --- |
| [MCP enrichment tuple and unconfigured waterfall](https://github.com/debpalash/OpenGTM/issues/114#issuecomment-6415506687) | Native MCP has `internal/mcp/tools_enrich_test.go`, including `TestEnrichCompanyResultShape` and `TestEnrichCompanyMissesCrossTenantLeadBeforeSpending` (mapped). Production wiring traced: `cmd/mcp openRuntime` -> `mcp.NewProductionDeps` -> `PyEnrichPort` -> `rpclocal` -> `website_scraper`. `cmd/mcp/enrich_wiring_integration_test.go` `TestEnrichCompanyRunsConfiguredChainThroughProductionRuntime` passed (cross-tenant lead denied before any fetch or cap slot; configured chain returns website_scraper fields; one settled reservation and audit row; `MCP_MAX_ENRICH_PER_DAY` cap refuses before any fetch, per workspace). | Defect found and fixed in this change: a provider miss aborted the whole chain because `PyEnrichPort.Py` returned an error for `Status != "ok"`. A miss (provider ran, `success=false`) now returns `mcp.ErrProviderMiss`, and the waterfall moves on to the next provider; infrastructure errors still abort. `TestEnrichCompanyProviderMissIsNotAToolFailureThroughProductionRuntime` passed. The browser service is a local fake; the `ddg_email` fallback was not run live. |
| [Poller nonzero costs and webhook SSRF](https://github.com/debpalash/OpenGTM/issues/114#issuecomment-6416909533) | `internal/poller/poller_integration_test.go` and `engine_integration_test.go` exercise nonzero source costs (mapped). Egress lexical guard and dial-time rebinding tests passed. | Run PostgreSQL poller accounting tests, concurrent debit/retry tests, and the automation webhook boundary using the reported blocked host classes. |
| [Scraper rebinding and unbounded download](https://github.com/debpalash/OpenGTM/issues/114#issuecomment-6422322748) | Browser `tests/test_egress.py` has `test_connect_tunnel_pins_the_single_resolution`, `test_resolve_blocks_when_any_address_private`, `test_session_byte_cap_cuts_the_download` and `test_proxy_requires_session_credentials` (mapped). `src/browser_service/engines.py` streams downloads and bounds response data. | Execute retained-browser tests, verify every supported engine uses the session proxy, and verify byte limits and browser isolation in the shipping container. |
| [Destination status length and API errors](https://github.com/debpalash/OpenGTM/issues/114#issuecomment-6422363715) | `internal/db/schema.sql` uses `destination_runs.status varchar(32)`. Destination store/API integration tests persist/retry `completed_with_errors`. `destscrud_integration_test.go` maps duplicate rename to 409 and null fields to 422; audience null validation appears in `audiencescrud_integration_test.go`. `TestSkippedDeliveriesFilterPaginationAndTenant` covers the repaired skipped filter (mapped in this review). Invalid-IPv6 account serialization (`GET /api/audiences/{id}/accounts`, `audiencevalid.AccountKey`): `TestAccountKeyInvalidIPv6FallsBackToCompany` and `TestAccountsInvalidIPv6WebsiteNoCrash` (HTTP 200, company-keyed) passed against a freshly migrated PostgreSQL. | Execute the destination SQL/API tests on the candidate schema. |

## Focused execution evidence

Run from `apps/backend-go` on the candidate above:

```sh
go test ./internal/auth ./internal/billing ./internal/manifest ./internal/egress ./internal/settings ./internal/api \
  -run 'Test(SignatureEmptySecretRejected|ExecuteEnvAllowlist|ExecuteEnvDenyAndUndeclared|ExecuteSuccess|VerifyPasswordLongAndNonASCII|VerifyPasswordEmptyIsFalse|NoUnauthenticatedRouteInAnyMode|HardenedHTTPTraversalNeverTouchesOutside|ProvidersMaskShortAndLongKeys|SetEncryptsSecretKeysOnly|CheckURLVectors|CheckURLGoOnlyVectors|DialTimeRebindBlocked|DialRejectsAnyBlockedIP)$' -count=1
```

On 2026-11-15, auth, billing, egress, settings and API selections passed on
macOS with Go 1.27.2. The initial restricted environment could not bind the
manifest test's local HTTP listener. Running the manifest selection separately
with local listeners permitted passed (0.246 s):

```sh
go test ./internal/manifest \
  -run 'Test(ExecuteEnvAllowlist|ExecuteEnvDenyAndUndeclared|ExecuteSuccess)$' -count=1
```

These are deterministic local regressions, not live-provider or staging tests.
No PostgreSQL integration or retained-browser suite result is claimed here.

## Gates before closure

- Complete every remaining verification cell, including provider attribution,
  credential updates, MCP production wiring and invalid-IPv6 account handling.
- Record shipping candidate hashes, deployment configuration and boundary-test
  results; include the retained browser service.
- ~~Decide the actual supported release lines and legacy Python retirement or
  backport policy.~~ Decided 2026-11-19: `SECURITY.md` lists the supported lines.
  4.x (Go) is supported, and 3.x (Python) gets security-only fixes for 90 days
  after v4.0.0 is published.
- Complete the upgrade, backup restore and release gates in
  [issue 120](https://github.com/debpalash/OpenGTM/issues/120), then publish
  affected/fixed-version guidance and any coordinated notices.

Keep issue 114 open until those results and dispositions are linked. Any new
exploitable finding belongs in the private reporting process in `SECURITY.md`.
