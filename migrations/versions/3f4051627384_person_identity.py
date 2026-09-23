"""Persist person identity: unique identifiers and employment history.

Revision ID: 3f4051627384
Revises: 2e3f40516273
"""
import os

from alembic import op
import sqlalchemy as sa

revision = "3f4051627384"
down_revision = "2e3f40516273"
branch_labels = None
depends_on = None

TABLES = ("person_identifiers", "person_employments")


def upgrade():
    op.create_table(
        "person_identifiers",
        sa.Column("id", sa.Integer(), primary_key=True, autoincrement=True),
        sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("kind", sa.String(32), nullable=False),
        sa.Column("value", sa.String(512), nullable=False),
        sa.Column("person_id", sa.String(),
                  sa.ForeignKey("person_entities.id", ondelete="CASCADE"), nullable=False),
        sa.Column("created_at", sa.DateTime(), server_default=sa.func.now()),
        sa.UniqueConstraint("workspace_id", "kind", "value", name="uq_person_identifier"),
    )
    op.create_index("ix_person_identifiers_workspace_id", "person_identifiers", ["workspace_id"])
    op.create_index("ix_person_identifiers_person_id", "person_identifiers", ["person_id"])
    op.create_table(
        "person_employments",
        sa.Column("id", sa.Integer(), primary_key=True, autoincrement=True),
        sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("person_id", sa.String(),
                  sa.ForeignKey("person_entities.id", ondelete="CASCADE"), nullable=False),
        sa.Column("company_key", sa.String(512), nullable=False),
        sa.Column("company_entity_id", sa.String(),
                  sa.ForeignKey("company_entities.id", ondelete="SET NULL"), nullable=True),
        sa.Column("company_name", sa.String(512)),
        sa.Column("company_domain", sa.String(255)),
        sa.Column("title", sa.String(512)),
        sa.Column("titles", sa.JSON()),
        sa.Column("source", sa.String(255)),
        sa.Column("evidence_url", sa.String(1024)),
        sa.Column("first_observed_at", sa.DateTime(), nullable=False),
        sa.Column("last_observed_at", sa.DateTime(), nullable=False),
        sa.Column("is_current", sa.Integer(), nullable=False, server_default="1"),
        sa.UniqueConstraint("workspace_id", "person_id", "company_key", name="uq_person_employment"),
    )
    op.create_index("ix_person_employments_workspace_id", "person_employments", ["workspace_id"])
    op.create_index("ix_person_employments_person_id", "person_employments", ["person_id"])
    op.create_index("ix_person_employments_company_entity_id", "person_employments", ["company_entity_id"])

    bind = op.get_bind()
    if bind.dialect.name == "postgresql":
        role = bind.dialect.identifier_preparer.quote(os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app"))
        for table in TABLES:
            op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON {table} TO {role}")
            op.execute(f"GRANT USAGE, SELECT ON SEQUENCE {table}_id_seq TO {role}")
            op.execute(f"ALTER TABLE {table} ENABLE ROW LEVEL SECURITY")
            op.execute(f"ALTER TABLE {table} FORCE ROW LEVEL SECURITY")
            op.execute(f"""CREATE POLICY workspace_isolation ON {table}
                USING (workspace_id = current_setting('app.workspace_id', true))
                WITH CHECK (workspace_id = current_setting('app.workspace_id', true))""")


def downgrade():
    op.drop_table("person_employments")
    op.drop_table("person_identifiers")
