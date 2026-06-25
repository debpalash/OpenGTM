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
from apps.api.services.automations import models as _automations_models  # noqa: F401
from apps.api.services.outreach import orm_models as _outreach_orm_models  # noqa: F401

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


def migrate_outreach():
    """Migrate the legacy single-file outreach.db into the RLS tenant tables.

    Idempotent + resumable: synthesizes deterministic keys and uses ON CONFLICT
    DO NOTHING. Modeled on migrate_per_workspace_leads_signals — per-batch GUC so
    RLS WITH CHECK accepts the owner inserts. The non-unique integer lead_id is
    validated per workspace; unresolved enrollments are skipped (logged) and
    unresolved sends are written terminal as skipped/lead_not_found. Migrated
    sends are TERMINAL rows that NEVER pass through check_and_debit/caps/handle_send
    (charged_usd=0.0, migrated=true) — the ledger is untouched.
    """
    import json
    from datetime import datetime, timezone

    from sqlalchemy.dialects.postgresql import insert as pg_insert

    from apps.api.core.config import settings
    from apps.api.services.outreach.normalize import normalize_email
    from apps.api.services.outreach.orm_models import (
        OutreachEnrollment, OutreachSend, OutreachSequence,
    )

    src_path = os.path.join(settings.DATA_DIR, "outreach.db")
    if not os.path.exists(src_path):
        print(f"  - outreach.db not found at {src_path}; skipping outreach migration")
        return

    ws_id = os.environ.get("OUTREACH_MIGRATION_WS")
    if not ws_id:
        from apps.api.services.workspace import manager as ws_manager
        conn = ws_manager._get_db()
        row = conn.execute("SELECT id FROM workspaces WHERE slug='main'").fetchone()
        conn.close()
        ws_id = row["id"] if row else None
    if not ws_id:
        print("  ! no 'main' workspace and OUTREACH_MIGRATION_WS unset; skipping")
        return

    def _epoch_to_dt(v):
        if not v:
            return None
        try:
            return datetime.fromtimestamp(float(v), tz=timezone.utc)
        except (ValueError, OSError, TypeError):
            return None

    src = create_engine(f"sqlite:///{src_path}").connect()
    try:
        src_tables = set(src.execute(
            text("SELECT name FROM sqlite_master WHERE type='table'")
        ).scalars().all())
        seqs = [dict(r) for r in src.execute(text("SELECT * FROM sequences")).mappings().all()] if "sequences" in src_tables else []
        enrolls = [dict(r) for r in src.execute(text("SELECT * FROM sequence_leads")).mappings().all()] if "sequence_leads" in src_tables else []
        sends = [dict(r) for r in src.execute(text("SELECT * FROM send_log")).mappings().all()] if "send_log" in src_tables else []
    finally:
        src.close()

    n_seq = n_enr = n_enr_skip = n_send = n_send_skip = 0
    with pg_engine.begin() as dst:
        dst.execute(text("SELECT set_config('app.workspace_id', :ws, true)"), {"ws": ws_id})

        for sq in seqs:
            try:
                steps = json.loads(sq.get("steps") or "[]")
            except (json.JSONDecodeError, TypeError):
                steps = []
            payload = {
                "id": sq["id"],
                "workspace_id": ws_id,
                "name": sq.get("name") or "Migrated sequence",
                "description": sq.get("description") or "",
                "steps": steps,
                "status": sq.get("status") or "draft",
                "daily_limit": sq.get("daily_limit") or 50,
                "send_window_start": sq.get("send_window_start") if sq.get("send_window_start") is not None else 9,
                "send_window_end": sq.get("send_window_end") if sq.get("send_window_end") is not None else 18,
                "send_window_tz": "UTC",
                "consent_basis": "legacy_migrated",
            }
            res = dst.execute(
                pg_insert(OutreachSequence.__table__)
                .values(**payload)
                .on_conflict_do_nothing(index_elements=["id"])
            )
            n_seq += res.rowcount or 0

        # Resolve lead emails for snapshot/validation.
        def _lead_email(lid):
            r = dst.execute(
                text("SELECT email FROM leads WHERE workspace_id=:ws AND id=:lid"),
                {"ws": ws_id, "lid": lid},
            ).fetchone()
            return r[0] if r else None

        for en in enrolls:
            lid = en.get("lead_id")
            email = _lead_email(lid)
            if not email:
                n_enr_skip += 1
                print(f"  - enrollment lead_id={lid}: unresolved in ws {ws_id}, skipped")
                continue
            norm = normalize_email(email)
            if not norm:
                n_enr_skip += 1
                continue
            payload = {
                "workspace_id": ws_id,
                "sequence_id": en.get("sequence_id"),
                "lead_id": lid,
                "to_email_snapshot": norm,
                "consent_source": "legacy_import",
                "current_step": en.get("current_step") or 0,
                "status": en.get("status") or "pending",
                "next_send_at": _epoch_to_dt(en.get("next_send_at")),
                "sent_count": en.get("sent_count") or 0,
                "last_sent_at": _epoch_to_dt(en.get("last_sent_at")),
                "error": en.get("error") or "",
            }
            res = dst.execute(
                pg_insert(OutreachEnrollment.__table__)
                .values(**payload)
                .on_conflict_do_nothing(constraint="uq_enroll_ws_seq_lead")
            )
            n_enr += res.rowcount or 0

        for sd in sends:
            lid = sd.get("lead_id")
            resolved = _lead_email(lid) is not None
            status = sd.get("status") or "sent"
            skip_reason = None
            if not resolved:
                status, skip_reason = "skipped", "lead_not_found"
            payload = {
                "workspace_id": ws_id,
                "sequence_id": sd.get("sequence_id"),
                "lead_id": lid,
                "step_number": sd.get("step_number") or 0,
                "to_email": normalize_email(sd.get("to_email") or "") or "unknown@unknown",
                "subject": sd.get("subject") or "",
                "status": status,
                "skip_reason": skip_reason,
                "message_id": sd.get("message_id") or "",
                "idempotency_key": f"migrate:{sd.get('id')}",
                "charged_usd": 0.0,
                "migrated": True,
                "error": sd.get("error") or "",
                "sent_at": _epoch_to_dt(sd.get("sent_at")),
                "opened_at": _epoch_to_dt(sd.get("opened_at")),
                "replied_at": _epoch_to_dt(sd.get("replied_at")),
            }
            res = dst.execute(
                pg_insert(OutreachSend.__table__)
                .values(**payload)
                .on_conflict_do_nothing(constraint="uq_outreach_send_idem")
            )
            if skip_reason:
                n_send_skip += 1
            n_send += res.rowcount or 0

    print(
        f"✓ Outreach migration: {n_seq} sequences, {n_enr} enrollments "
        f"({n_enr_skip} skipped unresolved), {n_send} sends ({n_send_skip} lead_not_found)"
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
    _TENANT_TABLES = {
        "leads", "signals",
        # Outreach tables are loaded by migrate_outreach() under a per-batch GUC
        # (RLS-aware) from the legacy single-file outreach.db.
        "outreach_sequences", "outreach_enrollments", "outreach_sends",
        "outreach_suppressions", "outreach_schedules",
    }

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

    # Legacy outreach.db → tenant tables (RLS-aware, idempotent). MUST run after
    # leads so lead_id resolution works.
    migrate_outreach()

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
