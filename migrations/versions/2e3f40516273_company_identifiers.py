"""Add unique company identifiers so concurrent imports converge on one entity.

Backfills domain identifiers from existing entities. When two existing entities
already share a domain, the oldest keeps it and the other is left untouched for
human review (never auto-merged by a migration). Shared platform hosts such as
facebook.com never become identifiers.

Revision ID: 2e3f40516273
Revises: 1d2e3f405162
"""
import os

from alembic import op
import sqlalchemy as sa

revision = "2e3f40516273"
down_revision = "1d2e3f405162"
branch_labels = None
depends_on = None


def upgrade():
    from apps.api.services.entities.graph import identity_domain

    op.create_table(
        "company_identifiers",
        sa.Column("id", sa.Integer(), primary_key=True, autoincrement=True),
        sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("kind", sa.String(32), nullable=False),
        sa.Column("value", sa.String(255), nullable=False),
        sa.Column("entity_id", sa.String(),
                  sa.ForeignKey("company_entities.id", ondelete="CASCADE"), nullable=False),
        sa.Column("created_at", sa.DateTime(), server_default=sa.func.now()),
        sa.UniqueConstraint("workspace_id", "kind", "value", name="uq_company_identifier"),
    )
    op.create_index("ix_company_identifiers_workspace_id", "company_identifiers", ["workspace_id"])
    op.create_index("ix_company_identifiers_entity_id", "company_identifiers", ["entity_id"])

    bind = op.get_bind()
    table = sa.table(
        "company_identifiers",
        sa.column("workspace_id", sa.String), sa.column("kind", sa.String),
        sa.column("value", sa.String), sa.column("entity_id", sa.String),
    )
    claimed = set()
    rows = []
    for entity_id, workspace_id, primary_domain, identity_keys in bind.execute(sa.text(
        "SELECT id, workspace_id, primary_domain, identity_keys FROM company_entities "
        "ORDER BY first_seen, id"
    )):
        keys = identity_keys
        if isinstance(keys, str):
            import json
            try:
                keys = json.loads(keys)
            except ValueError:
                keys = {}
        candidates = [primary_domain, *((keys or {}).get("domains") or [])]
        for raw in candidates:
            domain = identity_domain(raw or "")
            key = (workspace_id or "", domain)
            if domain and key not in claimed:
                claimed.add(key)
                rows.append({"workspace_id": workspace_id or "", "kind": "domain",
                             "value": domain, "entity_id": entity_id})
    if rows:
        op.bulk_insert(table, rows)

    if bind.dialect.name == "postgresql":
        role = bind.dialect.identifier_preparer.quote(os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app"))
        op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON company_identifiers TO {role}")
        op.execute(f"GRANT USAGE, SELECT ON SEQUENCE company_identifiers_id_seq TO {role}")
        op.execute("ALTER TABLE company_identifiers ENABLE ROW LEVEL SECURITY")
        op.execute("ALTER TABLE company_identifiers FORCE ROW LEVEL SECURITY")
        op.execute("""CREATE POLICY workspace_isolation ON company_identifiers
            USING (workspace_id = current_setting('app.workspace_id', true))
            WITH CHECK (workspace_id = current_setting('app.workspace_id', true))""")


def downgrade():
    op.drop_table("company_identifiers")
