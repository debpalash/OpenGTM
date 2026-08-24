"""Tenant-scope and RLS-harden the complete entity graph.

Revision ID: a2b3c4d5e6f8
Revises: f2a3b4c5d6e7
Create Date: 2026-08-24
"""

import os
import re
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


revision: str = "a2b3c4d5e6f8"
down_revision: Union[str, Sequence[str], None] = "f2a3b4c5d6e7"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")
CHILD_TABLES = (
    "person_entities",
    "entity_blocking_keys",
    "entity_merge_log",
    "entity_review_pairs",
)
RLS_TABLES = ("company_entities",) + CHILD_TABLES


def _columns(bind, table: str) -> set[str]:
    return {column["name"] for column in sa.inspect(bind).get_columns(table)}


def _indexes(bind, table: str) -> set[str]:
    return {index["name"] for index in sa.inspect(bind).get_indexes(table)}


def upgrade() -> None:
    bind = op.get_bind()
    is_sqlite = bind.dialect.name == "sqlite"

    # Legacy create_all deployments can be explicitly adopted at the previous
    # revision without ever having run c42d0273d9bd (which normally creates the
    # group role). Ensure it exists before the grants below. Validate the
    # operator-controlled identifier before interpolating it into role DDL.
    if bind.dialect.name == "postgresql":
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE):
            raise RuntimeError("YUPCHA_APP_DB_ROLE is not a safe SQL identifier")
        op.execute(sa.text(
            f"""
            DO $$
            BEGIN
                IF NOT EXISTS (
                    SELECT 1 FROM pg_roles WHERE rolname = '{APP_DB_ROLE}'
                ) THEN
                    CREATE ROLE "{APP_DB_ROLE}"
                        NOLOGIN NOSUPERUSER NOBYPASSRLS INHERIT;
                END IF;
            END
            $$
            """
        ))

    for table in CHILD_TABLES:
        if "workspace_id" not in _columns(bind, table):
            op.add_column(table, sa.Column("workspace_id", sa.String(), nullable=True))

    # Recover tenant ownership only through authoritative relationships. Never
    # guess when an orphan cannot be attributed to exactly one workspace.
    bind.execute(sa.text(
        "UPDATE company_entities SET workspace_id = ("
        "SELECT MIN(r.workspace_id) FROM workbook_rows r "
        "WHERE r.canonical_entity_id = company_entities.id"
        ") WHERE workspace_id IS NULL OR workspace_id = ''"
    ))
    bind.execute(sa.text(
        "UPDATE person_entities SET workspace_id = ("
        "SELECT c.workspace_id FROM company_entities c "
        "WHERE c.id = person_entities.company_entity_id"
        ") WHERE workspace_id IS NULL"
    ))
    for table, entity_column in (
        ("entity_blocking_keys", "entity_id"),
        ("entity_review_pairs", "entity_id"),
        ("entity_merge_log", "kept_id"),
    ):
        bind.execute(sa.text(
            f"UPDATE {table} SET workspace_id = ("
            f"SELECT c.workspace_id FROM company_entities c "
            f"WHERE c.id = {table}.{entity_column}"
            f") WHERE workspace_id IS NULL"
        ))

    for table in RLS_TABLES:
        remaining = bind.execute(sa.text(
            f"SELECT count(*) FROM {table} "
            "WHERE workspace_id IS NULL OR workspace_id = ''"
        )).scalar() or 0
        if remaining:
            raise RuntimeError(
                f"entity RLS migration aborted: {remaining} row(s) in {table} "
                "cannot be attributed to a workspace"
            )

    for table in RLS_TABLES:
        if is_sqlite:
            with op.batch_alter_table(table) as batch:
                batch.alter_column(
                    "workspace_id", existing_type=sa.String(), nullable=False
                )
        else:
            op.alter_column(
                table, "workspace_id", existing_type=sa.String(), nullable=False
            )

    for table in CHILD_TABLES:
        index_name = f"ix_{table}_workspace_id"
        if index_name not in _indexes(bind, table):
            op.create_index(index_name, table, ["workspace_id"], unique=False)

    if bind.dialect.name == "postgresql":
        for table in RLS_TABLES:
            op.execute(sa.text(
                f'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "{table}" '
                f'TO "{APP_DB_ROLE}"'
            ))
            op.execute(sa.text(f'ALTER TABLE "{table}" ENABLE ROW LEVEL SECURITY'))
            op.execute(sa.text(f'ALTER TABLE "{table}" FORCE ROW LEVEL SECURITY'))
            op.execute(sa.text(
                f'DROP POLICY IF EXISTS workspace_isolation ON "{table}"'
            ))
            op.execute(sa.text(
                f'CREATE POLICY workspace_isolation ON "{table}" '
                "USING (workspace_id = current_setting('app.workspace_id', true)) "
                "WITH CHECK (workspace_id = current_setting('app.workspace_id', true))"
            ))
        for sequence in (
            "entity_blocking_keys_id_seq",
            "entity_merge_log_id_seq",
            "entity_review_pairs_id_seq",
        ):
            op.execute(sa.text(
                f'GRANT USAGE, SELECT ON SEQUENCE "{sequence}" TO "{APP_DB_ROLE}"'
            ))


def downgrade() -> None:
    bind = op.get_bind()
    if bind.dialect.name == "postgresql":
        for table in RLS_TABLES:
            op.execute(sa.text(
                f'DROP POLICY IF EXISTS workspace_isolation ON "{table}"'
            ))
            op.execute(sa.text(f'ALTER TABLE "{table}" NO FORCE ROW LEVEL SECURITY'))
            op.execute(sa.text(f'ALTER TABLE "{table}" DISABLE ROW LEVEL SECURITY'))

    if bind.dialect.name == "sqlite":
        with op.batch_alter_table("company_entities") as batch:
            batch.alter_column(
                "workspace_id", existing_type=sa.String(), nullable=True
            )
    else:
        op.alter_column(
            "company_entities", "workspace_id", existing_type=sa.String(), nullable=True
        )

    for table in reversed(CHILD_TABLES):
        index_name = f"ix_{table}_workspace_id"
        if index_name in _indexes(bind, table):
            op.drop_index(index_name, table_name=table)
        op.drop_column(table, "workspace_id")
