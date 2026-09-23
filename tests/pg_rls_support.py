"""Shared PostgreSQL FORCE-RLS session factory for PG-gated tests.

Migrates TEST_DATABASE_URL to head, then connects as a NON-super, NON-BYPASSRLS
login role (member of yupcha_app) with the production after_begin GUC hook, so
workspace policies are genuinely enforced.
"""

import os
import subprocess

from sqlalchemy import create_engine, event, text
from sqlalchemy.engine import make_url
from sqlalchemy.orm import sessionmaker

APP_LOGIN_ROLE = "app_rls_test"
APP_LOGIN_PASSWORD = "rls_test_only"


def rls_app_session(database_url: str, *, pool_size: int = 20):
    """Return (sessionmaker, dispose) for the app role under FORCE RLS."""
    owner = create_engine(database_url)
    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
    res = subprocess.run(["uv", "run", "alembic", "upgrade", "head"], cwd=repo_root,
                         env=dict(os.environ, DATABASE_URL=database_url),
                         capture_output=True, text=True)
    assert res.returncode == 0, f"alembic upgrade failed:\n{res.stdout}\n{res.stderr}"
    with owner.begin() as c:
        c.execute(text(
            f"""DO $$ BEGIN
              IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='{APP_LOGIN_ROLE}') THEN
                CREATE ROLE {APP_LOGIN_ROLE} LOGIN NOSUPERUSER NOBYPASSRLS;
              END IF; END $$;"""))
        c.execute(text(f"ALTER ROLE {APP_LOGIN_ROLE} PASSWORD '{APP_LOGIN_PASSWORD}'"))
        c.execute(text(f"GRANT yupcha_app TO {APP_LOGIN_ROLE}"))
        c.execute(text(f"GRANT USAGE ON SCHEMA public TO {APP_LOGIN_ROLE}"))
        c.execute(text(f"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO {APP_LOGIN_ROLE}"))
    owner.dispose()
    url = make_url(database_url).set(username=APP_LOGIN_ROLE, password=APP_LOGIN_PASSWORD)
    engine = create_engine(url.render_as_string(hide_password=False),
                           pool_size=pool_size, max_overflow=0)

    import apps.api.core.tenancy as tenancy
    factory = sessionmaker(bind=engine, autoflush=False)

    @event.listens_for(factory, "after_begin")
    def _guc(session, transaction, connection):  # noqa: ANN001
        ws = tenancy.current_workspace_var.get()
        if ws:
            connection.exec_driver_sql("SELECT set_config('app.workspace_id', %s, true)", (ws,))

    return factory, engine.dispose
