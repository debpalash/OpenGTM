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
| Declarative result loses provider attribution | Manifest execution success regression passed, but its mapped `src` response field is not proof of outer provider attribution. | Trace execution through provider caller, waterfall result, persisted evidence and workbook presentation; add an assertion at that boundary if absent. Remains open. |
| Eleven Python providers import missing settings helper | Native runtime no longer imports those Python providers. Settings masking/encryption regression subset passed. | Map each affected provider to its native credential resolver; verify saved-key updates and workspace isolation without process-env write-through. Removal of Python imports alone is insufficient evidence. |
| Unauthenticated files/stats and unsafe deletion | `apps/backend-go/internal/api/systemroutes_test.go`: `TestNoUnauthenticatedRouteInAnyMode` and `TestHardenedHTTPTraversalNeverTouchesOutside`, passed. | Exercise authorization roles, symlinks, deletion behavior and data-root configuration in supported deployment. |
| Empty stored password accepted; long bcrypt password returns 500 | `apps/backend-go/internal/auth/auth_test.go`: `TestVerifyPasswordEmptyIsFalse` and `TestVerifyPasswordLongAndNonASCII`, passed. | Exercise login HTTP responses and ensure failures do not disclose credentials. |

## Follow-up comments

| Report | Current evidence | Remaining verification |
| --- | --- | --- |
| [MCP enrichment tuple and unconfigured waterfall](https://github.com/debpalash/OpenGTM/issues/114#issuecomment-6415506687) | Native MCP has `internal/mcp/tools_enrich_test.go`, including `TestEnrichCompanyResultShape` and `TestEnrichCompanyMissesCrossTenantLeadBeforeSpending` (mapped). | Trace production enrichment wiring and run configured-provider end-to-end tests, including tenant denial and accounting. A mocked result-shape test alone cannot prove the configured chain runs. |
| [Poller nonzero costs and webhook SSRF](https://github.com/debpalash/OpenGTM/issues/114#issuecomment-6416909533) | `internal/poller/poller_integration_test.go` and `engine_integration_test.go` exercise nonzero source costs (mapped). Egress lexical guard and dial-time rebinding tests passed. | Run PostgreSQL poller accounting tests, concurrent debit/retry tests, and the automation webhook boundary using the reported blocked host classes. |
| [Scraper rebinding and unbounded download](https://github.com/debpalash/OpenGTM/issues/114#issuecomment-6422322748) | Browser `tests/test_egress.py` has `test_connect_tunnel_pins_the_single_resolution`, `test_resolve_blocks_when_any_address_private`, `test_session_byte_cap_cuts_the_download` and `test_proxy_requires_session_credentials` (mapped). `src/browser_service/engines.py` streams downloads and bounds response data. | Execute retained-browser tests, verify every supported engine uses the session proxy, and verify byte limits and browser isolation in the shipping container. |
| [Destination status length and API errors](https://github.com/debpalash/OpenGTM/issues/114#issuecomment-6422363715) | `internal/db/schema.sql` uses `destination_runs.status varchar(32)`. Destination store/API integration tests persist/retry `completed_with_errors`. `destscrud_integration_test.go` maps duplicate rename to 409 and null fields to 422; audience null validation appears in `audiencescrud_integration_test.go`. `TestSkippedDeliveriesFilterPaginationAndTenant` covers the repaired skipped filter (mapped in this review). | Execute SQL/API tests on the candidate schema. Map invalid-IPv6 account serialization to a concrete regression; do not assume that item is closed from the other fixes. |

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
- Decide the actual supported release lines and legacy Python retirement or
  backport policy. `SECURITY.md` still describes a pre-1.0 project; this matrix
  does not choose a policy or infer support from merged source.
- Complete the upgrade, backup restore and release gates in
  [issue 120](https://github.com/debpalash/OpenGTM/issues/120), then publish
  affected/fixed-version guidance and any coordinated notices.

Keep issue 114 open until those results and dispositions are linked. Any new
exploitable finding belongs in the private reporting process in `SECURITY.md`.
