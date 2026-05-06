"""
JobSpy Hiring Signals Provider.

From speedyapply/JobSpy (10K+ stars).
Detects hiring signals from job boards — companies actively hiring
are more likely to be buying tools, expanding teams, and responsive
to sales outreach.

Hiring signals are a high-value enrichment: companies posting 5+ jobs
are in "rapid growth" mode, which correlates strongly with purchase intent.
"""

import asyncio
import json
import logging
import re
from time import time
from typing import Dict, List, Optional

from apps.api.services.leadgen.enrichment.provider import (
    EnrichmentProvider, EnrichmentResult,
)
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.jobspy")

# Keywords that indicate sales/GTM expansion
GTM_KEYWORDS = [
    "sales", "marketing", "business development", "account executive",
    "sdr", "bdr", "demand generation", "growth", "partnerships",
    "customer success", "revenue", "outbound",
]

# Keywords that indicate tech stack expansion
TECH_KEYWORDS = [
    "python", "node", "react", "aws", "azure", "gcp", "devops",
    "data engineer", "ml engineer", "ai", "machine learning",
    "full stack", "backend", "frontend", "cloud",
]


def _analyze_jobs(jobs_data: list) -> Dict:
    """Analyze job postings to extract hiring signals."""
    signals = {
        "total_jobs": len(jobs_data),
        "growth_signal": "none",
        "gtm_expansion": False,
        "tech_hiring": False,
        "roles": [],
        "score_boost": 0,
    }

    if not jobs_data:
        return signals

    gtm_count = 0
    tech_count = 0

    for job in jobs_data:
        title = (job.get("title") or "").lower()
        desc = (job.get("description") or "").lower()
        combined = f"{title} {desc}"

        # Track role
        signals["roles"].append(job.get("title", "Unknown"))

        # Check for GTM keywords
        if any(kw in combined for kw in GTM_KEYWORDS):
            gtm_count += 1

        # Check for tech keywords
        if any(kw in combined for kw in TECH_KEYWORDS):
            tech_count += 1

    # Determine growth signal
    total = len(jobs_data)
    if total >= 10:
        signals["growth_signal"] = "hypergrowth"
        signals["score_boost"] = 20
    elif total >= 5:
        signals["growth_signal"] = "rapid_growth"
        signals["score_boost"] = 15
    elif total >= 2:
        signals["growth_signal"] = "growing"
        signals["score_boost"] = 10
    elif total >= 1:
        signals["growth_signal"] = "hiring"
        signals["score_boost"] = 5

    signals["gtm_expansion"] = gtm_count >= 2
    signals["tech_hiring"] = tech_count >= 2

    if signals["gtm_expansion"]:
        signals["score_boost"] += 5
    if signals["tech_hiring"]:
        signals["score_boost"] += 5

    # Cap roles list
    signals["roles"] = signals["roles"][:5]

    return signals


class JobSpySignalProvider(EnrichmentProvider):
    """Detect hiring signals from job boards.

    Uses DDG search (to avoid heavy python-jobspy dependency).
    Searches Indeed and Naukri for open positions.
    """

    name = "jobspy"
    capabilities = ["hiring_signals"]
    default_confidence = 0.7

    def __init__(self, max_jobs: int = 5, delay: float = 1.0):
        self.max_jobs = max_jobs
        self.delay = delay

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        """Search for hiring signals for a company."""
        start = time()

        if not lead.company:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="no_company_name",
                duration_ms=(time() - start) * 1000,
            )

        jobs = await self._search_jobs(lead.company, lead.city)
        signals = _analyze_jobs(jobs)

        if signals["total_jobs"] == 0:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="no_jobs_found",
                duration_ms=(time() - start) * 1000,
            )

        return EnrichmentResult(
            provider=self.name,
            success=True,
            confidence=0.7,
            fields={
                "hiring_signals": json.dumps(signals),
            },
            duration_ms=(time() - start) * 1000,
        )

    async def _search_jobs(self, company: str, city: str = "") -> List[Dict]:
        """Search for job postings using DDG (lightweight approach)."""
        from ddgs import DDGS
        from apps.api.services.leadgen.proxy_client import get_ddgs

        jobs = []
        queries = [
            f'site:indeed.com "{company}" jobs',
            f'site:naukri.com "{company}" jobs',
        ]

        if city:
            queries = [f'{q} {city}' for q in queries]

        def _search(query: str):
            try:
                with get_ddgs() as ddgs:
                    return list(ddgs.text(query, max_results=self.max_jobs))
            except Exception:
                return []

        for query in queries:
            results = await asyncio.to_thread(_search, query)

            for r in results:
                title = r.get("title", "")
                body = r.get("body", "")

                # Extract job title from search result
                # Indeed format: "Job Title - Company - City | Indeed.com"
                job_title = title.split(" - ")[0].strip() if " - " in title else title

                # Clean up
                job_title = re.sub(r'\s*[\|·]\s*(Indeed|Naukri|LinkedIn).*$', '', job_title)

                if job_title and len(job_title) < 100:
                    jobs.append({
                        "title": job_title,
                        "description": body[:200],
                        "source": "indeed" if "indeed" in query else "naukri",
                    })

            await asyncio.sleep(self.delay)

        # Deduplicate by normalized title
        seen = set()
        unique_jobs = []
        for job in jobs:
            key = job["title"].lower().strip()
            if key not in seen:
                seen.add(key)
                unique_jobs.append(job)

        return unique_jobs[:self.max_jobs * 2]
