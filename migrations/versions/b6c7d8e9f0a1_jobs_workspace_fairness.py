"""Add indexed workspace ownership to the durable job queue.

Revision ID: b6c7d8e9f0a1
Revises: a5b6c7d8e9f0
"""

from alembic import op
import sqlalchemy as sa

revision = "b6c7d8e9f0a1"
down_revision = "a5b6c7d8e9f0"
branch_labels = None
depends_on = None


def upgrade():
    with op.batch_alter_table("jobs") as batch:
        batch.add_column(sa.Column("workspace_id", sa.String(), nullable=True))
        batch.create_index("ix_jobs_workspace_id", ["workspace_id"])
    bind = op.get_bind()
    if bind.dialect.name == "postgresql":
        op.execute("UPDATE jobs SET workspace_id = payload->>'workspace_id' WHERE workspace_id IS NULL AND payload->>'workspace_id' IS NOT NULL")
    else:
        op.execute("UPDATE jobs SET workspace_id = json_extract(payload, '$.workspace_id') WHERE workspace_id IS NULL AND json_valid(payload) AND json_extract(payload, '$.workspace_id') IS NOT NULL")


def downgrade():
    with op.batch_alter_table("jobs") as batch:
        batch.drop_index("ix_jobs_workspace_id")
        batch.drop_column("workspace_id")
