"""
Workbook Templates — Pre-built workbook configurations for common use cases.

Each template defines columns_config, filter_criteria, and metadata.
Users can create workbooks from templates in one click.
"""

from typing import List, Dict, Any


TEMPLATE_CATEGORIES = {
    "sales": {"label": "Sales Prospecting", "icon": "target", "color": "text-blue-400"},
    "recruiting": {"label": "Recruiting", "icon": "users", "color": "text-purple-400"},
    "research": {"label": "Market Research", "icon": "search", "color": "text-amber-400"},
    "agency": {"label": "Agency", "icon": "building-2", "color": "text-emerald-400"},
    "signals": {"label": "Signals & Alerts", "icon": "activity", "color": "text-rose-400"},
}


TEMPLATES: List[Dict[str, Any]] = [
    # ── Sales Prospecting ─────────────────────────────────────
    {
        "id": "saas-ctos",
        "name": "SaaS CTOs & Tech Leaders",
        "description": "Find CTOs and VP Engineering at SaaS companies. Includes email, LinkedIn, and hiring signals.",
        "category": "sales",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "contact_person", "name": "Contact", "type": "text"},
            {"key": "contact_title", "name": "Title", "type": "text"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "linkedin_url", "name": "LinkedIn", "type": "url"},
            {"key": "city", "name": "City", "type": "text"},
            {"key": "company_size", "name": "Size", "type": "text"},
            {"key": "score", "name": "Score", "type": "number"},
        ],
        "filter": {"specialization": "SaaS"},
    },
    {
        "id": "hot-leads-outreach",
        "name": "Hot Leads — Ready for Outreach",
        "description": "High-scoring leads with verified emails. Export-ready for email sequences.",
        "category": "sales",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "contact_person", "name": "Contact", "type": "text"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "phone", "name": "Phone", "type": "phone"},
            {"key": "score", "name": "Score", "type": "number"},
            {"key": "score_tier", "name": "Tier", "type": "text"},
            {"key": "city", "name": "City", "type": "text"},
        ],
        "filter": {"score_tier": "hot"},
    },
    {
        "id": "it-staffing-pune",
        "name": "IT Staffing Companies — Pune",
        "description": "IT staffing and consulting firms in Pune with decision maker contacts.",
        "category": "sales",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "phone", "name": "Phone", "type": "phone"},
            {"key": "contact_person", "name": "Contact", "type": "text"},
            {"key": "company_size", "name": "Size", "type": "text"},
            {"key": "specialization", "name": "Industry", "type": "text"},
            {"key": "score", "name": "Score", "type": "number"},
        ],
        "filter": {"city": "Pune", "specialization": "IT"},
    },
    {
        "id": "ecommerce-shopify",
        "name": "E-commerce Stores",
        "description": "E-commerce businesses with website, email, and growth indicators.",
        "category": "sales",
        "columns": [
            {"key": "company", "name": "Store", "type": "text"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "city", "name": "City", "type": "text"},
            {"key": "company_size", "name": "Size", "type": "text"},
            {"key": "score", "name": "Score", "type": "number"},
        ],
        "filter": {"specialization": "ecommerce"},
    },
    {
        "id": "funded-startups",
        "name": "Funded Startups",
        "description": "Recently funded startups actively hiring and growing.",
        "category": "sales",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "contact_person", "name": "Founder/CEO", "type": "text"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "linkedin_url", "name": "LinkedIn", "type": "url"},
            {"key": "city", "name": "City", "type": "text"},
            {"key": "company_size", "name": "Size", "type": "text"},
            {"key": "founding_year", "name": "Founded", "type": "text"},
            {"key": "funding_stage", "name": "Stage", "type": "text"},
            {"key": "last_funding_amount", "name": "Funding", "type": "text"},
            {"key": "investors", "name": "Investors", "type": "text"},
            {"key": "hiring_signals", "name": "Hiring", "type": "text"},
            {"key": "score", "name": "Score", "type": "number"},
        ],
        "filter": {},
    },

    # ── Recruiting ────────────────────────────────────────────
    {
        "id": "hiring-companies",
        "name": "Companies Actively Hiring",
        "description": "Companies with open positions — great for recruiting outreach.",
        "category": "recruiting",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "contact_person", "name": "HR Contact", "type": "text"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "phone", "name": "Phone", "type": "phone"},
            {"key": "company_size", "name": "Size", "type": "text"},
            {"key": "hiring_signals", "name": "Hiring Signals", "type": "text"},
            {"key": "city", "name": "City", "type": "text"},
        ],
        "filter": {},
    },
    {
        "id": "staffing-agencies",
        "name": "Staffing Agencies Directory",
        "description": "Staffing and recruitment agencies with contact details.",
        "category": "recruiting",
        "columns": [
            {"key": "company", "name": "Agency", "type": "text"},
            {"key": "specialization", "name": "Specialization", "type": "text"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "phone", "name": "Phone", "type": "phone"},
            {"key": "city", "name": "City", "type": "text"},
            {"key": "company_size", "name": "Size", "type": "text"},
        ],
        "filter": {"specialization": "staffing"},
    },
    {
        "id": "tech-talent-hubs",
        "name": "Tech Companies — Talent Hubs",
        "description": "Top tech companies in major cities for talent mapping.",
        "category": "recruiting",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "city", "name": "City", "type": "text"},
            {"key": "company_size", "name": "Headcount", "type": "text"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "linkedin_url", "name": "LinkedIn", "type": "url"},
            {"key": "specialization", "name": "Tech Stack", "type": "text"},
            {"key": "hiring_signals", "name": "Open Roles", "type": "text"},
        ],
        "filter": {},
    },

    # ── Market Research ───────────────────────────────────────
    {
        "id": "competitor-landscape",
        "name": "Competitor Landscape",
        "description": "Map competitors by size, location, and specialization.",
        "category": "research",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "specialization", "name": "Focus", "type": "text"},
            {"key": "company_size", "name": "Size", "type": "text"},
            {"key": "city", "name": "HQ", "type": "text"},
            {"key": "description", "name": "About", "type": "text"},
            {"key": "score", "name": "Score", "type": "number"},
        ],
        "filter": {},
    },
    {
        "id": "industry-directory",
        "name": "Industry Directory",
        "description": "Comprehensive company directory for a specific industry vertical.",
        "category": "research",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "specialization", "name": "Specialization", "type": "text"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "phone", "name": "Phone", "type": "phone"},
            {"key": "city", "name": "City", "type": "text"},
            {"key": "state", "name": "State", "type": "text"},
            {"key": "company_size", "name": "Size", "type": "text"},
        ],
        "filter": {},
    },
    {
        "id": "city-analysis",
        "name": "City Market Analysis",
        "description": "All companies in a specific city for market sizing.",
        "category": "research",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "specialization", "name": "Industry", "type": "text"},
            {"key": "company_size", "name": "Size", "type": "text"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "score", "name": "Score", "type": "number"},
        ],
        "filter": {},
    },
    {
        "id": "glassdoor-rated",
        "name": "Top-Rated Companies",
        "description": "Companies with high Glassdoor ratings — employer branding research.",
        "category": "research",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "city", "name": "City", "type": "text"},
            {"key": "company_size", "name": "Size", "type": "text"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "specialization", "name": "Industry", "type": "text"},
            {"key": "score", "name": "Score", "type": "number"},
        ],
        "filter": {},
    },

    # ── Agency ────────────────────────────────────────────────
    {
        "id": "client-prospecting",
        "name": "Client Prospecting Pipeline",
        "description": "Track prospecting progress for agency client acquisition.",
        "category": "agency",
        "columns": [
            {"key": "company", "name": "Prospect", "type": "text"},
            {"key": "contact_person", "name": "Decision Maker", "type": "text"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "phone", "name": "Phone", "type": "phone"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "status", "name": "Status", "type": "text"},
            {"key": "score", "name": "Score", "type": "number"},
            {"key": "notes", "name": "Notes", "type": "text"},
        ],
        "filter": {},
    },
    {
        "id": "client-delivery",
        "name": "Client Delivery — Lead List",
        "description": "Clean lead list format for client deliverables.",
        "category": "agency",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "contact_person", "name": "Contact Name", "type": "text"},
            {"key": "contact_title", "name": "Title", "type": "text"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "phone", "name": "Phone", "type": "phone"},
            {"key": "linkedin_url", "name": "LinkedIn", "type": "url"},
            {"key": "city", "name": "City", "type": "text"},
            {"key": "company_size", "name": "Company Size", "type": "text"},
        ],
        "filter": {},
    },
    {
        "id": "data-enrichment",
        "name": "Data Enrichment Queue",
        "description": "Leads missing key data — prioritized for enrichment.",
        "category": "agency",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "phone", "name": "Phone", "type": "phone"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "linkedin_url", "name": "LinkedIn", "type": "url"},
            {"key": "score", "name": "Score", "type": "number"},
        ],
        "filter": {},
    },

    # ── Signals ───────────────────────────────────────────────
    {
        "id": "buying-signals-tracker",
        "name": "Buying Signals Tracker",
        "description": "Track companies showing buying signals — hiring, growth, funding.",
        "category": "signals",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "hiring_signals", "name": "Hiring", "type": "text"},
            {"key": "company_size", "name": "Size", "type": "text"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "contact_person", "name": "Contact", "type": "text"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "score", "name": "Score", "type": "number"},
        ],
        "filter": {"score_tier": "hot"},
    },
    {
        "id": "warm-leads-nurture",
        "name": "Warm Leads — Nurture",
        "description": "Warm leads that need additional touch points before outreach.",
        "category": "signals",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "contact_person", "name": "Contact", "type": "text"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "score", "name": "Score", "type": "number"},
            {"key": "status", "name": "Status", "type": "text"},
            {"key": "notes", "name": "Notes", "type": "text"},
        ],
        "filter": {"score_tier": "warm"},
    },
    {
        "id": "dead-leads-reactivate",
        "name": "Dead Leads — Reactivation",
        "description": "Previously dead leads that may be worth re-engaging.",
        "category": "signals",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "phone", "name": "Phone", "type": "phone"},
            {"key": "status", "name": "Status", "type": "text"},
            {"key": "score", "name": "Score", "type": "number"},
            {"key": "notes", "name": "Notes", "type": "text"},
        ],
        "filter": {"status": "dead"},
    },
    {
        "id": "full-enrichment",
        "name": "Full Enrichment Pipeline",
        "description": "All lead fields — maximum data density view with OSS enrichment.",
        "category": "research",
        "columns": [
            {"key": "company", "name": "Company", "type": "text"},
            {"key": "website", "name": "Website", "type": "url"},
            {"key": "email", "name": "Email", "type": "email"},
            {"key": "phone", "name": "Phone", "type": "phone"},
            {"key": "contact_person", "name": "Contact", "type": "text"},
            {"key": "contact_title", "name": "Title", "type": "text"},
            {"key": "linkedin_url", "name": "LinkedIn", "type": "url"},
            {"key": "city", "name": "City", "type": "text"},
            {"key": "company_size", "name": "Size", "type": "text"},
            {"key": "specialization", "name": "Industry", "type": "text"},
            {"key": "founding_year", "name": "Founded", "type": "text"},
            {"key": "funding_stage", "name": "Funding Stage", "type": "text"},
            {"key": "last_funding_amount", "name": "Funding", "type": "text"},
            {"key": "investors", "name": "Investors", "type": "text"},
            {"key": "technologies", "name": "Tech Stack", "type": "text"},
            {"key": "recent_news", "name": "News", "type": "text"},
            {"key": "hiring_signals", "name": "Hiring", "type": "text"},
            {"key": "score", "name": "Score", "type": "number"},
            {"key": "score_tier", "name": "Tier", "type": "text"},
        ],
        "filter": {},
    },
]


def get_templates(category: str = None) -> List[Dict[str, Any]]:
    """Get templates, optionally filtered by category."""
    if category:
        return [t for t in TEMPLATES if t["category"] == category]
    return TEMPLATES


def get_template(template_id: str) -> Dict[str, Any] | None:
    """Get a single template by ID."""
    for t in TEMPLATES:
        if t["id"] == template_id:
            return t
    return None
