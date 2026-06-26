"""mcp: scoped tokens (auth-plane) + RLS-scoped audit log (Phase-1 security)

Revision ID: b8c9d0e1f2a3
Revises: a7b8c9d0e1f2
Create Date: 2026-06-26

Phase 1 of the write-capable-MCP spec (features/research-write-capable-mcp-spec.md)
— SECURITY ONLY. Creates the two foundation tables that let the MCP bridge
authenticate + scope every tool call to a single tenant (closing the current
unauthenticated, tenant-blind hole). No write tools ship in this revision.

  * ``mcp_tokens``    — AUTH-PLANE (NOT RLS), like ``users``: hashed,
    single-workspace-bound, capability-scoped PAT. Looked up by ``token_hash``
    with no workspace GUC set, so it must NOT carry an RLS policy. Every row is
    still workspace-bound (no cross-workspace token); plaintext is never stored.
  * ``mcp_audit_log`` — RLS-scoped by ``workspace_id`` (fail-closed policy on
    ``current_setting('app.workspace_id', true)``), mirroring the leads/signals/
    workbooks RLS posture (c42d0273d9bd / e5f6a7b8c9d0) so audit reads can only
    see the caller's tenant.

SQLite/self-host gets the tables + indexes only (RLS DDL is dialect-guarded).
Additive, backward-safe DDL.
"""
import os
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = "b8c9d0e1f2a3"
down_revision: Union[str, Sequence[str], None] = "a7b8c9d0e1f2"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

# Must match apps.api.core.config.settings.APP_DB_ROLE (and the head migrations).
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")


def upgrade() -> None:
    bind = op.get_bind()
    is_pg = bind.dialect.name == "postgresql"

    # ── mcp_tokens (auth-plane, NOT RLS) ──────────────────────────────────────
    op.create_table(
        "mcp_tokens",
        sa.Column("id", sa.String(length=36), primary_key=True),
        sa.Column("token_hash", sa.String(length=64), nullable=False),
        sa.Column("prefix", sa.String(length=20), nullable=False),
        sa.Column("name", sa.String(length=200), nullable=False, server_default=""),
        sa.Column("user_id", sa.Integer(), nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("capabilities", sa.JSON(), nullable=False),
        sa.Column("expires_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("revoked_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("last_used_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()),
        sa.Column("created_by", sa.Integer(), nullable=True),
    )
    op.create_index("ix_mcp_tokens_token_hash", "mcp_tokens", ["token_hash"], unique=True)
    op.create_index("ix_mcp_tokens_workspace_id", "mcp_tokens", ["workspace_id"])
    op.create_index("ix_mcp_tokens_user_id", "mcp_tokens", ["user_id"])

    # ── mcp_audit_log (RLS-scoped) ────────────────────────────────────────────
    op.create_table(
        "mcp_audit_log",
        sa.Column("id", sa.String(length=36), primary_key=True),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("token_id", sa.String(length=36), nullable=True),
        sa.Column("user_id", sa.Integer(), nullable=True),
        sa.Column("tool_name", sa.String(length=100), nullable=False),
        sa.Column("arguments_redacted", sa.JSON(), nullable=False),
        sa.Column("idempotency_key", sa.String(length=128), nullable=True),
        sa.Column("result_status", sa.String(length=20), nullable=False, server_default="ok"),
        sa.Column("error", sa.Text(), nullable=True),
        sa.Column("estimated_cost_usd", sa.Float(), nullable=False, server_default="0"),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()),
        sa.UniqueConstraint("workspace_id", "idempotency_key", name="uq_mcp_audit_ws_idem"),
    )
    op.create_index("ix_mcp_audit_log_workspace_id", "mcp_audit_log", ["workspace_id"])
    op.create_index("ix_mcp_audit_log_ws_created", "mcp_audit_log", ["workspace_id", "created_at"])

    if is_pg:
        # mcp_tokens: app role needs full DML (lookup by hash, touch last_used,
        # mint/revoke via the REST router). NO RLS — it is an auth-plane table
        # read with no workspace GUC bound.
        op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON mcp_tokens TO {APP_DB_ROLE}")

        # mcp_audit_log: RLS-scoped, same fail-closed posture as the lead tables.
        op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON mcp_audit_log TO {APP_DB_ROLE}")
        op.execute("ALTER TABLE mcp_audit_log ENABLE ROW LEVEL SECURITY")
        op.execute("ALTER TABLE mcp_audit_log FORCE ROW LEVEL SECURITY")
        op.execute("DROP POLICY IF EXISTS mcp_audit_log_workspace_isolation ON mcp_audit_log")
        op.execute(
            """
            CREATE POLICY mcp_audit_log_workspace_isolation ON mcp_audit_log
            USING (workspace_id = current_setting('app.workspace_id', true))
            WITH CHECK (workspace_id = current_setting('app.workspace_id', true))
            """
        )


def downgrade() -> None:
    bind = op.get_bind()
    is_pg = bind.dialect.name == "postgresql"

    if is_pg:
        op.execute("DROP POLICY IF EXISTS mcp_audit_log_workspace_isolation ON mcp_audit_log")
        op.execute("ALTER TABLE mcp_audit_log NO FORCE ROW LEVEL SECURITY")
        op.execute("ALTER TABLE mcp_audit_log DISABLE ROW LEVEL SECURITY")

    op.drop_index("ix_mcp_audit_log_ws_created", table_name="mcp_audit_log")
    op.drop_index("ix_mcp_audit_log_workspace_id", table_name="mcp_audit_log")
    op.drop_table("mcp_audit_log")

    op.drop_index("ix_mcp_tokens_user_id", table_name="mcp_tokens")
    op.drop_index("ix_mcp_tokens_workspace_id", table_name="mcp_tokens")
    op.drop_index("ix_mcp_tokens_token_hash", table_name="mcp_tokens")
    op.drop_table("mcp_tokens")
