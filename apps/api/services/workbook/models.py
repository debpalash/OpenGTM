"""
Workbook models — SQLAlchemy ORM for Clay-style self-contained tables.

Architecture (v2 — Clay-inspired):
  - Each workbook has its OWN rows (WorkbookRow) — not live views on leads DB
  - Rows are snapshots: data imported from CSV, leads DB, job results, or added manually
  - Enrichment results stored inline in WorkbookRow.enrichments JSON
  - Optional lead_id link for syncing back to leads DB
  - WorkbookEnrichment overlay table kept for backward compat (migrated to inline)
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
    """A self-contained workbook table — Clay-style independent execution environment."""
    __tablename__ = "workbooks"

    id = Column(String, primary_key=True, default=generate_uuid)
    name = Column(String(255), nullable=False)
    description = Column(Text, default="")
    status = Column(String(50), default="draft")  # draft, running, paused, complete

    # Source type — how this workbook was created
    # empty, csv, leads_filter, job_results
    source_type = Column(String(50), default="empty")
    # Source config — filter criteria, job IDs, etc. for refresh
    source_config = Column(JSON, default=dict)

    # Legacy — kept for backward compat, migrated workbooks use source_config
    filter_criteria = Column(JSON, default=dict)

    # Column definitions — ordered list of column configs
    columns_config = Column(JSON, default=list)

    # Stats (computed on read from WorkbookRow count)
    total_rows = Column(Integer, default=0)
    completed_rows = Column(Integer, default=0)

    # Sync enrichments back to leads DB (optional per-workbook toggle)
    sync_to_leads = Column(Boolean, default=True)

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
    rows = relationship(
        "WorkbookRow",
        back_populates="workbook",
        cascade="all, delete-orphan",
        lazy="dynamic",
    )

    def __repr__(self):
        return f"<Workbook {self.name} (source={self.source_type})>"


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


# ── WorkbookRow Model ─────────────────────────────────────────────────────

class WorkbookRow(Base):
    """Self-contained row within a workbook — Clay-style independent data.

    Each workbook has its own rows. Data is snapshotted at import time,
    not a live reference to the leads DB.
    """
    __tablename__ = "workbook_rows"

    id = Column(Integer, primary_key=True, autoincrement=True)
    workbook_id = Column(String, ForeignKey("workbooks.id", ondelete="CASCADE"), nullable=False, index=True)
    position = Column(Integer, default=0)  # Row ordering

    # Denormalized lead data (snapshot at import time)
    # e.g. {"company": "Acme", "website": "acme.com", "email": "", ...}
    data = Column(JSON, default=dict)

    # Enrichment results stored INLINE — no separate overlay table needed
    # e.g. {"find_email": {"status": "complete", "value": "...", "provider": "hunter_io"},
    #        "company_info": {"status": "error", "error": "timeout"}}
    enrichments = Column(JSON, default=dict)

    # Optional link back to leads DB (for CRM sync, dedup)
    lead_id = Column(Integer, nullable=True, index=True)

    # Timestamps
    created_at = Column(DateTime, server_default=func.now())
    updated_at = Column(DateTime, server_default=func.now(), onupdate=func.now())

    # Relationships
    workbook = relationship("Workbook", back_populates="rows")

    def __repr__(self):
        company = (self.data or {}).get("company", "?")
        return f"<WorkbookRow wb={self.workbook_id} #{self.position} '{company}'>"

    def to_api_row(self) -> dict:
        """Convert to API-compatible row format."""
        return {
            "row_id": self.id,
            "lead_id": self.lead_id or self.id,
            "position": self.position,
            "data": self.data or {},
            "enrichments": self.enrichments or {},
            # Flatten lead data fields for column rendering
            **(self.data or {}),
        }
