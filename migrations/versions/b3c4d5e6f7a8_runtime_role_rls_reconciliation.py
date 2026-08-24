"""Reconcile runtime grants, tenant RLS, FTS, and job single-flight.

Revision ID: b3c4d5e6f7a8
Revises: a2b3c4d5e6f8
Create Date: 2026-08-24

This is intentionally idempotent. Besides hardening normal Alembic installs,
it lets an explicitly adopted legacy ``create_all`` database receive every
security object that ORM metadata cannot represent.
"""

import os
import re
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


revision: str = "b3c4d5e6f7a8"
down_revision: Union[str, Sequence[str], None] = "a2b3c4d5e6f8"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")

# Auth-plane/scheduler-mirror tables are deliberately absent: they must be
# readable before a tenant GUC is known. Every tenant data table is fail-closed.
RLS_TABLES = (
    "leads",
    "signals",
    "workbooks",
    "workbook_rows",
    "workbook_enrichments",
    "workbook_activity",
    "cell_traces",
    "workbook_views",
    "workbook_ingest_idempotency",
    "triggers",
    "trigger_runs",
    "trigger_action_results",
    "trigger_cap_reservations",
    "outreach_sequences",
    "outreach_enrollments",
    "outreach_sends",
    "outreach_suppressions",
    "outreach_inbound_messages",
    "watch_subscriptions",
    "poll_budget_ledger",
    "mcp_audit_log",
    "company_entities",
    "person_entities",
    "entity_blocking_keys",
    "entity_merge_log",
    "entity_review_pairs",
    "workspace_credits",
    "credit_ledger_entries",
)

_FTS_COLUMNS = ("company", "city", "specialization", "notes", "description")


def _safe_role() -> str:
    if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE):
        raise RuntimeError("YUPCHA_APP_DB_ROLE is not a safe SQL identifier")
    return APP_DB_ROLE


def upgrade() -> None:
    bind = op.get_bind()
    if bind.dialect.name != "postgresql":
        return

    role = _safe_role()
    op.execute(sa.text(
        f"""
        DO $$
        BEGIN
            IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '{role}') THEN
                CREATE ROLE "{role}" NOLOGIN NOSUPERUSER NOBYPASSRLS INHERIT;
            END IF;
        END
        $$
        """
    ))

    live_tables = set(sa.inspect(bind).get_table_names(schema="public"))
    missing = sorted(set(RLS_TABLES) - live_tables)
    if missing:
        raise RuntimeError(
            "RLS reconciliation aborted; expected tenant tables are missing: "
            + ", ".join(missing)
        )

    # The runtime group needs the whole application schema. Tenant-bearing
    # tables below remain protected by forced RLS; auth-plane and queue tables
    # are application-internal and intentionally usable without a workspace GUC.
    op.execute(sa.text(f'GRANT USAGE ON SCHEMA public TO "{role}"'))
    op.execute(sa.text(
        f'GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO "{role}"'
    ))
    op.execute(sa.text(
        f'GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO "{role}"'
    ))
    op.execute(sa.text(
        f'ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT '
        f'SELECT, INSERT, UPDATE, DELETE ON TABLES TO "{role}"'
    ))
    op.execute(sa.text(
        f'ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT '
        f'USAGE, SELECT ON SEQUENCES TO "{role}"'
    ))

    # Remove all pre-existing policies before creating one canonical restrictive
    # boundary. PostgreSQL ORs permissive policies, so retaining an accidental
    # allow-all policy would silently defeat isolation.
    for table in RLS_TABLES:
        policies = bind.execute(
            sa.text(
                "SELECT policyname FROM pg_policies "
                "WHERE schemaname='public' AND tablename=:table"
            ),
            {"table": table},
        ).scalars().all()
        quoted_table = bind.dialect.identifier_preparer.quote(table)
        for policy in policies:
            quoted_policy = bind.dialect.identifier_preparer.quote(policy)
            op.execute(sa.text(
                f"DROP POLICY {quoted_policy} ON {quoted_table}"
            ))
        op.execute(sa.text(f"ALTER TABLE {quoted_table} ENABLE ROW LEVEL SECURITY"))
        op.execute(sa.text(f"ALTER TABLE {quoted_table} FORCE ROW LEVEL SECURITY"))
        op.execute(sa.text(
            f"CREATE POLICY workspace_isolation ON {quoted_table} "
            "USING (workspace_id = current_setting('app.workspace_id', true)) "
            "WITH CHECK (workspace_id = current_setting('app.workspace_id', true))"
        ))

    # Recreate the migration-only FTS objects missed by legacy create_all.
    columns = " || ' ' || ".join(f"coalesce(NEW.{c}, '')" for c in _FTS_COLUMNS)
    op.execute(sa.text("ALTER TABLE leads ADD COLUMN IF NOT EXISTS search_tsv tsvector"))
    op.execute(sa.text(
        f"""
        CREATE OR REPLACE FUNCTION leads_search_tsv_trigger() RETURNS trigger AS $$
        BEGIN
            NEW.search_tsv := to_tsvector('english', {columns});
            RETURN NEW;
        END
        $$ LANGUAGE plpgsql
        """
    ))
    op.execute(sa.text("DROP TRIGGER IF EXISTS leads_search_tsv_update ON leads"))
    op.execute(sa.text(
        "CREATE TRIGGER leads_search_tsv_update BEFORE INSERT OR UPDATE ON leads "
        "FOR EACH ROW EXECUTE FUNCTION leads_search_tsv_trigger()"
    ))
    op.execute(sa.text(
        "UPDATE leads SET search_tsv = to_tsvector('english', "
        "coalesce(company, '') || ' ' || coalesce(city, '') || ' ' || "
        "coalesce(specialization, '') || ' ' || coalesce(notes, '') || ' ' || "
        "coalesce(description, '')) WHERE search_tsv IS NULL"
    ))
    op.execute(sa.text(
        "CREATE INDEX IF NOT EXISTS ix_leads_search_tsv ON leads USING gin (search_tsv)"
    ))

    duplicates = bind.execute(sa.text(
        "SELECT fire_key FROM jobs WHERE fire_key IS NOT NULL "
        "AND status IN ('pending','processing') GROUP BY fire_key HAVING count(*) > 1"
    )).scalars().all()
    if duplicates:
        raise RuntimeError(
            "job single-flight reconciliation aborted; duplicate active fire_key(s): "
            + ", ".join(str(value) for value in duplicates[:10])
        )
    op.execute(sa.text(
        "CREATE UNIQUE INDEX IF NOT EXISTS uq_jobs_fire_key_active ON jobs (fire_key) "
        "WHERE fire_key IS NOT NULL AND status IN ('pending','processing')"
    ))


def downgrade() -> None:
    # Security reconciliation is deliberately non-destructive on downgrade:
    # earlier revisions still require these grants and policies. Only the two
    # billing tables first protected here are returned to their prior posture.
    bind = op.get_bind()
    if bind.dialect.name != "postgresql":
        return
    for table in ("workspace_credits", "credit_ledger_entries"):
        quoted = bind.dialect.identifier_preparer.quote(table)
        op.execute(sa.text(f"DROP POLICY IF EXISTS workspace_isolation ON {quoted}"))
        op.execute(sa.text(f"ALTER TABLE {quoted} NO FORCE ROW LEVEL SECURITY"))
        op.execute(sa.text(f"ALTER TABLE {quoted} DISABLE ROW LEVEL SECURITY"))
