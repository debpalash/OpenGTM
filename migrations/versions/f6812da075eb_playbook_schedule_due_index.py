"""Index due playbook schedule restart traversal.

Revision ID: f6812da075eb
Revises: e5701c9f64da
"""

from alembic import op

revision = "f6812da075eb"
down_revision = "e5701c9f64da"
branch_labels = None
depends_on = None


def upgrade():
    op.create_index(
        "ix_playbook_schedules_due_cursor",
        "playbook_schedules",
        ["enabled", "next_run_at", "playbook_id"],
    )


def downgrade():
    op.drop_index(
        "ix_playbook_schedules_due_cursor",
        table_name="playbook_schedules",
    )
