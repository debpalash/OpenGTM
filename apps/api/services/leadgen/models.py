"""
Lead data model — dataclass representing a lead in the pipeline.
"""

from dataclasses import dataclass, field, asdict
from datetime import datetime, timezone
from typing import Optional


@dataclass
class Lead:
    """A single lead (company) in the pipeline."""

    # ── Identity ───────────────────────────────────────────────────────
    id: Optional[int] = None
    company: str = ""
    website: str = ""

    # ── Contact ────────────────────────────────────────────────────────
    email: str = ""
    email_confidence: str = ""     # "smtp_verified", "verified", "pattern", "guessed", "generic"
    email_provider: str = ""       # Which provider found the email (waterfall provenance)
    phone: str = ""
    phone_provider: str = ""       # Which provider found the phone
    contact_person: str = ""       # Decision-maker name
    contact_title: str = ""        # e.g. "CEO", "HR Director"

    # ── Location ───────────────────────────────────────────────────────
    city: str = ""
    state: str = ""
    address: str = ""              # Full registered address

    # ── Company Info ───────────────────────────────────────────────────
    specialization: str = ""       # e.g. "IT Staffing", "Executive Search"
    company_size: str = ""         # "1-50", "51-200", "201-500", "500+"
    employee_count_exact: int = 0  # Numeric headcount if known
    description: str = ""          # Short company description
    revenue_range: str = ""        # e.g. "₹1-5 Cr", "$1M-5M"
    founded_year: str = ""         # Year the company was established
    industry_tags: str = ""        # Comma-separated: "IT Staffing, RPO, Payroll"
    technologies: str = ""         # Key tech stack / platforms (flat comma string, back-compat)
    technographics: str = ""       # JSON: [{"name","category","source","confidence"}] (website-detected tech)
    funding_stage: str = ""        # "Bootstrapped", "Seed", "Series A", etc.
    company_size_basis: str = ""   # Provenance of company_size: "exact" | "estimated:<signals>" | ""

    # ── Social ─────────────────────────────────────────────────────────
    linkedin_url: str = ""
    twitter_url: str = ""
    facebook_url: str = ""

    # ── Extended Contact ───────────────────────────────────────────────
    secondary_emails: str = ""     # Pipe-separated additional emails
    secondary_phones: str = ""     # Pipe-separated additional phones
    decision_makers: str = ""      # JSON: [{"name":"X","title":"CEO","linkedin":"..."}]
    glassdoor_rating: str = ""     # Company rating from review sites
    hiring_signals: str = ""       # JSON: {total_jobs, growth_signal, gtm_expansion, ...}

    # ── Enrichment Provenance ──────────────────────────────────────────
    enrichment_attempts: int = 0   # Total provider calls during waterfall
    enrichment_waterfall: str = "" # JSON log of provider chain results
    # Per-fact provenance: JSON object {field_name: {source, license,
    # confidence, fetched_at}} populated by the enrichment waterfall / write-back
    # when PROVENANCE_TRACKING_ENABLED. "" / NULL on legacy rows (no backfill).
    field_provenance: str = ""

    # ── Pipeline ───────────────────────────────────────────────────────
    source: str = ""               # "csv_import", "google_maps", "linkedin", etc.
    source_url: str = ""           # Exact page/profile where the candidate was discovered
    collection_job_id: str = ""    # Owning collection job; separate from source provenance
    workspace_id: str = ""         # Workspace campaign this lead belongs to
    score: int = 0                 # 0-100 quality score
    score_tier: str = "unqualified"  # "hot", "warm", "cold", "unqualified"
    status: str = "new"            # "new", "contacted", "qualified", "converted", "dead"

    # ── Yupcha Sales ───────────────────────────────────────────────────
    yupcha_value_prop: str = ""
    company_need: str = ""
    notes: str = ""

    # ── Timestamps ─────────────────────────────────────────────────────
    created_at: str = ""
    updated_at: str = ""
    last_enriched_at: str = ""

    def __post_init__(self):
        now = datetime.now(timezone.utc).isoformat()
        if not self.created_at:
            self.created_at = now
        if not self.updated_at:
            self.updated_at = now

    def to_dict(self) -> dict:
        """Convert to dictionary for database insertion."""
        return asdict(self)

    @classmethod
    def from_dict(cls, data: dict) -> "Lead":
        """Create a Lead from a dictionary, ignoring unknown keys."""
        valid_fields = {f.name for f in cls.__dataclass_fields__.values()}
        filtered = {k: v for k, v in data.items() if k in valid_fields}
        return cls(**filtered)

    @property
    def has_website(self) -> bool:
        return bool(self.website) and self.website not in ("N/A", "nan", "")

    @property
    def has_email(self) -> bool:
        return bool(self.email) and self.email not in ("N/A", "nan", "") and "@" in self.email

    @property
    def has_phone(self) -> bool:
        return bool(self.phone) and self.phone not in ("N/A", "nan", "")

    @property
    def has_linkedin(self) -> bool:
        return bool(self.linkedin_url) and self.linkedin_url not in ("N/A", "nan", "")

    @property
    def has_contact_person(self) -> bool:
        return bool(self.contact_person) and self.contact_person not in ("N/A", "nan", "")

    def __str__(self) -> str:
        return f"Lead({self.company}, {self.city}, score={self.score}, status={self.status})"


# ── Status constants ───────────────────────────────────────────────────
LEAD_STATUSES = ["new", "contacted", "qualified", "negotiating", "converted", "dead"]
LEAD_SOURCES = ["csv_import", "google_maps", "web_directory", "linkedin", "manual", "referral", "br_cnpj", "br_github"]
EMAIL_CONFIDENCE_LEVELS = ["smtp_verified", "verified", "pattern", "guessed", "generic"]
