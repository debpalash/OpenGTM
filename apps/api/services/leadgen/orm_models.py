"""ORM models for the shared, multi-tenant Postgres `leads` and `signals`
tables.

These mirror the legacy per-workspace SQLite schemas (services/leadgen/db.py
``leads`` and services/signals/monitor.py ``signals``) but live in ONE shared
Postgres database, scoped by a NOT NULL ``workspace_id`` column. Tenant
isolation is enforced two ways:

  * Application layer — :class:`PgLeadStore` always filters/stamps by
    ``ctx.workspace_id`` (works on the dev superuser connection and on SQLite).
  * Database layer (Postgres only) — Row-Level Security policies on these
    tables (created in the Alembic migration) are the *enforced backstop*: a
    missing ``app.workspace_id`` GUC yields zero rows (fail closed), and a
    cross-tenant write is rejected by the policy WITH CHECK.

The SQLite self-host / test path does NOT use these ORM tables — it keeps the
raw :class:`~apps.api.services.leadgen.db.LeadDB` (FTS5 etc.). These tables are
created on SQLite too (so the ORM/create_all stays consistent and the parity
tests can run), but RLS and the tsvector trigger are Postgres-only.
"""

from datetime import datetime, timezone

from sqlalchemy import (
    Column,
    Integer,
    String,
    Boolean,
    Text,
    Float,
    Index,
    UniqueConstraint,
)

from apps.api.database import Base


def _utcnow_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


class LeadRow(Base):
    """A lead (company) in the shared multi-tenant store.

    Column set mirrors :class:`apps.api.services.leadgen.models.Lead` so the
    dataclass round-trips through this table unchanged.
    """

    __tablename__ = "leads"

    id = Column(Integer, primary_key=True, autoincrement=True)
    # Tenancy key. NOT NULL — every row belongs to exactly one workspace. The
    # composite unique below + RLS both key off this.
    workspace_id = Column(String, nullable=False, index=True)

    # ── Identity ──
    company = Column(String, nullable=False)
    website = Column(String, default="")

    # ── Contact ──
    email = Column(String, default="")
    email_confidence = Column(String, default="")
    email_provider = Column(String, default="")
    phone = Column(String, default="")
    phone_provider = Column(String, default="")
    contact_person = Column(String, default="")
    contact_title = Column(String, default="")

    # ── Location ──
    city = Column(String, default="")
    state = Column(String, default="")
    address = Column(String, default="")

    # ── Company info ──
    specialization = Column(String, default="")
    company_size = Column(String, default="")
    # Provenance of company_size: "exact" | "estimated:<signals>" | "" (heuristic).
    company_size_basis = Column(String, default="")
    employee_count_exact = Column(Integer, default=0)
    description = Column(Text, default="")
    revenue_range = Column(String, default="")
    founded_year = Column(String, default="")
    founding_year = Column(String, default="")
    industry_tags = Column(Text, default="")
    technologies = Column(Text, default="")
    technographics = Column(Text, nullable=True, default="")
    funding_stage = Column(String, default="")

    # ── Social ──
    linkedin_url = Column(String, default="")
    twitter_url = Column(String, default="")
    facebook_url = Column(String, default="")

    # ── Extended contact ──
    secondary_emails = Column(Text, default="")
    secondary_phones = Column(Text, default="")
    decision_makers = Column(Text, default="")
    glassdoor_rating = Column(String, default="")
    hiring_signals = Column(Text, default="")

    # ── Enrichment provenance ──
    enrichment_attempts = Column(Integer, default=0)
    enrichment_waterfall = Column(Text, default="")
    # Per-fact provenance JSON object {field: {source,license,confidence,
    # fetched_at}}. Nullable/inert: only written when PROVENANCE_TRACKING_ENABLED;
    # NULL on legacy rows (no retroactive backfill). Stored as JSON text (matches
    # enrichment_waterfall) so it round-trips through both the PG ORM store and
    # the raw-SQLite LeadDB store without dict-binding/double-encoding surprises.
    field_provenance = Column(Text, nullable=True, default="")
    last_funding_amount = Column(String, default="")
    investors = Column(Text, default="")
    recent_news = Column(Text, default="")
    google_rating = Column(String, default="")
    revenue_estimate = Column(String, default="")
    email_verify = Column(String, default="")
    email_presence = Column(String, default="")

    # ── Pipeline ──
    source = Column(String, default="")
    score = Column(Integer, default=0)
    score_tier = Column(String, default="unqualified")
    status = Column(String, default="new")

    # ── Yupcha sales ──
    yupcha_value_prop = Column(Text, default="")
    company_need = Column(Text, default="")
    notes = Column(Text, default="")

    # ── Timestamps (ISO strings, matching the dataclass) ──
    created_at = Column(String, default=_utcnow_iso)
    updated_at = Column(String, default=_utcnow_iso)
    last_enriched_at = Column(String, default="")

    __table_args__ = (
        # Dedup is now PER TENANT: (workspace_id, company, city). Two tenants
        # may each own (Acme, NYC); a same-tenant duplicate is rejected.
        UniqueConstraint(
            "workspace_id", "company", "city", name="uq_leads_ws_company_city"
        ),
        Index("ix_leads_ws_score", "workspace_id", "score"),
        Index("ix_leads_ws_status", "workspace_id", "status"),
        Index("ix_leads_ws_source", "workspace_id", "source"),
    )


class SignalRow(Base):
    """A buying signal in the shared multi-tenant store.

    Mirrors :class:`apps.api.services.signals.monitor.Signal` plus the new
    NOT NULL ``workspace_id`` (the legacy SQLite ``signals`` table had none).
    """

    __tablename__ = "signals"

    id = Column(String, primary_key=True)
    workspace_id = Column(String, nullable=False, index=True)
    lead_id = Column(Integer, default=0)
    company = Column(String, default="")
    signal_type = Column(String, default="")
    title = Column(String, default="")
    description = Column(Text, default="")
    source = Column(String, default="")
    source_url = Column(String, default="")
    weight = Column(Integer, default=5)
    created_at = Column(Float, default=0.0)
    read = Column(Boolean, default=False)

    __table_args__ = (
        Index("ix_signals_ws_type", "workspace_id", "signal_type"),
        Index("ix_signals_ws_lead", "workspace_id", "lead_id"),
        Index("ix_signals_ws_created", "workspace_id", "created_at"),
    )
