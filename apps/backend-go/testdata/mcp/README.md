# MCP contract fixtures (plan 13)

Recorded from the Python reference (`legacy/python/apps/mcp/server.py`, `apps/api/services/mcp/*`), never hand-written.
Generators: `legacy/python/scripts/{mcp_seed.py,mcp_record.py,gen_mcp_vectors.py}`. Re-recording is byte-identical.

## Layout
- `index.json`: per profile the feature flags and the fixture list IN RECORDING ORDER (writes accumulate state, so replay
  each profile in order against a freshly seeded database), plus the token table (alias, id, sha256, user, workspace,
  capabilities, expires_at, revoked_at). Fixtures carry the token ALIAS (`read`, `write`, `admin`, `adminrole`, `all`,
  `viewer`, `expired`, `revoked`, `removed_member`, `workspace_b`, `nocaps`); the test plaintext is `ycp_` + `seed_` + alias
  (test-only, listed in `mcp_seed.py`, never stored in a fixture). `Authorization` request values are `"<redacted>"`.
- `rpc/` and `tools/`: `handle_message(request, ctx)` with the ctx resolved from the token. `request` is the JSON-RPC
  message, `response` the returned object (`null` for notifications).
- `auth/` and `sse/`: HTTP through `build_sse_app()`: `{request: {method, path, headers, body}, response: {status, headers, body}}`.
- `stdio/`: a real `python -m apps.mcp.server` subprocess: `{request: {stdin}, response: {stdout, exit_code}}` with the raw
  `Content-Length` frames (exit 1 and empty stdout when the env token is missing/invalid).
- Every fixture also has `audit` (new `mcp_audit_log` rows: tool, status, error, idempotency_key, redacted arguments),
  `effects` (row diffs of leads, workbooks, triggers, trigger_cap_reservations, outreach_enrollments, outreach_suppressions,
  jobs; timestamp columns omitted), `stubs`/`stub_calls` (enrichment providers are stubbed; the stub and what the tool
  passed to it are recorded; no vendor is contacted).

## Profiles (flags in `index.json`)
`read` (writes off), `writes` (`MCP_WRITE_ENABLED`, `AUTOMATIONS_ENABLED`, `AUTOMATIONS_ALLOW_LEGACY_OUTREACH`), `capped`
(`MCP_MAX_WRITES_PER_DAY=2`), `outreach_off` (writes on, legacy outreach off), `automations_off` (writes on, automations off),
`rules_cap` (`AUTOMATIONS_MAX_RULES_PER_WS=1`).

## Comparing
- Generated ids are deterministic in the recording (`uuid4` is counter-patched): compare any value matching
  `00000000-0000-4000-8000-[0-9a-f]{12}` or `mcpw_[0-9a-f]{32}` by shape only.
- `volatile` lists JSON paths inside the tool text payload that were clock-derived and are stored as `"<volatile>"`.
- Seeded lead ids are 1-6 (alpha) and 7-8 (beta); new leads continue from 9 in recording order.
- `deviation` marks a case where Go deliberately differs (D3 argument errors, D5 reservation semantics, D6 audit redaction
  of nested secrets); assert the documented Go behaviour instead of the recorded Python text.
- Not recordable from Python (it raises or hangs, see D4): malformed JSON, non-object bodies, oversized frames, concurrent
  same-key calls; these are Go-only tests.

## Vectors (`internal/mcp/testdata/`)
`redact_arguments`, `plan_columns` (`_plan_workbook_columns`), `json_dumps` (three `json.dumps` formats over a corpus; typed
Python values as `{"$py": ..., "v": ...}`), `hash_token`, `lead_write_fields`, `upsert_lead` (real PG store: new, same
company+city overwrites every field, differing case/whitespace is new), `roles` (`require_role` truth table).

## Record procedure
Private DB `opengtm_contract_<x>` on 127.0.0.1:55480, a NOSUPERUSER NOBYPASSRLS login role that is a member of `yupcha_app`,
`TZ=UTC`, `RUN_INLINE_WORKER=0`, `PG_LEAD_STORE=true`, `MCP_REQUIRE_AUTH=true`, `CONTRACT_SEED_CONFIRM=yes`,
`DATABASE_URL` (app role) and `MCP_SEED_DATABASE_URL` (superuser, same DB; used to re-seed before every profile):
`uv run alembic upgrade head && uv run python scripts/mcp_seed.py && uv run python scripts/mcp_record.py && uv run python scripts/gen_mcp_vectors.py`
(run from `legacy/python`; the scripts refuse non-loopback hosts, other database names and a superuser recorder role).
