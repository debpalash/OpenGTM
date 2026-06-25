"""One-off migration: copy the SQLAlchemy-managed tables from the legacy SQLite
file (data/data.db) into the local Postgres database.

Usage (from apps/api, so apps.api imports resolve):
    PYTHONPATH=../.. uv run python scripts/migrate_sqlite_to_pg.py

It:
  1. create_all() the full current schema on Postgres (the live engine).
  2. For each mapped table in FK order, read all rows from SQLite and bulk-insert
     into Postgres using the typed Table objects (so JSON/Boolean/DateTime columns
     convert correctly across backends).
  3. Reset Postgres identity sequences to max(id)+1 so new inserts don't collide.

Idempotency: Postgres tables are TRUNCATEd before load, so it can be re-run.
"""
import os
import sys

from sqlalchemy import create_engine, select, insert, text

# Import the live engine (Postgres, per config) + Base with all models registered.
from apps.api.database import engine as pg_engine, Base

# Force-import every module that defines a model so Base.metadata is complete —
# mirrors the import block in apps/api/main.py.
import apps.api.models  # noqa: F401
from apps.api.services.workbook import models as _wb_models  # noqa: F401
from apps.api.services.entities import models as _entity_models  # noqa: F401
from apps.api.services.workbook import planner_models as _planner_models  # noqa: F401
from apps.api.services.workbook import activity_models as _activity_models  # noqa: F401
from apps.api.services.workbook import trace_models as _trace_models  # noqa: F401
from apps.api.services.leadgen import orm_models as _leadgen_orm_models  # noqa: F401

SQLITE_PATH = os.environ.get(
    "LEGACY_SQLITE",
    os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "..", "data", "data.db")),
)


def _iter_workspaces():
    """Yield (workspace_id, slug, leads_db_path, signals_db_path) for every
    workspace, resolving the per-workspace SQLite file locations the way the app
    does (services/workspace/manager.workspace_leads_db_path).

    The legacy `signals.db` was a SINGLE global file (no per-workspace split and
    no workspace_id column), so all its rows are attributed to the `main`
    workspace. Per-workspace signals only exist going forward via the PG store.
    """
    from apps.api.services.workspace import manager as ws_manager

    project_root = os.path.abspath(
        os.path.join(os.path.dirname(__file__), "..", "..", "..")
    )
    global_signals = os.path.join(project_root, "data", "signals.db")

    conn = ws_manager._get_db()
    rows = conn.execute("SELECT id, slug FROM workspaces").fetchall()
    conn.close()
    for r in rows:
        ws_id, slug = r["id"], r["slug"]
        leads_path = ws_manager.workspace_leads_db_path(slug)
        sig_path = global_signals if slug == "main" else None
        yield ws_id, slug, leads_path, sig_path


def migrate_per_workspace_leads_signals():
    """Load each workspace's per-file leads (and the main signals.db) into the
    shared, RLS-protected Postgres `leads`/`signals` tables, stamping
    workspace_id from the workspace. Run as the OWNER; FORCE RLS applies to the
    owner too, so we set app.workspace_id per batch (WITH CHECK then passes).
    """
    from apps.api.services.leadgen.orm_models import LeadRow, SignalRow

    lead_cols = {c.name for c in LeadRow.__table__.columns} - {"id", "search_tsv"}
    sig_cols = {c.name for c in SignalRow.__table__.columns}

    total_leads = total_sigs = 0
    for ws_id, slug, leads_path, sig_path in _iter_workspaces():
        with pg_engine.begin() as dst:
            # Scope this batch so RLS WITH CHECK accepts the inserts (FORCE RLS
            # binds the owner too). is_local=true → reset at txn end.
            dst.execute(
                text("SELECT set_config('app.workspace_id', :ws, true)"),
                {"ws": ws_id},
            )

            # ── leads ──
            if leads_path and os.path.exists(leads_path):
                src = create_engine(f"sqlite:///{leads_path}").connect()
                try:
                    raw = [dict(r) for r in src.execute(
                        text("SELECT * FROM leads")
                    ).mappings().all()]
                finally:
                    src.close()
                # Pre-collapse (company, city) collisions within the workspace
                # (composite unique is (workspace_id, company, city)); last wins.
                collapsed = {}
                for row in raw:
                    payload = {k: v for k, v in row.items() if k in lead_cols}
                    payload["workspace_id"] = ws_id
                    collapsed[(payload.get("company"), payload.get("city"))] = payload
                rows = list(collapsed.values())
                if rows:
                    dst.execute(insert(LeadRow.__table__), rows)
                    total_leads += len(rows)
                    print(f"  ✓ {slug} leads: {len(rows)} rows (from {leads_path})")

            # ── signals (main only — legacy single file) ──
            if sig_path and os.path.exists(sig_path):
                src = create_engine(f"sqlite:///{sig_path}").connect()
                try:
                    raw = [dict(r) for r in src.execute(
                        text("SELECT * FROM signals")
                    ).mappings().all()]
                finally:
                    src.close()
                rows = []
                for row in raw:
                    payload = {k: v for k, v in row.items() if k in sig_cols}
                    payload["workspace_id"] = ws_id
                    payload["read"] = bool(payload.get("read"))
                    rows.append(payload)
                if rows:
                    dst.execute(insert(SignalRow.__table__), rows)
                    total_sigs += len(rows)
                    print(f"  ✓ {slug} signals: {len(rows)} rows (from {sig_path})")

    print(
        f"✓ Per-workspace load complete: {total_leads} leads, {total_sigs} signals"
    )


def main():
    assert pg_engine.dialect.name == "postgresql", (
        f"Expected Postgres engine, got {pg_engine.dialect.name}. "
        "Set DATABASE_URL to the Postgres URL before running."
    )
    sqlite_engine = create_engine(f"sqlite:///{SQLITE_PATH}")
    print(f"Source SQLite : {SQLITE_PATH}")
    print(f"Target Postgres: {pg_engine.url}")

    # 1. Build schema on Postgres via Alembic so the shared leads/signals tables
    #    get their RLS policies, tsvector/GIN, app role and grants (create_all()
    #    cannot emit any of that). Falls back to create_all() for the non-tenant
    #    tables only if Alembic is unavailable.
    try:
        from apps.api.db_init import _alembic_upgrade_head
        _alembic_upgrade_head()
        print("✓ alembic upgrade head done on Postgres")
    except Exception as exc:
        print(f"! alembic upgrade failed ({exc}); falling back to create_all() "
              "— RLS/tsvector will be MISSING, run 'alembic upgrade head' manually")
        Base.metadata.create_all(bind=pg_engine)

    sqlite_tables = set(create_engine(f"sqlite:///{SQLITE_PATH}").connect().execute(
        text("SELECT name FROM sqlite_master WHERE type='table'")
    ).scalars().all())

    tables = list(Base.metadata.sorted_tables)  # FK-safe order (parents first)
    # The shared multi-tenant tables are loaded separately, PER WORKSPACE, with
    # the RLS GUC set per batch (see migrate_per_workspace_leads_signals). Their
    # source is the per-workspace leads.db files, NOT the single data.db, and a
    # plain insert here would hit the RLS WITH CHECK with no app.workspace_id.
    _TENANT_TABLES = {"leads", "signals"}

    total = 0
    with sqlite_engine.connect() as src, pg_engine.begin() as dst:
        # Truncate children-first (reverse FK order) for a clean idempotent load.
        for table in reversed(tables):
            dst.execute(text(f'TRUNCATE TABLE "{table.name}" RESTART IDENTITY CASCADE'))

        for table in tables:
            if table.name in _TENANT_TABLES:
                continue
            if table.name not in sqlite_tables:
                print(f"  - {table.name}: not in SQLite, skipping")
                continue
            rows = [dict(r) for r in src.execute(select(table)).mappings().all()]
            if not rows:
                print(f"  - {table.name}: 0 rows")
                continue
            dst.execute(insert(table), rows)
            total += len(rows)
            print(f"  ✓ {table.name}: {len(rows)} rows")

    # Per-workspace leads/signals load (stamps workspace_id, RLS-aware).
    migrate_per_workspace_leads_signals()

    # 3. Reset identity sequences to max(id)+1 for integer PK tables.
    with pg_engine.begin() as dst:
        for table in tables:
            pk_cols = [c for c in table.primary_key.columns]
            if len(pk_cols) == 1 and pk_cols[0].autoincrement and str(pk_cols[0].type).upper().startswith("INTEGER"):
                col = pk_cols[0].name
                dst.execute(text(
                    f"SELECT setval(pg_get_serial_sequence('\"{table.name}\"', '{col}'), "
                    f"COALESCE((SELECT MAX(\"{col}\") FROM \"{table.name}\"), 1), true)"
                ))

    print(f"✓ Migration complete: {total} rows copied across {len(tables)} tables")


if __name__ == "__main__":
    main()
