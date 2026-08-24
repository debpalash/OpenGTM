"""Explicitly adopt a legacy ``create_all`` PostgreSQL schema into Alembic.

This command is intentionally never called by application startup. It accepts
only the known legacy shape, creates additive model tables that old imports did
not register, stamps the last compatible revision, and lets normal migrations
perform the remaining ALTER/RLS work.

Always take a database backup first, then run::

    python -m apps.api.scripts.adopt_legacy_schema --yes-i-backed-up
"""

from __future__ import annotations

import argparse

from alembic import command
from alembic.config import Config
from sqlalchemy import inspect

from apps.api.database import Base, engine
from apps.api.db_init import _REPO_ROOT, _import_all_models


COMPATIBLE_REVISION = "f2a3b4c5d6e7"
REQUIRED_ANCHORS = {"users", "jobs", "workbooks", "workbook_rows"}
EXPECTED_MIGRATION_COLUMNS = {
    "person_entities": {"workspace_id"},
    "entity_blocking_keys": {"workspace_id"},
    "entity_merge_log": {"workspace_id"},
    "entity_review_pairs": {"workspace_id"},
}


def _config() -> Config:
    return Config(f"{_REPO_ROOT}/alembic.ini")


def adopt() -> None:
    if engine.dialect.name != "postgresql":
        raise RuntimeError("legacy schema adoption is only supported for PostgreSQL")

    _import_all_models()
    inspector = inspect(engine)
    live_tables = set(inspector.get_table_names(schema="public"))
    if "alembic_version" in live_tables:
        raise RuntimeError(
            "database already has an Alembic version table; run the normal migration command"
        )
    missing_anchors = sorted(REQUIRED_ANCHORS - live_tables)
    if missing_anchors:
        raise RuntimeError(
            "database is not the supported legacy schema; missing anchor table(s): "
            + ", ".join(missing_anchors)
        )

    model_tables = set(Base.metadata.tables)
    unexpected_missing_columns: list[str] = []
    for table in sorted(model_tables & live_tables):
        live_columns = {column["name"] for column in inspector.get_columns(table)}
        model_columns = {column.name for column in Base.metadata.tables[table].columns}
        missing = model_columns - live_columns
        allowed = EXPECTED_MIGRATION_COLUMNS.get(table, set())
        unexpected = missing - allowed
        if unexpected:
            unexpected_missing_columns.append(
                f"{table}: {', '.join(sorted(unexpected))}"
            )
    if unexpected_missing_columns:
        raise RuntimeError(
            "legacy schema is older or different than the supported adoption shape; "
            "missing model columns: " + "; ".join(unexpected_missing_columns)
        )

    missing_tables = sorted(model_tables - live_tables)
    if missing_tables:
        print("Creating additive model tables: " + ", ".join(missing_tables))
        Base.metadata.create_all(bind=engine, tables=[Base.metadata.tables[t] for t in missing_tables])

    cfg = _config()
    print(f"Stamping compatible legacy schema at {COMPATIBLE_REVISION}")
    command.stamp(cfg, COMPATIBLE_REVISION)
    print("Applying remaining Alembic migrations")
    command.upgrade(cfg, "head")
    print("Legacy schema adoption complete")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--yes-i-backed-up",
        action="store_true",
        help="required acknowledgement that a restorable database backup exists",
    )
    args = parser.parse_args()
    if not args.yes_i_backed_up:
        parser.error("refusing to adopt without --yes-i-backed-up")
    adopt()


if __name__ == "__main__":
    main()
