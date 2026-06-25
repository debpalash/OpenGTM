"""
ATS Hiring-Signal Provider — direct Greenhouse / Lever / Ashby board scraping.

Ported from rahulchhabria/local-enrichment-tool (job-scraper.ts). These ATS
platforms expose PUBLIC, keyless JSON job-board APIs — far more reliable than
scraping aggregator job boards. A non-empty board is a strong, free buying
signal (the company is actively hiring); the open-role count is a cheap
hiring-velocity proxy, and role titles reveal departments + tech intent.

Capabilities: hiring_signals (JSON), open_roles
Free, no API key. See docs/clay-alternatives-ingestion-catalog.md (#9).
"""

import json
import logging
import re
import time
from typing import Dict, List, Optional

import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.enrichment.providers.job_tech_intent import analyze_job_text
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.ats_hiring")

# Public, keyless board APIs. {slug} = company slug.
GREENHOUSE = "https://boards-api.greenhouse.io/v1/boards/{slug}/jobs"
LEVER = "https://api.lever.co/v0/postings/{slug}?mode=json"
ASHBY = "https://api.ashbyhq.com/posting-api/job-board/{slug}"

_DEPT_KEYWORDS = {
    "engineering": ["engineer", "developer", "swe", "devops", "sre", "data ", "ml ", "machine learning", "backend", "frontend", "full stack", "platform"],
    "sales": ["sales", "account executive", "ae ", "sdr", "bdr", "revenue", "account manager"],
    "marketing": ["marketing", "growth", "demand gen", "content", "seo", "brand"],
    "product": ["product manager", "product owner", "pm ", "design", "ux", "ui "],
    "operations": ["operations", "ops ", "supply", "logistics"],
    "people": ["recruiter", "talent", "people ops", "hr ", "human resources"],
    "finance": ["finance", "accounting", "controller", "fp&a"],
}

_TECH_KEYWORDS = [
    "python", "java", "javascript", "typescript", "react", "node", "go ", "golang",
    "rust", "kubernetes", "aws", "gcp", "azure", "terraform", "ruby", "rails",
    "django", "postgres", "kafka", "spark", "snowflake", "databricks",
]


def _slug_candidates(lead: Lead) -> List[str]:
    """Best-effort ATS slugs: domain root and the company name compacted."""
    cands: List[str] = []
    website = (getattr(lead, "website", "") or "").lower()
    if website:
        d = website
        for p in ("https://", "http://", "www."):
            d = d.removeprefix(p)
        root = d.split("/")[0].split("?")[0].split(".")[0]
        if root:
            cands.append(root)
    company = (getattr(lead, "company", "") or "").lower()
    if company:
        compact = re.sub(r"[^a-z0-9]+", "", company)
        if compact:
            cands.append(compact)
        dashed = re.sub(r"[^a-z0-9]+", "-", company).strip("-")
        if dashed and dashed != compact:
            cands.append(dashed)
    # de-dupe preserving order
    seen, out = set(), []
    for c in cands:
        if c and c not in seen:
            seen.add(c); out.append(c)
    return out


def _titles_to_signals(titles: List[str]) -> Dict:
    depts: Dict[str, int] = {}
    tech: set = set()
    for t in titles:
        tl = t.lower()
        for dept, kws in _DEPT_KEYWORDS.items():
            if any(k in tl for k in kws):
                depts[dept] = depts.get(dept, 0) + 1
        for tk in _TECH_KEYWORDS:
            if tk in tl:
                tech.add(tk.strip())
    # Tech-adoption intent from the role titles we already fetched (no new
    # network). Titles carry less context than full descriptions, so most
    # detections land as intent "using"/"expanding".
    intent = analyze_job_text(" \n ".join(titles), open_roles=len(titles))

    return {
        "open_roles": len(titles),
        "departments": depts,
        "tech_hiring": sorted(tech),
        "is_hiring": len(titles) > 0,
        "eng_hiring": depts.get("engineering", 0) > 0,
        "sample_titles": titles[:8],
        "technologies": intent["technologies"],
        "tech_adoption_signal": intent["tech_adoption_signal"],
        "hiring_velocity": intent["hiring_velocity"],
    }


async def _fetch_json(client: httpx.AsyncClient, url: str) -> Optional[dict]:
    try:
        r = await client.get(url, timeout=10.0, headers={"User-Agent": "Mozilla/5.0 (compatible; YupchaBot/1.0)"})
        if r.status_code == 200:
            return r.json()
    except Exception:
        return None
    return None


async def _try_greenhouse(client, slug) -> Optional[List[str]]:
    data = await _fetch_json(client, GREENHOUSE.format(slug=slug))
    if data and isinstance(data.get("jobs"), list):
        return [j.get("title", "") for j in data["jobs"] if j.get("title")]
    return None


async def _try_lever(client, slug) -> Optional[List[str]]:
    data = await _fetch_json(client, LEVER.format(slug=slug))
    if isinstance(data, list):
        return [j.get("text", "") for j in data if j.get("text")]
    return None


async def _try_ashby(client, slug) -> Optional[List[str]]:
    data = await _fetch_json(client, ASHBY.format(slug=slug))
    if data and isinstance(data.get("jobs"), list):
        return [j.get("title", "") for j in data["jobs"] if j.get("title")]
    return None


class AtsHiringProvider(EnrichmentProvider):
    name = "ats_hiring"
    capabilities = ["hiring_signals", "open_roles"]
    default_confidence = 0.8
    cost_per_lookup = 0.0  # free, public ATS boards

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.monotonic()
        slugs = _slug_candidates(lead)
        if not slugs:
            return EnrichmentResult(success=False, error="no_slug")

        async with httpx.AsyncClient(follow_redirects=True) as client:
            for slug in slugs:
                for fetch in (_try_greenhouse, _try_lever, _try_ashby):
                    titles = await fetch(client, slug)
                    if titles:  # found a live board with roles
                        signals = _titles_to_signals(titles)
                        signals["ats"] = fetch.__name__.replace("_try_", "")
                        signals["slug"] = slug
                        return EnrichmentResult(
                            success=True,
                            fields={
                                "hiring_signals": json.dumps(signals),
                                "open_roles": signals["open_roles"],
                            },
                            confidence=self.default_confidence,
                            duration_ms=(time.monotonic() - t0) * 1000.0,
                        )
        return EnrichmentResult(success=False, error="no_board", duration_ms=(time.monotonic() - t0) * 1000.0)
