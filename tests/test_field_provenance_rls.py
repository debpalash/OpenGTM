"""Per-fact provenance rides the leads-table RLS (PG-gated). Spec AC8/AC9.

The new ``leads.field_provenance`` column adds NO new RLS policy — it inherits
the leads table's existing workspace-scoped policy (c42d0273d9bd). This proves
the invariant on a real Postgres under a NON-super, NON-BYPASSRLS role: a session
scoped to workspace A sees only A's lead (and its field_provenance) and can never
read workspace B's row/provenance.

GATED on TEST_DATABASE_URL; SKIPs otherwise (mirrors test_workbook_worker_rls).

Run:
    TEST_DATABASE_URL='postgresql+psycopg://user4@localhost:5432/<throwaway>' \
    PYTHONPATH=. uv run --group dev --with 'psycopg[binary]' \
    python -m pytest tests/test_field_provenance_rls.py -q
"""
import json
import os
import subprocess

import pytest
from sqlalchemy import create_engine, event, text
from sqlalchemy.orm import sessionmaker

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL,
    reason="TEST_DATABASE_URL not set (Postgres provenance-RLS test skipped)",
)

APP_LOGIN_ROLE = "app_rls_test"
APP_LOGIN_PASSWORD = "rls_test_only"
WA = "ws_prov_alpha"
WB = "ws_prov_beta"


def _app_url():
    from sqlalchemy.engine import make_url
    u = make_url(TEST_DATABASE_URL)
    return u.set(username=APP_LOGIN_ROLE, password=APP_LOGIN_PASSWORD).render_as_string(hide_password=False)


@pytest.fixture(scope="module")
def owner_engine():
    eng = create_engine(TEST_DATABASE_URL)
    yield eng
    eng.dispose()


@pytest.fixture(scope="module")
def app_engine(owner_engine):
    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
    env = dict(os.environ, DATABASE_URL=TEST_DATABASE_URL)
    res = subprocess.run(
        ["uv", "run", "--with", "psycopg[binary]", "alembic", "upgrade", "head"],
        cwd=repo_root, env=env, capture_output=True, text=True,
    )
    assert res.returncode == 0, f"alembic upgrade failed:\n{res.stdout}\n{res.stderr}"

    with owner_engine.begin() as c:
        c.execute(text(
            f"""DO $$ BEGIN
              IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='{APP_LOGIN_ROLE}') THEN
                CREATE ROLE {APP_LOGIN_ROLE} LOGIN NOSUPERUSER NOBYPASSRLS;
              END IF; END $$;"""
        ))
        c.execute(text(f"ALTER ROLE {APP_LOGIN_ROLE} PASSWORD '{APP_LOGIN_PASSWORD}'"))
        c.execute(text(f"GRANT yupcha_app TO {APP_LOGIN_ROLE}"))
        c.execute(text(f"GRANT USAGE ON SCHEMA public TO {APP_LOGIN_ROLE}"))
        c.execute(text(f"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO {APP_LOGIN_ROLE}"))
        # Owner-seed one lead per tenant, each carrying field_provenance.
        for ws, src in ((WA, "gleif"), (WB, "hunter_io")):
            fp = json.dumps({"email": {"source": src, "license": "x",
                                       "confidence": 0.9, "fetched_at": "2026-01-01T00:00:00+00:00"}})
            c.execute(text(
                "INSERT INTO leads (workspace_id, company, city, field_provenance) "
                "VALUES (:w, :co, :ci, :fp)"
            ), {"w": ws, "co": "Acme", "ci": ws, "fp": fp})

    eng = create_engine(_app_url())
    yield eng
    eng.dispose()
    with owner_engine.begin() as c:
        c.execute(text("DELETE FROM leads WHERE workspace_id IN (:a, :b)"),
                  {"a": WA, "b": WB})


def _scoped_session(app_engine, ws):
    Session = sessionmaker(bind=app_engine, autoflush=False)

    @event.listens_for(Session, "after_begin")
    def _guc(session, transaction, connection):  # noqa: ANN001
        connection.exec_driver_sql(
            "SELECT set_config('app.workspace_id', %s, true)", (ws,)
        )
    return Session


def test_provenance_isolated_by_leads_rls(app_engine):
    from apps.api.services.leadgen.orm_models import LeadRow

    SA = _scoped_session(app_engine, WA)
    with SA() as s:
        rows = s.query(LeadRow).all()
        assert {r.workspace_id for r in rows} == {WA}, "workspace A must see only its own row"
        prov = json.loads(rows[0].field_provenance)
        assert prov["email"]["source"] == "gleif"
        # Cannot reach workspace B's row (and therefore its provenance) at all.
        assert s.query(LeadRow).filter(LeadRow.workspace_id == WB).count() == 0

    SB = _scoped_session(app_engine, WB)
    with SB() as s:
        rows = s.query(LeadRow).all()
        assert {r.workspace_id for r in rows} == {WB}
        assert json.loads(rows[0].field_provenance)["email"]["source"] == "hunter_io"
