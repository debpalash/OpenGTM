# REST contract fixtures

Golden request/response pairs **recorded from the FastAPI reference** (never hand-written),
compared by `internal/contract` (`contract.Load`, `contract.AssertEqual`).
Layout: `testdata/contract/<area>/<case>.json`, one directory per case file
(`auth/` from `legacy/python/scripts/contract_cases/auth.yaml`).

Fixture: `{"request": {method, path, headers, body}, "response": {status, headers, body}}`.
`Authorization` / `X-Workspace-Id` request values and issued `access_token`/`refresh_token`
values are stored as `"<redacted>"`; response headers are limited to
`content-type`, `www-authenticate`, `allow`. Compare generated values with
`IgnorePaths` / `IgnoreShapeOf` (e.g. `$.last_login`, `$.access_token`).

## Record procedure

Needs the Postgres test container on 127.0.0.1:55480 and a throwaway database. The seed script
TRUNCATEs users and identity tables (CASCADE) and refuses unless the host is loopback or a unix
socket, the database name matches `^opengtm_contract(_[a-z0-9]+)?$`, and `CONTRACT_SEED_CONFIRM=yes`.

```sh
docker exec opengtm-go-test-pg psql -U postgres -c "CREATE DATABASE opengtm_contract"   # once
cd legacy/python
export DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55480/opengtm_contract \
       SQLITE_STORES_BACKEND=postgres SECRET_KEY=<test-only-value> APP_ENV=test \
       PG_RLS_REQUIRE_SAFE_ROLE=false   # test DB role is a superuser; FastAPI refuses it otherwise
uv run alembic upgrade head
CONTRACT_SEED_CONFIRM=yes uv run python scripts/contract_seed.py   # fixed users alice/bob/carol(inactive), ws-alpha/ws-beta
uv run uvicorn apps.api.main:app --port 18000 &
python3 scripts/contract_record.py --base http://127.0.0.1:18000 \
    --out ../../apps/backend-go/testdata/contract --cases scripts/contract_cases/auth.yaml
kill %1
```

Case files are YAML (PyYAML) or JSON (same structure, `.json` extension). Re-seed before every
recording so ids and `created_at` are stable; `last_login` and tokens are not.
Seed credentials are in `legacy/python/scripts/contract_seed.py` (test-only values).
Review `git diff` of the fixtures after re-recording: a changed fixture is a changed contract.

## Deliberate deviations from FastAPI

- `POST /api/workspaces` with a name whose slug already exists: FastAPI answers 500 (unhandled
  `IntegrityError`); Go answers `409 {"detail":"Workspace slug already exists"}` (user decision,
  Task 11). No fixture exists for this case; it is covered by `TestCreateSlugCollision409`.
- The recorder (`contract_record.py`) can reference earlier responses in a case path or JSON body with
  `{{body:<case>.<dotted.path>}}` (used by `workspaces.yaml` for generated workspace ids).
- `PUT`/`PATCH /scim/v2/{slug}/Groups/{id}` renaming a group onto an existing displayName: FastAPI
  answers 500 (unhandled `IntegrityError`); Go answers `409 {"detail":"SCIM group already exists"}`
  like `POST`. Covered by `TestDuplicateDisplayName409`, no fixture.
- `POST /Groups` with a non-numeric member `value` answers `409 SCIM group already exists` in both
  (FastAPI's blanket `except Exception` swallows the 400); PUT/PATCH answer 400. Fixture
  `scim_groups/create_nonnumeric_member`.
- `GET /api/settings/llm-usage`: FastAPI reads `db.get_llm_usage()` from the lead store, and the shared
  PostgreSQL lead store does not persist LLM usage (it answers an empty list). Go reads
  `llm_usage_daily` (tenant table, FORCE RLS) instead, so a populated table yields real rows where FastAPI
  returns none: a deliberate improvement. Fixtures `llm_usage_empty` / `llm_usage_with_date` record the
  empty FastAPI answer; populated behaviour is covered by `TestLLMUsageTenantIsolation`.
- `PUT /api/settings/enrichment-perf` returns the stored values without the `defaults` key (only GET
  includes it); Go matches the recorded fixture `perf_put_clamp`, not the plan text.
- `GET /api/settings/system`: an environment variable that is set but empty counts as unset (FastAPI keeps
  the empty string; a non-integer `API_PORT` is a 500 in both).
- `settings.yaml` fixtures need `ENRICH_*` and enrichment API keys absent from `app_settings` and no
  `DEBUG`/`API_*`/`CORS_ORIGINS`/`GOOGLE_API_KEY`/`LINKEDIN_LI_AT_COOKIE` in the FastAPI environment.
