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

SQLITE_PATH = os.environ.get(
    "LEGACY_SQLITE",
    os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "..", "data", "data.db")),
)


def main():
    assert pg_engine.dialect.name == "postgresql", (
        f"Expected Postgres engine, got {pg_engine.dialect.name}. "
        "Set DATABASE_URL to the Postgres URL before running."
    )
    sqlite_engine = create_engine(f"sqlite:///{SQLITE_PATH}")
    print(f"Source SQLite : {SQLITE_PATH}")
    print(f"Target Postgres: {pg_engine.url}")

    # 1. Build schema on Postgres.
    Base.metadata.create_all(bind=pg_engine)
    print("✓ create_all() done on Postgres")

    sqlite_tables = set(create_engine(f"sqlite:///{SQLITE_PATH}").connect().execute(
        text("SELECT name FROM sqlite_master WHERE type='table'")
    ).scalars().all())

    tables = list(Base.metadata.sorted_tables)  # FK-safe order (parents first)

    total = 0
    with sqlite_engine.connect() as src, pg_engine.begin() as dst:
        # Truncate children-first (reverse FK order) for a clean idempotent load.
        for table in reversed(tables):
            dst.execute(text(f'TRUNCATE TABLE "{table.name}" RESTART IDENTITY CASCADE'))

        for table in tables:
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
