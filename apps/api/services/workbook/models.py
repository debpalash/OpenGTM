"""
Workbook models — SQLAlchemy ORM for the hybrid workbook engine.

Architecture (Hybrid Model):
  - Leads DB (SQLite) = the single source of truth
  - Workbook = a "smart playlist" / filtered view on leads
  - WorkbookEnrichment = overlay table for AI/computed columns that don't map to Lead fields

Key principles:
  1. Workbook rows ARE leads — they come from the leads table via filter_criteria
  2. Enrichment of known fields (email, phone, etc.) writes BACK to the Lead record
  3. AI columns / custom columns store results in WorkbookEnrichment
  4. Multiple workbooks can reference the same leads
  5. CSV import creates new leads in the DB and tags them for the workbook
"""

from sqlalchemy import (
    Column, String, Integer, Text, DateTime, ForeignKey, JSON, Float, Boolean
)
from sqlalchemy.orm import relationship
from sqlalchemy.sql import func
from apps.api.database import Base
import uuid


def generate_uuid():
    return str(uuid.uuid4())


# ── Column Type Registry ──────────────────────────────────────────────────

COLUMN_TYPES = {
    "lead_field": {
        "description": "Mapped to a Lead field (editable, writes back to Lead)",
        "icon": "Type",
        "editable": True,
        "has_config": False,
    },
    "enrichment": {
        "description": "Single provider enrichment",
        "icon": "Sparkles",
        "editable": False,
        "has_config": True,
    },
    "waterfall": {
        "description": "Chain of providers with fallback",
        "icon": "Layers",
        "editable": False,
        "has_config": True,
    },
    "ai_formula": {
        "description": "LLM-powered transformation",
        "icon": "Brain",
        "editable": False,
        "has_config": True,
    },
    "conditional": {
        "description": "Only runs if condition is met",
        "icon": "GitBranch",
        "editable": False,
        "has_config": True,
    },
    "output": {
        "description": "Push to CRM, sequencer, webhook",
        "icon": "Send",
        "editable": False,
        "has_config": True,
    },
}

# ── Constants ─────────────────────────────────────────────────────────────

CELL_STATUSES = ["pending", "running", "complete", "error", "skipped"]
WORKBOOK_STATUSES = ["draft", "running", "paused", "complete"]

# Lead fields that can be mapped to workbook columns
LEAD_FIELD_MAP = {
    "company": "company",
    "website": "website",
    "email": "email",
    "phone": "phone",
    "contact_person": "contact_person",
    "contact_title": "contact_title",
    "city": "city",
    "state": "state",
    "address": "address",
    "specialization": "specialization",
    "company_size": "company_size",
    "description": "description",
    "linkedin_url": "linkedin_url",
    "twitter_url": "twitter_url",
    "facebook_url": "facebook_url",
    "score": "score",
    "score_tier": "score_tier",
    "status": "status",
    "source": "source",
    "notes": "notes",
    "email_confidence": "email_confidence",
    "industry_tags": "industry_tags",
    "hiring_signals": "hiring_signals",
    "decision_makers": "decision_makers",
}


# ── Workbook Model ────────────────────────────────────────────────────────

class Workbook(Base):
    """A filtered workspace on the Leads DB — a 'smart playlist' with enrichment columns."""
    __tablename__ = "workbooks"

    id = Column(String, primary_key=True, default=generate_uuid)
    name = Column(String(255), nullable=False)
    description = Column(Text, default="")
    status = Column(String(50), default="draft")  # draft, running, paused, complete

    # Filter criteria — which leads appear in this workbook
    # e.g. {"city": "Pune", "score_tier": "hot", "specialization": "IT Staffing"}
    # Empty = all leads
    filter_criteria = Column(JSON, default=dict)

    # Column definitions — ordered list of column configs
    # Each entry: {id, name, type, lead_field, width, provider, waterfall, prompt, condition, ...}
    # type="lead_field" columns map to a Lead field and are editable
    # type="ai_formula" columns store results in WorkbookEnrichment
    columns_config = Column(JSON, default=list)

    # Stats (computed on read, cached)
    total_rows = Column(Integer, default=0)
    completed_rows = Column(Integer, default=0)

    # Timestamps
    created_at = Column(DateTime, server_default=func.now())
    updated_at = Column(DateTime, server_default=func.now(), onupdate=func.now())
    last_run_at = Column(DateTime, nullable=True)

    # Relationships
    enrichments = relationship(
        "WorkbookEnrichment",
        back_populates="workbook",
        cascade="all, delete-orphan",
        lazy="dynamic",
    )

    def __repr__(self):
        return f"<Workbook {self.name} (filter={self.filter_criteria})>"


# ── WorkbookEnrichment Model ──────────────────────────────────────────────

class WorkbookEnrichment(Base):
    """Overlay data for AI/computed columns that don't map to Lead fields.

    This stores per-lead, per-column enrichment results.
    Known Lead fields (email, phone, etc.) write directly to the Lead record.
    AI columns, custom formulas, etc. store results here.
    """
    __tablename__ = "workbook_enrichments"

    id = Column(Integer, primary_key=True, autoincrement=True)
    workbook_id = Column(String, ForeignKey("workbooks.id", ondelete="CASCADE"), nullable=False, index=True)
    lead_id = Column(Integer, nullable=False, index=True)  # References leads.id in SQLite
    column_id = Column(String(100), nullable=False)  # Which column this enrichment is for

    # Result data
    value = Column(Text, nullable=True)  # The computed/enriched value
    status = Column(String(50), default="pending")  # pending, running, complete, error, skipped
    provider = Column(String(100), nullable=True)  # Which provider/model produced this
    error = Column(Text, nullable=True)  # Error message if status=error

    # Timestamps
    created_at = Column(DateTime, server_default=func.now())
    updated_at = Column(DateTime, server_default=func.now(), onupdate=func.now())

    # Relationships
    workbook = relationship("Workbook", back_populates="enrichments")

    def __repr__(self):
        return f"<WorkbookEnrichment wb={self.workbook_id} lead={self.lead_id} col={self.column_id}>"
