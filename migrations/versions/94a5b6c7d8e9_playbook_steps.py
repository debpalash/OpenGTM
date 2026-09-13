"""Add versioned multi-step research playbook definitions.

Revision ID: 94a5b6c7d8e9
Revises: 8394a5b6c7d8
"""
from alembic import op
import sqlalchemy as sa

revision = "94a5b6c7d8e9"
down_revision = "8394a5b6c7d8"
branch_labels = None
depends_on = None


def upgrade():
    with op.batch_alter_table("research_playbooks") as batch:
        batch.add_column(sa.Column("steps", sa.JSON(), nullable=False, server_default="[]"))
    with op.batch_alter_table("playbook_runs") as batch:
        batch.add_column(sa.Column("steps_snapshot", sa.JSON(), nullable=False, server_default="[]"))


def downgrade():
    with op.batch_alter_table("playbook_runs") as batch:
        batch.drop_column("steps_snapshot")
    with op.batch_alter_table("research_playbooks") as batch:
        batch.drop_column("steps")
