"""Add append-only workspace governance audit events.

Revision ID: 61728394a5b6
Revises: 5061728394a5
"""
import os
import re
from alembic import op
import sqlalchemy as sa

revision = "61728394a5b6"
down_revision = "5061728394a5"
branch_labels = None
depends_on = None
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")


def upgrade():
    op.create_table("governance_audit_events",
        sa.Column("id", sa.String(36), primary_key=True),
        sa.Column("workspace_id", sa.String(64), nullable=False),
        sa.Column("actor_user_id", sa.Integer()), sa.Column("actor_role", sa.String(32), nullable=False),
        sa.Column("method", sa.String(10), nullable=False), sa.Column("route", sa.String(255), nullable=False),
        sa.Column("resource_path", sa.String(500), nullable=False), sa.Column("response_status", sa.Integer(), nullable=False),
        sa.Column("outcome", sa.String(20), nullable=False), sa.Column("request_id", sa.String(64), nullable=False),
        sa.Column("metadata_json", sa.JSON(), nullable=False), sa.Column("created_at", sa.DateTime(), nullable=False, server_default=sa.func.now()))
    for name, cols in (("ix_governance_audit_events_workspace_id", ["workspace_id"]), ("ix_governance_audit_ws_created", ["workspace_id", "created_at"]), ("ix_governance_audit_ws_actor", ["workspace_id", "actor_user_id"]), ("ix_governance_audit_request", ["request_id"])):
        op.create_index(name, "governance_audit_events", cols)
    if op.get_bind().dialect.name == "postgresql":
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE): raise RuntimeError("unsafe database role")
        op.execute(sa.text(f'GRANT SELECT, INSERT ON TABLE "governance_audit_events" TO "{APP_DB_ROLE}"'))
        op.execute(sa.text(f'ALTER TABLE "governance_audit_events" ENABLE ROW LEVEL SECURITY'))
        op.execute(sa.text(f'ALTER TABLE "governance_audit_events" FORCE ROW LEVEL SECURITY'))
        op.execute(sa.text("CREATE POLICY workspace_isolation ON governance_audit_events USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true))"))


def downgrade():
    op.drop_table("governance_audit_events")
