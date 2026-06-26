"""
Workbook Pydantic schemas — request/response validation for Clay-style workbook API.

v2: Workbooks are self-contained tables with their own rows (WorkbookRow),
not live views on the leads database.
"""

from pydantic import BaseModel, Field
from typing import Optional, Any, Literal
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

    # AI formula / research config
    prompt: Optional[str] = Field(None, description="LLM prompt template for ai_formula / research columns")
    input_columns: Optional[list[str]] = Field(None, description="Column IDs to use as input for the prompt")
    max_steps: Optional[int] = Field(None, description="Research column: max search/fetch steps (1-6)")
    output_format: Optional[str] = Field(None, description="Research/AI column output: 'text' or 'json'")

    # Conditional config
    condition: Optional[str] = Field(None, description="Expression to evaluate (e.g. '{email} == \"\"')")

    # Output config
    destination: Optional[str] = Field(None, description="Output destination type: webhook, crm, sequencer")
    destination_config: Optional[dict] = Field(None, description="Config for the output destination")

    # HTTP action column config (type='http')
    http_url: Optional[str] = Field(None, description="URL template with {column} placeholders")
    http_method: Optional[str] = Field(None, description="GET | POST | PUT | PATCH (default GET)")
    http_headers: Optional[dict] = Field(None, description="Request headers (values are {column} templates)")
    http_body: Optional[Any] = Field(None, description="Request body template (dict/str) for POST/PUT")
    http_extract: Optional[str] = Field(None, description="JSONPath-lite expr for the cell value, e.g. $.data.email")

    # Formula action column config (type='formula')
    formula: Optional[str] = Field(None, description="Safe expression over {column} refs, e.g. {Email}.split(\"@\")[1]")


# ── Filter Criteria ───────────────────────────────────────────────────────

class FilterCriteria(BaseModel):
    """Filter to select which leads to snapshot into a workbook."""
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
    """Create a new workbook — Clay-style with source selection."""
    name: str = Field(..., min_length=1, max_length=255)
    description: str = Field("", max_length=2000)
    source: str = Field("empty", description="Source type: empty, csv, leads_filter, job_results")
    source_config: Optional[dict] = Field(None, description="Source config: filter criteria, job IDs, CSV rows, etc.")
    columns_config: list[ColumnConfig] = Field(default_factory=list)
    # Legacy compat
    filter_criteria: Optional[FilterCriteria] = None
    # Row limit for leads_filter source
    max_rows: int = Field(1000, description="Max rows to snapshot from source", ge=1, le=50000)


class WorkbookUpdate(BaseModel):
    """Update workbook metadata."""
    name: Optional[str] = Field(None, min_length=1, max_length=255)
    description: Optional[str] = Field(None, max_length=2000)
    filter_criteria: Optional[FilterCriteria] = None
    columns_config: Optional[list[ColumnConfig]] = None
    status: Optional[str] = None
    sync_to_leads: Optional[bool] = None


class WorkbookResponse(BaseModel):
    """Workbook response — returned from API."""
    id: str
    name: str
    description: str
    status: str
    source_type: str = "empty"
    source_config: Optional[dict] = None
    filter_criteria: Optional[dict] = None
    columns_config: list[dict]
    total_rows: int
    completed_rows: int
    sync_to_leads: bool = True
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None
    last_run_at: Optional[datetime] = None

    model_config = {"from_attributes": True}


class WorkbookListResponse(BaseModel):
    """List of workbooks."""
    workbooks: list[WorkbookResponse]
    total: int


# ── Row schemas (v2 — self-contained rows) ────────────────────────────────

class Provenance(BaseModel):
    """Per-fact provenance for an enriched cell value (license/freshness).

    Recorded only when PROVENANCE_TRACKING_ENABLED; absent on legacy/un-enriched
    cells (backward-compatible). ``license`` is a token from licenses.LICENSE_VOCAB.
    """
    source: str
    license: str = "unknown"
    confidence: Optional[float] = None
    fetched_at: Optional[str] = None


class EnrichmentOverlay(BaseModel):
    """Enrichment data for a single cell (AI/computed columns)."""
    value: Any = None
    status: str = "pending"
    provider: Optional[str] = None
    error: Optional[str] = None
    # 4-status email verification: "valid" | "invalid" | "catch_all" | "unknown"
    verify_status: Optional[str] = None
    # Per-fact provenance (optional; omitted on legacy/un-enriched cells).
    provenance: Optional[Provenance] = None


class WorkbookLeadRow(BaseModel):
    """A row as it appears in a workbook — lead data + enrichment overlay.

    Compatible with both v1 (leads DB) and v2 (WorkbookRow).
    """
    lead_id: int
    row_id: Optional[int] = None  # WorkbookRow.id (v2)
    position: Optional[int] = None
    lead: dict = {}  # Legacy — full lead data
    data: dict = {}  # v2 — self-contained row data (same shape as lead)
    enrichments: dict[str, EnrichmentOverlay] = {}  # {column_id: enrichment_data}
    # Pillar 1: canonical entity binding + cross-source trust signal
    canonical_entity_id: Optional[str] = None
    corroboration_count: Optional[int] = None

    # Convenience: merge lead and data so columns can read from either
    @property
    def merged_data(self) -> dict:
        return {**(self.lead or {}), **(self.data or {})}

    # Flatten lead data fields into top-level for backward compat
    def __getattr__(self, name: str):
        if name in (self.data or {}):
            return self.data[name]
        if name in (self.lead or {}):
            return self.lead[name]
        raise AttributeError(f"'{type(self).__name__}' has no attribute '{name}'")


class WorkbookWithLeadsResponse(BaseModel):
    """Full workbook with paginated rows."""
    workbook: WorkbookResponse
    rows: list[WorkbookLeadRow]
    total_rows: int
    page: int
    page_size: int


# ── Run / Execution Schemas ───────────────────────────────────────────────

class RunWorkbookRequest(BaseModel):
    """Request to run enrichment on a workbook."""
    column_ids: Optional[list[str]] = Field(None, description="Specific columns to run. None = all enrichment columns")
    row_ids: Optional[list[int]] = Field(None, description="Specific row IDs to run. None = all rows")
    # Legacy compat
    lead_ids: Optional[list[int]] = Field(None, description="Legacy: specific leads to run")
    fill_missing: bool = Field(False, description="Only enrich cells not already complete — fills gaps, preserves good values")


class RunWorkbookResponse(BaseModel):
    """Response after starting a workbook run."""
    status: str = "started"
    total_jobs: int = 0
    message: str = ""


# ── Column Management ────────────────────────────────────────────────────

class AddColumnRequest(BaseModel):
    """Add a new column to a workbook."""
    column: ColumnConfig


# ── Row Management ───────────────────────────────────────────────────────

class AddRowsRequest(BaseModel):
    """Add rows to a workbook."""
    rows: list[dict] = Field(..., min_length=1, description="List of row data dicts")
    dedupe: bool = Field(True, description="Skip rows whose identity (domain/company) already exists or repeats")


class DeleteRowsRequest(BaseModel):
    """Delete rows from a workbook."""
    row_ids: list[int] = Field(..., min_length=1, description="Row IDs to delete")


# ── Export ────────────────────────────────────────────────────────────────

class ExportRequest(BaseModel):
    """Export workbook data."""
    format: str = Field("csv", description="Export format: csv, json")
    column_ids: Optional[list[str]] = Field(None, description="Columns to include. None = all")
