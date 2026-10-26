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

Needs the Postgres test container on 127.0.0.1:55480 and a database whose name contains
`contract` (the seed script refuses anything else; it TRUNCATES users and identity tables).

```sh
docker exec opengtm-go-test-pg psql -U postgres -c "CREATE DATABASE opengtm_contract"   # once
cd legacy/python
export DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55480/opengtm_contract \
       SQLITE_STORES_BACKEND=postgres SECRET_KEY=<test-only-value> APP_ENV=test \
       PG_RLS_REQUIRE_SAFE_ROLE=false   # test DB role is a superuser; FastAPI refuses it otherwise
uv run alembic upgrade head
uv run python scripts/contract_seed.py        # fixed users alice/bob/carol(inactive), ws-alpha/ws-beta
uv run uvicorn apps.api.main:app --port 18000 &
python3 scripts/contract_record.py --base http://127.0.0.1:18000 \
    --out ../../apps/backend-go/testdata/contract --cases scripts/contract_cases/auth.yaml
kill %1
```

Case files are YAML (PyYAML) or JSON (same structure, `.json` extension). Re-seed before every
recording so ids and `created_at` are stable; `last_login` and tokens are not.
Seed credentials are in `legacy/python/scripts/contract_seed.py` (test-only values).
Review `git diff` of the fixtures after re-recording: a changed fixture is a changed contract.
