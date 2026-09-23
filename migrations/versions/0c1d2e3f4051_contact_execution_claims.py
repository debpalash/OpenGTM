"""Persist contact execution claims before provider spend."""

from alembic import op
import sqlalchemy as sa

revision = "0c1d2e3f4051"
down_revision = "f7923eb186fc"
branch_labels = None
depends_on = None


def upgrade():
    op.create_table(
        "contact_executions",
        sa.Column("id", sa.String(36), primary_key=True),
        sa.Column("workspace_id", sa.String(255), nullable=False),
        sa.Column("action_id", sa.String(255), nullable=False),
        sa.Column("contract_hash", sa.String(64), nullable=False),
        sa.Column("status", sa.String(20), nullable=False),
        sa.Column("started_at", sa.Float(), nullable=False),
        sa.Column("expires_at", sa.Float(), nullable=False),
        sa.Column("result", sa.JSON(), nullable=True),
        sa.UniqueConstraint("workspace_id", "action_id", name="uq_contact_execution_action"),
    )


def downgrade():
    op.drop_table("contact_executions")
