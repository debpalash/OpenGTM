"""
Source Registry — Scalable DDG-based source channel definitions.

Instead of writing individual strategy methods for each directory/source,
this registry defines sources as configuration. Each source specifies:
  - DDG search query templates
  - Domain patterns to scrape
  - Extraction rules for company data

Adding a new source = adding a dict entry. Zero code changes needed.

Usage in job_runner:
    from apps.api.services.leadgen.source_registry import get_all_sources, search_source
    sources = get_all_sources(region="india")
    for source in sources:
        leads = await search_source(source, query, city)
"""

import asyncio
import re
import logging
from typing import Dict, List, Optional
from urllib.parse import urlparse

from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.sources")


# ── Source Definition Schema ─────────────────────────────────────────────
# Each source is a dict with:
#   name: str               — unique identifier
#   label: str              — display name
#   region: list[str]       — target regions ("global", "india", "us", "eu")
#   category: str           — "directory", "b2b_marketplace", "review", "jobs", "social", "government", "startup"
#   query_templates: list   — DDG search templates. {q}=query, {city}=city, {industry}=cleaned query
#   site_domain: str|None   — if set, all queries are site-scoped
#   extract_from_listing: bool — if True, scrape the page for company cards
#   priority: int           — execution order (lower = first)
#   enabled: bool           — default enabled state

SOURCES: List[Dict] = [
    # ══════════════════════════════════════════════════════════════════════
    # INDIA — B2B Marketplaces & Directories
    # ══════════════════════════════════════════════════════════════════════
    {
        "name": "indiamart",
        "label": "IndiaMart",
        "region": ["india"],
        "category": "b2b_marketplace",
        "site_domain": "indiamart.com",
        "query_templates": [
            "site:indiamart.com {q}",
            "site:indiamart.com {industry} {city} manufacturers suppliers",
            "site:indiamart.com {industry} companies {city}",
        ],
        "extract_from_listing": False,
        "priority": 10,
        "enabled": True,
    },
    {
        "name": "justdial",
        "label": "JustDial",
        "region": ["india"],
        "category": "directory",
        "site_domain": "justdial.com",
        "query_templates": [
            "site:justdial.com {q}",
            "site:justdial.com {industry} {city}",
        ],
        "extract_from_listing": False,
        "priority": 11,
        "enabled": True,
    },
    {
        "name": "tradeindia",
        "label": "TradeIndia",
        "region": ["india"],
        "category": "b2b_marketplace",
        "site_domain": "tradeindia.com",
        "query_templates": [
            "site:tradeindia.com {industry} {city} manufacturers exporters",
        ],
        "extract_from_listing": False,
        "priority": 12,
        "enabled": True,
    },
    {
        "name": "sulekha",
        "label": "Sulekha",
        "region": ["india"],
        "category": "directory",
        "site_domain": "sulekha.com",
        "query_templates": [
            "site:sulekha.com {industry} {city}",
        ],
        "extract_from_listing": False,
        "priority": 13,
        "enabled": True,
    },
    {
        "name": "exportersindia",
        "label": "ExportersIndia",
        "region": ["india"],
        "category": "b2b_marketplace",
        "site_domain": "exportersindia.com",
        "query_templates": [
            "site:exportersindia.com {industry} {city}",
        ],
        "extract_from_listing": False,
        "priority": 14,
        "enabled": True,
    },

    # ══════════════════════════════════════════════════════════════════════
    # INDIA — Review & Rating Sites
    # ══════════════════════════════════════════════════════════════════════
    {
        "name": "ambitionbox",
        "label": "AmbitionBox",
        "region": ["india"],
        "category": "review",
        "site_domain": "ambitionbox.com",
        "query_templates": [
            "site:ambitionbox.com {industry} companies {city}",
        ],
        "extract_from_listing": False,
        "priority": 20,
        "enabled": True,
    },
    {
        "name": "glassdoor_in",
        "label": "Glassdoor India",
        "region": ["india"],
        "category": "review",
        "site_domain": "glassdoor.co.in",
        "query_templates": [
            "site:glassdoor.co.in {industry} {city} company reviews",
        ],
        "extract_from_listing": False,
        "priority": 21,
        "enabled": True,
    },

    # ══════════════════════════════════════════════════════════════════════
    # INDIA — Job Boards (hiring = buying signal)
    # ══════════════════════════════════════════════════════════════════════
    {
        "name": "naukri",
        "label": "Naukri",
        "region": ["india"],
        "category": "jobs",
        "site_domain": "naukri.com",
        "query_templates": [
            "site:naukri.com {industry} companies {city} jobs",
        ],
        "extract_from_listing": False,
        "priority": 30,
        "enabled": True,
    },

    # ══════════════════════════════════════════════════════════════════════
    # INDIA — Government & Registration
    # ══════════════════════════════════════════════════════════════════════
    {
        "name": "zaubacorp",
        "label": "Zauba Corp (MCA)",
        "region": ["india"],
        "category": "government",
        "site_domain": "zaubacorp.com",
        "query_templates": [
            "site:zaubacorp.com {industry} {city} active company",
        ],
        "extract_from_listing": False,
        "priority": 40,
        "enabled": True,
    },
    {
        "name": "tofler",
        "label": "Tofler (Company Data)",
        "region": ["india"],
        "category": "government",
        "site_domain": "tofler.in",
        "query_templates": [
            "site:tofler.in {industry} {city}",
        ],
        "extract_from_listing": False,
        "priority": 41,
        "enabled": True,
    },

    # ══════════════════════════════════════════════════════════════════════
    # GLOBAL — IT/Tech Directories
    # ══════════════════════════════════════════════════════════════════════
    {
        "name": "clutch",
        "label": "Clutch",
        "region": ["global", "india", "us", "eu"],
        "category": "directory",
        "site_domain": "clutch.co",
        "query_templates": [
            "site:clutch.co {industry} companies {city}",
            "site:clutch.co {q}",
        ],
        "extract_from_listing": True,
        "priority": 50,
        "enabled": True,
    },
    {
        "name": "goodfirms",
        "label": "GoodFirms",
        "region": ["global", "india", "us"],
        "category": "directory",
        "site_domain": "goodfirms.co",
        "query_templates": [
            "site:goodfirms.co {industry} companies {city}",
            "site:goodfirms.co {q}",
        ],
        "extract_from_listing": True,
        "priority": 51,
        "enabled": True,
    },
    {
        "name": "g2",
        "label": "G2",
        "region": ["global"],
        "category": "review",
        "site_domain": "g2.com",
        "query_templates": [
            "site:g2.com {industry} software companies",
        ],
        "extract_from_listing": True,
        "priority": 52,
        "enabled": True,
    },
    {
        "name": "softwaresuggest",
        "label": "SoftwareSuggest",
        "region": ["global", "india"],
        "category": "directory",
        "site_domain": "softwaresuggest.com",
        "query_templates": [
            "site:softwaresuggest.com {industry} {city}",
        ],
        "extract_from_listing": True,
        "priority": 53,
        "enabled": True,
    },
    {
        "name": "techbehemoths",
        "label": "TechBehemoths",
        "region": ["global", "india", "eu"],
        "category": "directory",
        "site_domain": "techbehemoths.com",
        "query_templates": [
            "site:techbehemoths.com {industry} {city}",
        ],
        "extract_from_listing": False,
        "priority": 54,
        "enabled": True,
    },

    # ══════════════════════════════════════════════════════════════════════
    # GLOBAL — Business Directories
    # ══════════════════════════════════════════════════════════════════════
    {
        "name": "yellowpages",
        "label": "Yellow Pages",
        "region": ["us", "global"],
        "category": "directory",
        "site_domain": "yellowpages.com",
        "query_templates": [
            "site:yellowpages.com {industry} {city}",
        ],
        "extract_from_listing": False,
        "priority": 60,
        "enabled": True,
    },
    {
        "name": "yelp",
        "label": "Yelp",
        "region": ["us", "global"],
        "category": "directory",
        "site_domain": "yelp.com",
        "query_templates": [
            "site:yelp.com {industry} {city}",
        ],
        "extract_from_listing": False,
        "priority": 61,
        "enabled": True,
    },
    {
        "name": "bbb",
        "label": "Better Business Bureau",
        "region": ["us"],
        "category": "directory",
        "site_domain": "bbb.org",
        "query_templates": [
            "site:bbb.org {industry} {city}",
        ],
        "extract_from_listing": False,
        "priority": 62,
        "enabled": True,
    },
    {
        "name": "thomasnet",
        "label": "ThomasNet",
        "region": ["us"],
        "category": "b2b_marketplace",
        "site_domain": "thomasnet.com",
        "query_templates": [
            "site:thomasnet.com {industry} suppliers manufacturers",
        ],
        "extract_from_listing": False,
        "priority": 63,
        "enabled": True,
    },

    # ══════════════════════════════════════════════════════════════════════
    # GLOBAL — Startup & Funding
    # ══════════════════════════════════════════════════════════════════════
    {
        "name": "crunchbase",
        "label": "Crunchbase",
        "region": ["global"],
        "category": "startup",
        "site_domain": "crunchbase.com",
        "query_templates": [
            "site:crunchbase.com/organization {industry} {city}",
            "site:crunchbase.com {q} funding",
        ],
        "extract_from_listing": False,
        "priority": 70,
        "enabled": True,
    },
    {
        "name": "tracxn",
        "label": "Tracxn",
        "region": ["global", "india"],
        "category": "startup",
        "site_domain": "tracxn.com",
        "query_templates": [
            "site:tracxn.com {industry} {city} startups",
        ],
        "extract_from_listing": False,
        "priority": 71,
        "enabled": True,
    },
    {
        "name": "angellist",
        "label": "AngelList / Wellfound",
        "region": ["global"],
        "category": "startup",
        "site_domain": "wellfound.com",
        "query_templates": [
            "site:wellfound.com {industry} {city} startups",
        ],
        "extract_from_listing": False,
        "priority": 72,
        "enabled": True,
    },
    {
        "name": "yourstory",
        "label": "YourStory",
        "region": ["india"],
        "category": "startup",
        "site_domain": "yourstory.com",
        "query_templates": [
            "site:yourstory.com {industry} {city} startup",
        ],
        "extract_from_listing": False,
        "priority": 73,
        "enabled": True,
    },
    {
        "name": "inc42",
        "label": "Inc42",
        "region": ["india"],
        "category": "startup",
        "site_domain": "inc42.com",
        "query_templates": [
            "site:inc42.com {industry} startups {city}",
        ],
        "extract_from_listing": False,
        "priority": 74,
        "enabled": True,
    },

    # ══════════════════════════════════════════════════════════════════════
    # GLOBAL — Professional Networks & Social
    # ══════════════════════════════════════════════════════════════════════
    {
        "name": "linkedin_companies",
        "label": "LinkedIn Companies",
        "region": ["global"],
        "category": "social",
        "site_domain": "linkedin.com/company",
        "query_templates": [
            "site:linkedin.com/company {industry} {city}",
        ],
        "extract_from_listing": False,
        "priority": 80,
        "enabled": True,
    },
    {
        "name": "facebook_pages",
        "label": "Facebook Business Pages",
        "region": ["global", "india"],
        "category": "social",
        "site_domain": None,
        "query_templates": [
            "site:facebook.com {industry} {city} company",
        ],
        "extract_from_listing": False,
        "priority": 81,
        "enabled": True,
    },

    # ══════════════════════════════════════════════════════════════════════
    # GLOBAL — Job Boards (hiring = intent signal)
    # ══════════════════════════════════════════════════════════════════════
    {
        "name": "indeed",
        "label": "Indeed",
        "region": ["global", "india", "us"],
        "category": "jobs",
        "site_domain": None,
        "query_templates": [
            "site:indeed.com {industry} {city} company hiring",
        ],
        "extract_from_listing": False,
        "priority": 90,
        "enabled": True,
    },
    {
        "name": "glassdoor",
        "label": "Glassdoor",
        "region": ["global", "us"],
        "category": "review",
        "site_domain": "glassdoor.com",
        "query_templates": [
            "site:glassdoor.com {industry} companies {city} reviews",
        ],
        "extract_from_listing": False,
        "priority": 91,
        "enabled": True,
    },

    # ══════════════════════════════════════════════════════════════════════
    # EUROPE — Directories
    # ══════════════════════════════════════════════════════════════════════
    {
        "name": "europages",
        "label": "Europages",
        "region": ["eu"],
        "category": "b2b_marketplace",
        "site_domain": "europages.co.uk",
        "query_templates": [
            "site:europages.co.uk {industry}",
        ],
        "extract_from_listing": False,
        "priority": 100,
        "enabled": True,
    },
    {
        "name": "kompass",
        "label": "Kompass",
        "region": ["eu", "global"],
        "category": "b2b_marketplace",
        "site_domain": "kompass.com",
        "query_templates": [
            "site:kompass.com {industry} {city}",
        ],
        "extract_from_listing": False,
        "priority": 101,
        "enabled": True,
    },

    # ══════════════════════════════════════════════════════════════════════
    # GENERIC — Catch-all search patterns (no specific site)
    # ══════════════════════════════════════════════════════════════════════
    {
        "name": "generic_companies_list",
        "label": "Company Lists",
        "region": ["global"],
        "category": "directory",
        "site_domain": None,
        "query_templates": [
            '"{industry}" companies {city} list email contact',
            '"top {industry} companies" {city} 2024 2025',
        ],
        "extract_from_listing": False,
        "priority": 200,
        "enabled": True,
    },
    {
        "name": "generic_association",
        "label": "Industry Associations",
        "region": ["global", "india"],
        "category": "directory",
        "site_domain": None,
        "query_templates": [
            '{industry} association members {city} directory',
            '{industry} chamber of commerce {city} members list',
        ],
        "extract_from_listing": False,
        "priority": 201,
        "enabled": True,
    },
    {
        "name": "generic_awards",
        "label": "Award Winners",
        "region": ["global"],
        "category": "directory",
        "site_domain": None,
        "query_templates": [
            'best {industry} companies {city} awards 2024 2025',
            '"fastest growing" {industry} companies {city}',
        ],
        "extract_from_listing": False,
        "priority": 202,
        "enabled": True,
    },

    # ══════════════════════════════════════════════════════════════════════
    # PRODUCT / SAAS DIRECTORIES (from Clay's provider catalog)
    # ══════════════════════════════════════════════════════════════════════
    {"name": "capterra", "label": "Capterra", "region": ["global"], "category": "saas_directory", "site_domain": "capterra.com", "query_templates": ["site:capterra.com {industry} software"], "extract_from_listing": True, "priority": 110, "enabled": True},
    {"name": "getapp", "label": "GetApp", "region": ["global"], "category": "saas_directory", "site_domain": "getapp.com", "query_templates": ["site:getapp.com {industry} software"], "extract_from_listing": True, "priority": 111, "enabled": True},
    {"name": "producthunt", "label": "Product Hunt", "region": ["global"], "category": "saas_directory", "site_domain": "producthunt.com", "query_templates": ["site:producthunt.com/products {industry}"], "extract_from_listing": False, "priority": 112, "enabled": True},
    {"name": "sourceforge", "label": "SourceForge", "region": ["global"], "category": "saas_directory", "site_domain": "sourceforge.net", "query_templates": ["site:sourceforge.net {industry} software"], "extract_from_listing": False, "priority": 113, "enabled": True},
    {"name": "alternativeto", "label": "AlternativeTo", "region": ["global"], "category": "saas_directory", "site_domain": "alternativeto.net", "query_templates": ["site:alternativeto.net {industry}"], "extract_from_listing": False, "priority": 114, "enabled": True},
    {"name": "saashub", "label": "SaaSHub", "region": ["global"], "category": "saas_directory", "site_domain": "saashub.com", "query_templates": ["site:saashub.com {industry}"], "extract_from_listing": False, "priority": 115, "enabled": True},
    {"name": "stackshare", "label": "StackShare", "region": ["global"], "category": "saas_directory", "site_domain": "stackshare.io", "query_templates": ["site:stackshare.io {industry} companies"], "extract_from_listing": False, "priority": 116, "enabled": True},
    {"name": "appsumo", "label": "AppSumo", "region": ["global"], "category": "saas_directory", "site_domain": "appsumo.com", "query_templates": ["site:appsumo.com {industry}"], "extract_from_listing": False, "priority": 117, "enabled": True},

    # ══════════════════════════════════════════════════════════════════════
    # TRUST / REVIEW PLATFORMS
    # ══════════════════════════════════════════════════════════════════════
    {"name": "trustpilot", "label": "Trustpilot", "region": ["global", "us", "eu"], "category": "review", "site_domain": "trustpilot.com", "query_templates": ["site:trustpilot.com/review {industry} {city}"], "extract_from_listing": False, "priority": 120, "enabled": True},
    {"name": "tripadvisor", "label": "TripAdvisor", "region": ["global"], "category": "review", "site_domain": "tripadvisor.com", "query_templates": ["site:tripadvisor.com {industry} {city}"], "extract_from_listing": False, "priority": 121, "enabled": False},
    {"name": "google_reviews", "label": "Google Reviews", "region": ["global"], "category": "review", "site_domain": None, "query_templates": ['{industry} {city} "google reviews" company'], "extract_from_listing": False, "priority": 122, "enabled": True},
    {"name": "mouthshut", "label": "MouthShut", "region": ["india"], "category": "review", "site_domain": "mouthshut.com", "query_templates": ["site:mouthshut.com {industry} {city}"], "extract_from_listing": False, "priority": 123, "enabled": True},

    # ══════════════════════════════════════════════════════════════════════
    # FREELANCE & SERVICE MARKETPLACES
    # ══════════════════════════════════════════════════════════════════════
    {"name": "upwork", "label": "Upwork", "region": ["global"], "category": "freelance", "site_domain": "upwork.com", "query_templates": ["site:upwork.com/agencies {industry} {city}"], "extract_from_listing": False, "priority": 130, "enabled": True},
    {"name": "fiverr", "label": "Fiverr Business", "region": ["global"], "category": "freelance", "site_domain": "fiverr.com", "query_templates": ["site:fiverr.com {industry} agency {city}"], "extract_from_listing": False, "priority": 131, "enabled": False},
    {"name": "toptal", "label": "Toptal", "region": ["global"], "category": "freelance", "site_domain": "toptal.com", "query_templates": ["site:toptal.com {industry} companies"], "extract_from_listing": False, "priority": 132, "enabled": False},
    {"name": "freelancer", "label": "Freelancer", "region": ["global"], "category": "freelance", "site_domain": "freelancer.com", "query_templates": ["site:freelancer.com {industry} {city}"], "extract_from_listing": False, "priority": 133, "enabled": False},
    {"name": "bark", "label": "Bark", "region": ["global", "us"], "category": "freelance", "site_domain": "bark.com", "query_templates": ["site:bark.com {industry} {city}"], "extract_from_listing": False, "priority": 134, "enabled": True},

    # ══════════════════════════════════════════════════════════════════════
    # DEVELOPER & TECH COMMUNITIES
    # ══════════════════════════════════════════════════════════════════════
    {"name": "github_orgs", "label": "GitHub Organizations", "region": ["global"], "category": "developer", "site_domain": "github.com", "query_templates": ["site:github.com {industry} company organization {city}"], "extract_from_listing": False, "priority": 140, "enabled": True},
    {"name": "stackoverflow_jobs", "label": "StackOverflow Companies", "region": ["global"], "category": "developer", "site_domain": "stackoverflow.com", "query_templates": ["site:stackoverflow.com/jobs/companies {industry}"], "extract_from_listing": False, "priority": 141, "enabled": True},
    {"name": "hackernews", "label": "Hacker News", "region": ["global"], "category": "developer", "site_domain": "news.ycombinator.com", "query_templates": ['site:news.ycombinator.com "Show HN" {industry}'], "extract_from_listing": False, "priority": 142, "enabled": True},
    {"name": "devto", "label": "Dev.to", "region": ["global"], "category": "developer", "site_domain": "dev.to", "query_templates": ["site:dev.to {industry} company {city}"], "extract_from_listing": False, "priority": 143, "enabled": False},

    # ══════════════════════════════════════════════════════════════════════
    # NEWS & MEDIA — Company mentions
    # ══════════════════════════════════════════════════════════════════════
    {"name": "google_news", "label": "Google News", "region": ["global"], "category": "news", "site_domain": None, "query_templates": ['"{industry}" company {city} site:news.google.com'], "extract_from_listing": False, "priority": 150, "enabled": True},
    {"name": "economic_times", "label": "Economic Times", "region": ["india"], "category": "news", "site_domain": "economictimes.com", "query_templates": ["site:economictimes.indiatimes.com {industry} companies {city}"], "extract_from_listing": False, "priority": 151, "enabled": True},
    {"name": "business_standard", "label": "Business Standard", "region": ["india"], "category": "news", "site_domain": "business-standard.com", "query_templates": ["site:business-standard.com {industry} {city} company"], "extract_from_listing": False, "priority": 152, "enabled": True},
    {"name": "livemint", "label": "Livemint", "region": ["india"], "category": "news", "site_domain": "livemint.com", "query_templates": ["site:livemint.com {industry} {city} company startup"], "extract_from_listing": False, "priority": 153, "enabled": True},
    {"name": "techcrunch", "label": "TechCrunch", "region": ["global"], "category": "news", "site_domain": "techcrunch.com", "query_templates": ["site:techcrunch.com {industry} {city} startup funding"], "extract_from_listing": False, "priority": 154, "enabled": True},
    {"name": "forbes", "label": "Forbes", "region": ["global"], "category": "news", "site_domain": "forbes.com", "query_templates": ['site:forbes.com "best {industry} companies" {city}'], "extract_from_listing": False, "priority": 155, "enabled": True},

    # ══════════════════════════════════════════════════════════════════════
    # INDIA — More directories & platforms
    # ══════════════════════════════════════════════════════════════════════
    {"name": "mouthshut_biz", "label": "India Yellow Pages", "region": ["india"], "category": "directory", "site_domain": "yellowpages.in", "query_templates": ["site:yellowpages.in {industry} {city}"], "extract_from_listing": False, "priority": 15, "enabled": True},
    {"name": "grotal", "label": "Grotal", "region": ["india"], "category": "directory", "site_domain": "grotal.com", "query_templates": ["site:grotal.com {industry} {city}"], "extract_from_listing": False, "priority": 16, "enabled": True},
    {"name": "urbanpro", "label": "UrbanPro", "region": ["india"], "category": "directory", "site_domain": "urbanpro.com", "query_templates": ["site:urbanpro.com {industry} {city}"], "extract_from_listing": False, "priority": 17, "enabled": True},
    {"name": "dial4trade", "label": "Dial4Trade", "region": ["india"], "category": "b2b_marketplace", "site_domain": "dial4trade.com", "query_templates": ["site:dial4trade.com {industry} {city}"], "extract_from_listing": False, "priority": 18, "enabled": True},
    {"name": "fundoodata", "label": "FundooData", "region": ["india"], "category": "directory", "site_domain": "fundoodata.com", "query_templates": ["site:fundoodata.com {industry} companies {city}"], "extract_from_listing": False, "priority": 19, "enabled": True},
    {"name": "startup_india", "label": "Startup India", "region": ["india"], "category": "startup", "site_domain": "startupindia.gov.in", "query_templates": ["site:startupindia.gov.in {industry} {city}"], "extract_from_listing": False, "priority": 75, "enabled": True},
    {"name": "nasscom", "label": "NASSCOM", "region": ["india"], "category": "directory", "site_domain": "nasscom.in", "query_templates": ["site:nasscom.in {industry} member companies"], "extract_from_listing": False, "priority": 42, "enabled": True},
    {"name": "shine", "label": "Shine Jobs", "region": ["india"], "category": "jobs", "site_domain": "shine.com", "query_templates": ["site:shine.com {industry} companies {city}"], "extract_from_listing": False, "priority": 31, "enabled": True},
    {"name": "monsterindia", "label": "Monster India", "region": ["india"], "category": "jobs", "site_domain": "monsterindia.com", "query_templates": ["site:monsterindia.com {industry} companies {city}"], "extract_from_listing": False, "priority": 32, "enabled": True},

    # ══════════════════════════════════════════════════════════════════════
    # GLOBAL — More B2B & industry directories
    # ══════════════════════════════════════════════════════════════════════
    {"name": "manta", "label": "Manta", "region": ["us"], "category": "directory", "site_domain": "manta.com", "query_templates": ["site:manta.com {industry} {city}"], "extract_from_listing": False, "priority": 64, "enabled": True},
    {"name": "dnb", "label": "D&B (Dun & Bradstreet)", "region": ["global", "us"], "category": "directory", "site_domain": "dnb.com", "query_templates": ["site:dnb.com {industry} company profile"], "extract_from_listing": False, "priority": 65, "enabled": False},
    {"name": "opencorporates", "label": "OpenCorporates", "region": ["global", "eu"], "category": "government", "site_domain": "opencorporates.com", "query_templates": ["site:opencorporates.com {industry} {city}"], "extract_from_listing": False, "priority": 102, "enabled": True},
    {"name": "companieshouse", "label": "Companies House UK", "region": ["eu"], "category": "government", "site_domain": "companieshouse.gov.uk", "query_templates": ["site:find-and-update.company-information.service.gov.uk {industry}"], "extract_from_listing": False, "priority": 103, "enabled": False},
    {"name": "dealroom", "label": "Dealroom", "region": ["global", "eu"], "category": "startup", "site_domain": "dealroom.co", "query_templates": ["site:dealroom.co {industry} {city} companies"], "extract_from_listing": False, "priority": 76, "enabled": True},
    {"name": "cbinsights", "label": "CB Insights", "region": ["global"], "category": "startup", "site_domain": "cbinsights.com", "query_templates": ["site:cbinsights.com {industry} companies"], "extract_from_listing": False, "priority": 77, "enabled": True},
    {"name": "foursquare", "label": "Foursquare", "region": ["global", "us"], "category": "directory", "site_domain": "foursquare.com", "query_templates": ["site:foursquare.com {industry} {city}"], "extract_from_listing": False, "priority": 66, "enabled": False},
    {"name": "hotfrog", "label": "Hotfrog", "region": ["global"], "category": "directory", "site_domain": "hotfrog.com", "query_templates": ["site:hotfrog.com {industry} {city}"], "extract_from_listing": False, "priority": 67, "enabled": True},
    {"name": "brownbook", "label": "BrownBook", "region": ["global"], "category": "directory", "site_domain": "brownbook.net", "query_templates": ["site:brownbook.net {industry} {city}"], "extract_from_listing": False, "priority": 68, "enabled": True},
    {"name": "cylex", "label": "Cylex", "region": ["global", "eu"], "category": "directory", "site_domain": "cylex.us.com", "query_templates": ["site:cylex.us.com {industry} {city}"], "extract_from_listing": False, "priority": 69, "enabled": True},

    # ══════════════════════════════════════════════════════════════════════
    # SOCIAL MEDIA & COMMUNITY SIGNALS
    # ══════════════════════════════════════════════════════════════════════
    {"name": "twitter_companies", "label": "Twitter/X Companies", "region": ["global"], "category": "social", "site_domain": None, "query_templates": ["site:twitter.com {industry} {city} company official"], "extract_from_listing": False, "priority": 82, "enabled": True},
    {"name": "instagram_business", "label": "Instagram Business", "region": ["global", "india"], "category": "social", "site_domain": None, "query_templates": ["site:instagram.com {industry} {city} company"], "extract_from_listing": False, "priority": 83, "enabled": True},
    {"name": "reddit_companies", "label": "Reddit Mentions", "region": ["global"], "category": "social", "site_domain": "reddit.com", "query_templates": ['site:reddit.com "best {industry} companies" {city}'], "extract_from_listing": False, "priority": 84, "enabled": True},
    {"name": "quora_companies", "label": "Quora Mentions", "region": ["global", "india"], "category": "social", "site_domain": "quora.com", "query_templates": ['site:quora.com "best {industry} companies" {city}'], "extract_from_listing": False, "priority": 85, "enabled": True},

    # ══════════════════════════════════════════════════════════════════════
    # E-COMMERCE & MARKETPLACE SELLERS (buying signals)
    # ══════════════════════════════════════════════════════════════════════
    {"name": "amazon_sellers", "label": "Amazon Sellers", "region": ["global", "india"], "category": "ecommerce", "site_domain": None, "query_templates": ["site:amazon.in {industry} seller {city}", "site:amazon.com {industry} seller brand"], "extract_from_listing": False, "priority": 160, "enabled": True},
    {"name": "flipkart_sellers", "label": "Flipkart Sellers", "region": ["india"], "category": "ecommerce", "site_domain": "flipkart.com", "query_templates": ["site:flipkart.com {industry} seller {city}"], "extract_from_listing": False, "priority": 161, "enabled": True},
    {"name": "shopify_stores", "label": "Shopify Stores", "region": ["global"], "category": "ecommerce", "site_domain": None, "query_templates": ['"powered by shopify" {industry} {city}'], "extract_from_listing": False, "priority": 162, "enabled": True},

    # ══════════════════════════════════════════════════════════════════════
    # GENERIC — More search pattern variations
    # ══════════════════════════════════════════════════════════════════════
    {"name": "generic_expo", "label": "Trade Shows & Expos", "region": ["global", "india"], "category": "directory", "site_domain": None, "query_templates": ["{industry} expo exhibitors {city} 2024 2025 list"], "extract_from_listing": False, "priority": 203, "enabled": True},
    {"name": "generic_incubator", "label": "Incubator/Accelerator", "region": ["global", "india"], "category": "startup", "site_domain": None, "query_templates": ["{industry} startup incubator accelerator {city} portfolio companies"], "extract_from_listing": False, "priority": 204, "enabled": True},
    {"name": "generic_govt_tender", "label": "Government Tenders", "region": ["india"], "category": "government", "site_domain": None, "query_templates": ["{industry} government tender {city} vendor supplier list"], "extract_from_listing": False, "priority": 205, "enabled": True},
    {"name": "generic_iso_certified", "label": "ISO Certified Companies", "region": ["global", "india"], "category": "directory", "site_domain": None, "query_templates": ['"{industry}" "ISO certified" companies {city}'], "extract_from_listing": False, "priority": 206, "enabled": True},
    {"name": "generic_hiring_surge", "label": "Hiring Surge Detection", "region": ["global"], "category": "jobs", "site_domain": None, "query_templates": ['"{industry}" company {city} "we are hiring" OR "join our team"'], "extract_from_listing": False, "priority": 207, "enabled": True},
]


# ── Registry API ─────────────────────────────────────────────────────────

def get_all_sources(
    region: Optional[str] = None,
    category: Optional[str] = None,
    enabled_only: bool = True,
) -> List[Dict]:
    """Get sources filtered by region and/or category."""
    results = SOURCES
    if enabled_only:
        results = [s for s in results if s.get("enabled", True)]
    if region:
        results = [s for s in results if region in s.get("region", [])]
    if category:
        results = [s for s in results if s.get("category") == category]
    return sorted(results, key=lambda s: s.get("priority", 999))


def get_source(name: str) -> Optional[Dict]:
    """Get a single source by name."""
    for s in SOURCES:
        if s["name"] == name:
            return s
    return None


def get_source_count(region: Optional[str] = None) -> int:
    """Count enabled sources for a region."""
    return len(get_all_sources(region=region))


def build_queries(source: Dict, query: str, city: str = "") -> List[str]:
    """Build DDG search queries from a source's templates."""
    # Clean the query to extract industry/core term
    industry = query.lower()
    if city:
        industry = industry.replace(city.lower(), "").replace(" in ", " ").strip()

    queries = []
    for template in source.get("query_templates", []):
        q = template.format(
            q=query,
            city=city or "",
            industry=industry,
        ).strip()
        if q:
            queries.append(q)
    return queries


def get_source_summary() -> Dict:
    """Get a summary of all registered sources."""
    by_region: Dict[str, int] = {}
    by_category: Dict[str, int] = {}
    for s in SOURCES:
        if not s.get("enabled", True):
            continue
        for r in s.get("region", []):
            by_region[r] = by_region.get(r, 0) + 1
        cat = s.get("category", "other")
        by_category[cat] = by_category.get(cat, 0) + 1
    return {
        "total": len([s for s in SOURCES if s.get("enabled", True)]),
        "by_region": by_region,
        "by_category": by_category,
        "sources": [
            {"name": s["name"], "label": s["label"], "region": s["region"],
             "category": s["category"], "enabled": s.get("enabled", True)}
            for s in sorted(SOURCES, key=lambda x: x.get("priority", 999))
        ],
    }
