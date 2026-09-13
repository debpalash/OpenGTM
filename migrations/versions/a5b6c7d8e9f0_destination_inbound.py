"""Add authenticated idempotent CRM inbound reconciliation.

Revision ID: a5b6c7d8e9f0
Revises: 94a5b6c7d8e9
"""
import os
import re

from alembic import op
import sqlalchemy as sa

revision = "a5b6c7d8e9f0"
down_revision = "94a5b6c7d8e9"
branch_labels = None
depends_on = None
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")


def upgrade():
    op.create_table("destination_inbound_tokens", sa.Column("id", sa.String(36), primary_key=True), sa.Column("workspace_id", sa.String(64), nullable=False), sa.Column("destination_id", sa.String(), nullable=False), sa.Column("token_hash", sa.String(64), nullable=False), sa.Column("prefix", sa.String(20), nullable=False), sa.Column("created_by", sa.Integer()), sa.Column("created_at", sa.DateTime(), nullable=False, server_default=sa.func.now()), sa.Column("revoked_at", sa.DateTime()))
    op.create_index("ix_destination_inbound_token_hash", "destination_inbound_tokens", ["token_hash"], unique=True)
    op.create_index("ix_destination_inbound_tokens_workspace_id", "destination_inbound_tokens", ["workspace_id"])
    op.create_index("ix_destination_inbound_tokens_destination_id", "destination_inbound_tokens", ["destination_id"])
    op.create_table("destination_inbound_receipts", sa.Column("id", sa.String(36), primary_key=True), sa.Column("workspace_id", sa.String(64), nullable=False), sa.Column("destination_id", sa.String(), nullable=False), sa.Column("provider", sa.String(32), nullable=False), sa.Column("external_event_id", sa.String(255), nullable=False), sa.Column("external_record_id", sa.String(255)), sa.Column("lead_id", sa.Integer()), sa.Column("status", sa.String(24), nullable=False), sa.Column("conflict_policy", sa.String(24), nullable=False), sa.Column("applied_fields", sa.JSON(), nullable=False), sa.Column("ignored_fields", sa.JSON(), nullable=False), sa.Column("payload_fingerprint", sa.String(64), nullable=False), sa.Column("error", sa.Text()), sa.Column("created_at", sa.DateTime(), nullable=False, server_default=sa.func.now()), sa.UniqueConstraint("workspace_id", "destination_id", "external_event_id", name="uq_destination_inbound_event"))
    op.create_index("ix_destination_inbound_receipts_workspace_id", "destination_inbound_receipts", ["workspace_id"])
    op.create_index("ix_destination_inbound_receipts_destination_id", "destination_inbound_receipts", ["destination_id"])
    op.create_index("ix_destination_inbound_receipts_lead_id", "destination_inbound_receipts", ["lead_id"])
    op.create_index("ix_destination_inbound_ws_destination_created", "destination_inbound_receipts", ["workspace_id", "destination_id", "created_at"])
    if op.get_bind().dialect.name == "postgresql":
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE):
            raise RuntimeError("unsafe database role")
        op.execute(sa.text(f'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "destination_inbound_tokens" TO "{APP_DB_ROLE}"'))
        op.execute(sa.text(f'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "destination_inbound_receipts" TO "{APP_DB_ROLE}"'))
        op.execute(sa.text('ALTER TABLE "destination_inbound_receipts" ENABLE ROW LEVEL SECURITY'))
        op.execute(sa.text('ALTER TABLE "destination_inbound_receipts" FORCE ROW LEVEL SECURITY'))
        op.execute(sa.text('CREATE POLICY workspace_isolation ON "destination_inbound_receipts" USING (workspace_id = current_setting(\'app.workspace_id\', true)) WITH CHECK (workspace_id = current_setting(\'app.workspace_id\', true))'))


def downgrade():
    op.drop_table("destination_inbound_receipts")
    op.drop_table("destination_inbound_tokens")
