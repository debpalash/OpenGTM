"""
Decision Maker Finder — Discover key contacts at companies.

Uses DuckDuckGo to find CEOs, CTOs, CHROs, and Founders on LinkedIn,
then stores them as structured JSON in the lead's decision_makers field.
"""

import asyncio
import json
import re
import time
from typing import List, Optional

from apps.api.services.leadgen.models import Lead


# Titles to search for, ordered by decision-making authority
TARGET_TITLES = [
    "Founder", "CEO", "Managing Director", "CHRO",
    "HR Director", "CTO", "COO", "VP HR",
]


async def _search_decision_maker(company: str, title: str) -> Optional[dict]:
    """Search DDG for a decision maker at a company."""
    try:
        from ddgs import DDGS
        from apps.api.services.leadgen.proxy_client import get_ddgs
        query = f'site:linkedin.com/in "{company}" "{title}" India'
        with get_ddgs() as ddgs:
            results = list(ddgs.text(query, max_results=2))

        for r in results:
            href = r.get("href", "")
            result_title = r.get("title", "")

            if "/in/" not in href:
                continue

            # LinkedIn titles: "Person Name - Title - Company | LinkedIn"
            person_name = result_title.split(" - ")[0].split(" | ")[0].strip()
            if not person_name or person_name.lower() == "linkedin":
                continue
            if len(person_name) < 3 or len(person_name) > 60:
                continue

            # Clean the LinkedIn URL
            linkedin_url = href.split("?")[0].rstrip("/")

            return {
                "name": person_name,
                "title": title,
                "linkedin": linkedin_url,
            }
    except Exception:
        pass
    return None


async def find_decision_makers_for_lead(lead: Lead, max_contacts: int = 3) -> Lead:
    """Find decision makers for a single lead.

    Updates lead.decision_makers with a JSON array and
    sets lead.contact_person / contact_title from the first result.
    """
    if not lead.company or len(lead.company) < 3:
        return lead

    # Skip if already has decision makers
    if lead.decision_makers:
        try:
            existing = json.loads(lead.decision_makers)
            if len(existing) >= max_contacts:
                return lead
        except (json.JSONDecodeError, TypeError):
            pass

    found = []

    for title in TARGET_TITLES:
        if len(found) >= max_contacts:
            break

        result = await _search_decision_maker(lead.company, title)
        if result:
            # Avoid duplicate people
            if not any(f["name"].lower() == result["name"].lower() for f in found):
                found.append(result)

        # Rate limiting
        await asyncio.sleep(1.5)

    if found:
        lead.decision_makers = json.dumps(found)

        # Set primary contact from first result
        if not lead.contact_person:
            lead.contact_person = found[0]["name"]
            lead.contact_title = found[0]["title"]
        if not lead.linkedin_url and found[0].get("linkedin"):
            lead.linkedin_url = found[0]["linkedin"]

    return lead


async def enrich_decision_makers(
    leads: List[Lead],
    concurrency: int = 3,
    max_contacts: int = 3,
) -> List[Lead]:
    """Find decision makers for leads that are missing them.

    Processes leads in batches to avoid rate limiting.
    Updates leads in-place.
    """
    needs_enrichment = [
        l for l in leads
        if l.company and not l.decision_makers and not l.contact_person
    ]

    if not needs_enrichment:
        return leads

    print(f"  👤 Finding decision makers for {len(needs_enrichment)} leads...")

    sem = asyncio.Semaphore(concurrency)
    found_count = 0

    async def _limited(lead):
        nonlocal found_count
        async with sem:
            await find_decision_makers_for_lead(lead, max_contacts)
            if lead.decision_makers:
                found_count += 1
                print(f"    ✅ {lead.company}: {lead.contact_person} ({lead.contact_title})")

    await asyncio.gather(*[_limited(l) for l in needs_enrichment])

    print(f"  📊 Found decision makers for {found_count}/{len(needs_enrichment)} leads")
    return leads
