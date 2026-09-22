"""Add durable workbook provider exposure receipts (execution integration pending)."""
from alembic import op
import sqlalchemy as sa
import os

revision = "1d2e3f405162"
down_revision = "0c1d2e3f4051"
branch_labels = None
depends_on = None


def upgrade():
    op.create_table(
        "workbook_spend_attempts",
        sa.Column("id", sa.String(36), primary_key=True),
        *[sa.Column(name, sa.String(length), nullable=False) for name, length in [
            ("workspace_id", 255), ("workbook_id", 36), ("run_id", 255),
            ("row_identity", 255), ("column_id", 255), ("provider", 255),
            ("attempt_key", 255), ("contract_hash", 64), ("status", 20),
        ]],
        sa.Column("reserved_microusd", sa.BigInteger(), nullable=False),
        sa.Column("settled_microusd", sa.BigInteger(), nullable=True),
        sa.Column("cost_basis", sa.JSON(), nullable=False),
        sa.Column("created_at", sa.Float(), nullable=False),
        sa.Column("updated_at", sa.Float(), nullable=False),
        sa.Column("result", sa.JSON(), nullable=True),
        sa.UniqueConstraint("workspace_id", "attempt_key", name="uq_workbook_spend_attempt_key"),
        sa.CheckConstraint("reserved_microusd >= 0", name="ck_workbook_spend_nonnegative"),
        sa.CheckConstraint("settled_microusd IS NULL OR settled_microusd >= 0", name="ck_workbook_spend_settlement_nonnegative"),
        sa.CheckConstraint("status IN ('reserved', 'dispatched', 'settled', 'released', 'uncertain')", name="ck_workbook_spend_status"),
    )
    op.create_index("ix_workbook_spend_exposure", "workbook_spend_attempts", ["workspace_id", "workbook_id", "status"])
    if op.get_bind().dialect.name == "postgresql":
        role = op.get_bind().dialect.identifier_preparer.quote(os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app"))
        op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON workbook_spend_attempts TO {role}")
        op.execute("ALTER TABLE workbook_spend_attempts ENABLE ROW LEVEL SECURITY")
        op.execute("ALTER TABLE workbook_spend_attempts FORCE ROW LEVEL SECURITY")
        op.execute("""CREATE POLICY workbook_spend_attempts_workspace_isolation ON workbook_spend_attempts
            USING (workspace_id = current_setting('app.workspace_id', true))
            WITH CHECK (workspace_id = current_setting('app.workspace_id', true))""")


def downgrade():
    op.drop_table("workbook_spend_attempts")
