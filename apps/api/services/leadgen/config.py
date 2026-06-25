"""
Yupcha Lead Generation Pipeline — Configuration

Defines Ideal Customer Profile (ICP), scraper settings, and database path.
"""

import os
from pathlib import Path

# ── Paths ──────────────────────────────────────────────────────────────
BASE_DIR = Path(__file__).parent
DATA_DIR = BASE_DIR / "data"
DB_PATH = DATA_DIR / "leads.db"

# ── Ideal Customer Profile ─────────────────────────────────────────────
ICP = {
    "target_industries": [
        "HR Services",
        "IT Staffing",
        "Recruitment",
        "Workforce Solutions",
        "Payroll",
        "Recruitment Process Outsourcing",
        "Contract Staffing",
        "Executive Search",
        "Talent Acquisition",
        "HR Consultancy",
        "Manpower Services",
        "Staffing & Payroll",
        "HR Outsourcing",
    ],
    "target_cities": [
        "Bangalore", "Mumbai", "Delhi", "Hyderabad", "Pune",
        "Chennai", "Noida", "Gurgaon", "Kolkata", "Ahmedabad",
    ],
    "tier1_cities": [
        "Bangalore", "Mumbai", "Delhi", "Hyderabad", "Pune",
    ],
    "preferred_specializations": [
        "IT Staffing",
        "Workforce Management",
        "Recruitment Process Outsourcing",
        "HR SaaS",
        "Staffing & Payroll",
        "Talent Acquisition",
        "Leadership Hiring",
    ],
    "min_company_size": 10,
    "value_proposition": (
        "Yupcha Superadmin Console — Unified multi-database CRM "
        "for candidate tracking, attendance, client management, "
        "and automated reporting"
    ),
}

# ── Scraper Configuration ──────────────────────────────────────────────
SCRAPER_CONFIG = {
    "google_maps_queries": [
        "HR staffing agency",
        "recruitment consultancy",
        "manpower services",
        "IT staffing company",
        "HR outsourcing company",
        "contract staffing agency",
        "talent acquisition firm",
    ],
    "directory_queries": [
        "staffing agencies",
        "HR consultants",
        "recruitment companies",
        "manpower agencies",
    ],
    "batch_size": 5,
    "request_delay": 2.0,       # seconds between requests
    "page_timeout": 15000,      # ms for page loads
    "headless": True,
    "max_results_per_query": 20,
}

# ── Lead Scoring Weights ───────────────────────────────────────────────
SCORING_WEIGHTS = {
    "has_website":           10,
    "has_email":             10,
    "has_phone":             10,
    "has_linkedin":           5,
    "company_size_large":    15,  # > 50 employees
    "specialization_match":  20,  # matches preferred specialization
    "tier1_city":            10,
    "decision_maker_found":  15,
    "multiple_contacts":      5,
}

SCORE_TIERS = {
    "hot":          (75, 100),
    "warm":         (50, 74),
    "cold":         (25, 49),
    "unqualified":  (0, 24),
}

# ── Dashboard ──────────────────────────────────────────────────────────
DASHBOARD_HOST = "127.0.0.1"
DASHBOARD_PORT = 5050

# ── API Keys (optional, loaded from .env) ──────────────────────────────
# All optional/BYOK. When blank, the email-finder waterfall simply skips that
# provider and degrades gracefully (regex/scrape still runs with no keys).
HUNTER_API_KEY = os.getenv("HUNTER_API_KEY", "")
SNOVIO_CLIENT_ID = os.getenv("SNOVIO_CLIENT_ID", "")
SNOVIO_CLIENT_SECRET = os.getenv("SNOVIO_CLIENT_SECRET", "")

# Optional keyed search engines used as a fallback chain behind DuckDuckGo
# (see services/leadgen/search_engines.py). All optional: with none set, web
# search behaves exactly as today (keyless DDG / SearXNG only).
SERPAPI_KEY = os.getenv("SERPAPI_KEY", "")
BING_SEARCH_KEY = os.getenv("BING_SEARCH_KEY", "")
GOOGLE_CSE_ID = os.getenv("GOOGLE_CSE_ID", "")
GOOGLE_CSE_KEY = os.getenv("GOOGLE_CSE_KEY", "")
BRAVE_SEARCH_KEY = os.getenv("BRAVE_SEARCH_KEY", "")
