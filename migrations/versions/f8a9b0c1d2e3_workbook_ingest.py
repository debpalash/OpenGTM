"""ingest: per-workbook ingest tokens (auth-plane) + RLS-scoped idempotency ledger

Revision ID: f8a9b0c1d2e3
Revises: d0e1f2a3b4c5
Create Date: 2026-07-10

Inbound rows API ("webhook source") — lets external systems push rows INTO a
workbook via POST /api/v2/workbooks/{id}/rows/ingest (flag-gated by
INGEST_API_ENABLED, default OFF). Two tables:

  * ``workbook_ingest_tokens``      — AUTH-PLANE (NOT RLS), like ``mcp_tokens``
    (b8c9d0e1f2a3): looked up by sha256 ``token_hash`` with no workspace GUC
    set, so it must NOT carry an RLS policy. Every row is still bound to
    exactly one (workspace_id, workbook_id); plaintext is never stored.
  * ``workbook_ingest_idempotency`` — RLS-scoped by ``workspace_id``
    (fail-closed policy on ``current_setting('app.workspace_id', true)``),
    mirroring the workbooks RLS posture (e5f6a7b8c9d0). Stores recent
    Idempotency-Key results per workbook so a replayed ingest is a no-op.

SQLite/self-host gets the tables + indexes only (RLS DDL is dialect-guarded).
Additive, backward-safe DDL.
"""
import os
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = "f8a9b0c1d2e3"
down_revision: Union[str, Sequence[str], None] = "d0e1f2a3b4c5"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

# Must match apps.api.core.config.settings.APP_DB_ROLE (and the head migrations).
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")


def upgrade() -> None:
    bind = op.get_bind()
    is_pg = bind.dialect.name == "postgresql"

    # ── workbook_ingest_tokens (auth-plane, NOT RLS) ─────────────────────────
    op.create_table(
        "workbook_ingest_tokens",
        sa.Column("id", sa.String(length=36), primary_key=True),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("workbook_id", sa.String(), nullable=False),
        sa.Column("token_hash", sa.String(length=64), nullable=False),
        sa.Column("prefix", sa.String(length=20), nullable=False),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()),
        sa.Column("revoked_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("created_by", sa.Integer(), nullable=True),
    )
    op.create_index(
        "ix_workbook_ingest_tokens_token_hash", "workbook_ingest_tokens",
        ["token_hash"], unique=True,
    )
    op.create_index(
        "ix_workbook_ingest_tokens_workspace_id", "workbook_ingest_tokens",
        ["workspace_id"],
    )
    op.create_index(
        "ix_workbook_ingest_tokens_workbook_id", "workbook_ingest_tokens",
        ["workbook_id"],
    )

    # ── workbook_ingest_idempotency (RLS-scoped) ─────────────────────────────
    op.create_table(
        "workbook_ingest_idempotency",
        sa.Column("id", sa.String(length=36), primary_key=True),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("workbook_id", sa.String(), nullable=False),
        sa.Column("idempotency_key", sa.String(length=128), nullable=False),
        sa.Column("response", sa.JSON(), nullable=False),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()),
        sa.UniqueConstraint("workbook_id", "idempotency_key", name="uq_wb_ingest_idem"),
    )
    op.create_index(
        "ix_workbook_ingest_idem_workspace_id", "workbook_ingest_idempotency",
        ["workspace_id"],
    )
    op.create_index(
        "ix_workbook_ingest_idem_workbook_id", "workbook_ingest_idempotency",
        ["workbook_id"],
    )

    if is_pg:
        # workbook_ingest_tokens: app role needs full DML (lookup by hash, mint/
        # rotate via the REST router). NO RLS — auth-plane table read with no
        # workspace GUC bound (mirrors mcp_tokens).
        op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON workbook_ingest_tokens TO {APP_DB_ROLE}")

        # workbook_ingest_idempotency: RLS-scoped, same fail-closed posture as
        # the workbook tables (e5f6a7b8c9d0).
        op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON workbook_ingest_idempotency TO {APP_DB_ROLE}")
        op.execute("ALTER TABLE workbook_ingest_idempotency ENABLE ROW LEVEL SECURITY")
        op.execute("ALTER TABLE workbook_ingest_idempotency FORCE ROW LEVEL SECURITY")
        op.execute("DROP POLICY IF EXISTS workbook_ingest_idempotency_workspace_isolation ON workbook_ingest_idempotency")
        op.execute(
            """
            CREATE POLICY workbook_ingest_idempotency_workspace_isolation ON workbook_ingest_idempotency
            USING (workspace_id = current_setting('app.workspace_id', true))
            WITH CHECK (workspace_id = current_setting('app.workspace_id', true))
            """
        )


def downgrade() -> None:
    bind = op.get_bind()
    is_pg = bind.dialect.name == "postgresql"

    if is_pg:
        op.execute("DROP POLICY IF EXISTS workbook_ingest_idempotency_workspace_isolation ON workbook_ingest_idempotency")
        op.execute("ALTER TABLE workbook_ingest_idempotency NO FORCE ROW LEVEL SECURITY")
        op.execute("ALTER TABLE workbook_ingest_idempotency DISABLE ROW LEVEL SECURITY")

    op.drop_index("ix_workbook_ingest_idem_workbook_id", table_name="workbook_ingest_idempotency")
    op.drop_index("ix_workbook_ingest_idem_workspace_id", table_name="workbook_ingest_idempotency")
    op.drop_table("workbook_ingest_idempotency")

    op.drop_index("ix_workbook_ingest_tokens_workbook_id", table_name="workbook_ingest_tokens")
    op.drop_index("ix_workbook_ingest_tokens_workspace_id", table_name="workbook_ingest_tokens")
    op.drop_index("ix_workbook_ingest_tokens_token_hash", table_name="workbook_ingest_tokens")
    op.drop_table("workbook_ingest_tokens")
