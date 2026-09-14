"""Add compound playbook history traversal indexes.

Revision ID: a13c9e7b42d6
Revises: e8f9a0b1c2d3
"""

from alembic import op

revision = "a13c9e7b42d6"
down_revision = "e8f9a0b1c2d3"
branch_labels = None
depends_on = None


def upgrade():
    op.create_index(
        "ix_playbook_runs_ws_playbook_cursor", "playbook_runs",
        ["workspace_id", "playbook_id", "created_at", "id"],
    )
    op.create_index(
        "ix_playbook_results_ws_run_cursor", "playbook_results",
        ["workspace_id", "run_id", "id"],
    )


def downgrade():
    op.drop_index("ix_playbook_results_ws_run_cursor", table_name="playbook_results")
    op.drop_index("ix_playbook_runs_ws_playbook_cursor", table_name="playbook_runs")
