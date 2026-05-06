"""
Workbook Pydantic schemas — request/response validation for the hybrid Workbook API.

The key difference from v1: rows are leads, not separate entities.
"""

from pydantic import BaseModel, Field
from typing import Optional, Any
from datetime import datetime


# ── Column Config ─────────────────────────────────────────────────────────

class ColumnConfig(BaseModel):
    """Configuration for a single workbook column."""
    id: str = Field(..., description="Unique column identifier")
    name: str = Field(..., description="Display name")
    type: str = Field("lead_field", description="Column type: lead_field, enrichment, waterfall, ai_formula, conditional, output")
    width: int = Field(200, description="Column width in pixels")

    # Lead field mapping (for type="lead_field")
    lead_field: Optional[str] = Field(None, description="Which Lead field this column maps to (e.g. 'email', 'company')")

    # Enrichment config
    provider: Optional[str] = Field(None, description="Provider name for single enrichment columns")
    waterfall: Optional[list[str]] = Field(None, description="Ordered list of provider names for waterfall columns")
    # Which Lead field to write the enrichment result to (e.g. waterfall email → writes to lead.email)
    target_field: Optional[str] = Field(None, description="Lead field to write enrichment results back to")

    # AI formula config
    prompt: Optional[str] = Field(None, description="LLM prompt template for ai_formula columns")
    input_columns: Optional[list[str]] = Field(None, description="Column IDs to use as input for the prompt")

    # Conditional config
    condition: Optional[str] = Field(None, description="Expression to evaluate (e.g. '{email} == \"\"')")

    # Output config
    destination: Optional[str] = Field(None, description="Output destination type: webhook, crm, sequencer")
    destination_config: Optional[dict] = Field(None, description="Config for the output destination")


# ── Filter Criteria ───────────────────────────────────────────────────────

class FilterCriteria(BaseModel):
    """Filter to select which leads appear in this workbook."""
    city: Optional[str] = None
    state: Optional[str] = None
    score_tier: Optional[str] = None
    status: Optional[str] = None
    source: Optional[str] = None
    job_ids: Optional[list[str]] = Field(None, description="Filter leads by job IDs (source = 'job:xxx')")
    specialization: Optional[str] = None
    company_size: Optional[str] = None
    has_email: Optional[bool] = None      # True = email != "", False = email == ""
    has_phone: Optional[bool] = None
    has_website: Optional[bool] = None
    min_score: Optional[int] = None
    max_score: Optional[int] = None
    search: Optional[str] = None          # Full-text search across company, city, specialization


# ── Workbook Schemas ──────────────────────────────────────────────────────

class WorkbookCreate(BaseModel):
    """Create a new workbook."""
    name: str = Field(..., min_length=1, max_length=255)
    description: str = Field("", max_length=2000)
    filter_criteria: Optional[FilterCriteria] = None
    columns_config: list[ColumnConfig] = Field(default_factory=list)


class WorkbookUpdate(BaseModel):
    """Update workbook metadata."""
    name: Optional[str] = Field(None, min_length=1, max_length=255)
    description: Optional[str] = Field(None, max_length=2000)
    filter_criteria: Optional[FilterCriteria] = None
    columns_config: Optional[list[ColumnConfig]] = None
    status: Optional[str] = None


class WorkbookResponse(BaseModel):
    """Workbook response — returned from API."""
    id: str
    name: str
    description: str
    status: str
    filter_criteria: Optional[dict] = None
    columns_config: list[dict]
    total_rows: int
    completed_rows: int
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None
    last_run_at: Optional[datetime] = None

    model_config = {"from_attributes": True}


class WorkbookListResponse(BaseModel):
    """List of workbooks."""
    workbooks: list[WorkbookResponse]
    total: int


# ── Lead Row (how leads appear in workbook context) ───────────────────────

class EnrichmentOverlay(BaseModel):
    """Enrichment data for a single cell (AI/computed columns)."""
    value: Any = None
    status: str = "pending"
    provider: Optional[str] = None
    error: Optional[str] = None


class WorkbookLeadRow(BaseModel):
    """A lead row as it appears in a workbook — lead data + enrichment overlay."""
    lead_id: int
    lead: dict  # Full lead data
    enrichments: dict[str, EnrichmentOverlay] = {}  # {column_id: enrichment_data}


class WorkbookWithLeadsResponse(BaseModel):
    """Full workbook with paginated lead rows."""
    workbook: WorkbookResponse
    rows: list[WorkbookLeadRow]
    total_rows: int
    page: int
    page_size: int


# ── Run / Execution Schemas ───────────────────────────────────────────────

class RunWorkbookRequest(BaseModel):
    """Request to run enrichment on a workbook."""
    column_ids: Optional[list[str]] = Field(None, description="Specific columns to run. None = all enrichment columns")
    lead_ids: Optional[list[int]] = Field(None, description="Specific leads to run. None = all matching leads")


class RunWorkbookResponse(BaseModel):
    """Response after starting a workbook run."""
    status: str = "started"
    total_jobs: int = 0
    message: str = ""


# ── Column Management ────────────────────────────────────────────────────

class AddColumnRequest(BaseModel):
    """Add a new column to a workbook."""
    column: ColumnConfig


# ── Export ────────────────────────────────────────────────────────────────

class ExportRequest(BaseModel):
    """Export workbook data."""
    format: str = Field("csv", description="Export format: csv, json")
    column_ids: Optional[list[str]] = Field(None, description="Columns to include. None = all")
