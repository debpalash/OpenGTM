"""
CrossLinked Provider — No-login LinkedIn people enumeration.

Ported from m8sec/CrossLinked (800+ stars).
Uses DuckDuckGo to search `site:linkedin.com/in "Company Name" "Title"`
and extracts name + title from search result snippets.

No LinkedIn login or cookies required.
"""

import asyncio
import json
import logging
import re
from typing import List, Dict, Optional
from time import time

from apps.api.services.leadgen.enrichment.provider import (
    EnrichmentProvider, EnrichmentResult,
)
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.crosslinked")

# Title keywords we look for (decision makers)
TITLE_KEYWORDS = [
    "CEO", "CTO", "COO", "CFO", "CMO", "CIO", "CHRO",
    "Founder", "Co-Founder", "Co Founder",
    "Director", "VP", "Vice President",
    "Head of", "Head -",
    "Managing Director", "MD",
    "Partner", "Principal",
    "Manager", "Lead",
    "Owner", "Proprietor",
]

# Search engine domains to exclude from results
SKIP_DOMAINS = [
    "linkedin.com", "facebook.com", "twitter.com", "x.com",
    "youtube.com", "instagram.com", "reddit.com", "quora.com",
    "wikipedia.org", "glassdoor.com", "ambitionbox.com",
]


def _parse_linkedin_name(text: str) -> str:
    """Extract person name from LinkedIn search result text.

    LinkedIn results typically look like:
    "John Doe - CEO at Company | LinkedIn"
    "Jane Smith, CTO - Company Name ..."
    """
    if not text:
        return ""

    # Remove "LinkedIn" suffix
    text = re.sub(r'\s*[\|·\-–]\s*LinkedIn.*$', '', text, flags=re.IGNORECASE)

    # Take the part before the first separator (dash, pipe, comma)
    name_part = re.split(r'\s*[\|·\-–,]\s*', text)[0].strip()

    # Clean up: remove "Dr.", "Mr.", "Mrs." etc.
    name_part = re.sub(r'^(Dr|Mr|Mrs|Ms|Prof)\.\s*', '', name_part)

    # Must look like a name (2-4 words, alphabetic)
    words = name_part.split()
    if 1 <= len(words) <= 5 and all(w.replace('.', '').replace("'", "").isalpha() for w in words):
        return name_part

    return ""


def _parse_linkedin_title(text: str) -> str:
    """Extract job title from LinkedIn search result text.

    "John Doe - CEO at Company | LinkedIn"  → "CEO"
    "Jane Smith - Head of Sales - Company"   → "Head of Sales"
    """
    if not text:
        return ""

    # Remove LinkedIn suffix
    text = re.sub(r'\s*[\|·]\s*LinkedIn.*$', '', text, flags=re.IGNORECASE)

    parts = text.split(' - ')
    if len(parts) >= 2:
        # Title is typically the second part
        title = parts[1].strip()
        # Remove "at Company" suffix
        title = re.sub(r'\s+at\s+.*$', '', title, flags=re.IGNORECASE)
        # Remove company name indicators
        title = re.sub(r'\s*[\|·]\s*.*$', '', title)

        if title and len(title) < 80:
            return title.strip()

    return ""


def _extract_linkedin_url(href: str) -> str:
    """Extract clean LinkedIn profile URL."""
    if not href:
        return ""
    # Match linkedin.com/in/username patterns
    match = re.search(r'(https?://(?:www\.)?linkedin\.com/in/[a-zA-Z0-9\-_%]+)', href)
    return match.group(1) if match else ""


async def _ddg_linkedin_search(query: str, max_results: int = 10) -> list:
    """Search DDG for LinkedIn profiles with retry + proxy rotation."""
    from ddgs import DDGS
    from apps.api.services.leadgen.proxy_client import get_proxy

    MAX_ATTEMPTS = 3

    for attempt in range(MAX_ATTEMPTS):
        # Attempt 0: use proxy client (may or may not have proxy)
        # Attempt 1: use a fresh proxy
        # Attempt 2: direct connection (no proxy)
        proxy = None
        if attempt < MAX_ATTEMPTS - 1:
            proxy = get_proxy()
        
        def _search():
            ddgs = DDGS(proxy=proxy) if proxy else DDGS()
            with ddgs:
                return list(ddgs.text(query, max_results=max_results))

        try:
            return await asyncio.to_thread(_search)
        except Exception as e:
            is_connect_error = "ConnectError" in str(type(e).__name__) or "ConnectError" in str(e)
            if is_connect_error and attempt < MAX_ATTEMPTS - 1:
                logger.debug(f"DDG search attempt {attempt + 1} failed (ConnectError), retrying...")
                await asyncio.sleep(0.5 * (attempt + 1))
                continue
            logger.warning(f"DDG search failed after {attempt + 1} attempts: {e}")
            return []


class CrossLinkedProvider(EnrichmentProvider):
    """Find decision makers via search engine LinkedIn enumeration.

    No login required — uses DDG to search LinkedIn public profiles.
    """

    name = "crosslinked"
    capabilities = ["decision_makers", "contact_person", "contact_title"]
    default_confidence = 0.6

    def __init__(self, max_people: int = 5, delay: float = 1.0):
        self.max_people = max_people
        self.delay = delay

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        """Find LinkedIn people for a company."""
        start = time()

        if not lead.company:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="no_company_name",
                duration_ms=(time() - start) * 1000,
            )

        people = await self.find_people(lead.company, lead.city)

        if not people:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="no_people_found",
                duration_ms=(time() - start) * 1000,
            )

        # Build decision_makers JSON
        dm_json = json.dumps(people[:self.max_people])

        # Use the first (highest-ranking) person as primary contact
        primary = people[0]

        return EnrichmentResult(
            provider=self.name,
            success=True,
            confidence=0.65,
            fields={
                "decision_makers": dm_json,
                "contact_person": primary.get("name", ""),
                "contact_title": primary.get("title", ""),
            },
            duration_ms=(time() - start) * 1000,
        )

    async def find_people(
        self,
        company_name: str,
        city: str = "",
    ) -> List[Dict[str, str]]:
        """Search for company employees on LinkedIn via DDG.

        Returns list of dicts: [{name, title, linkedin_url}]
        """
        people: List[Dict[str, str]] = []
        seen_names: set = set()

        # Strategy 1: General company search
        query = f'site:linkedin.com/in "{company_name}"'
        if city:
            query += f' "{city}"'

        results = await _ddg_linkedin_search(query, max_results=15)

        for r in results:
            person = self._parse_result(r)
            if person and person["name"].lower() not in seen_names:
                seen_names.add(person["name"].lower())
                people.append(person)

        # Strategy 2: Title-specific searches (if not enough results)
        if len(people) < 3:
            await asyncio.sleep(self.delay)

            title_groups = ["CEO OR CTO OR Founder", "Director OR VP OR Head"]
            for titles in title_groups:
                query = f'site:linkedin.com/in "{company_name}" {titles}'
                results = await _ddg_linkedin_search(query, max_results=8)

                for r in results:
                    person = self._parse_result(r)
                    if person and person["name"].lower() not in seen_names:
                        seen_names.add(person["name"].lower())
                        people.append(person)

                if len(people) >= self.max_people:
                    break

                await asyncio.sleep(self.delay)

        # Sort: prioritize people with known titles (C-level first)
        def _title_rank(p: dict) -> int:
            t = (p.get("title") or "").upper()
            if any(x in t for x in ["CEO", "FOUNDER", "OWNER", "MD"]):
                return 0
            if any(x in t for x in ["CTO", "COO", "CFO", "CMO", "CIO"]):
                return 1
            if any(x in t for x in ["VP", "VICE PRESIDENT", "DIRECTOR"]):
                return 2
            if any(x in t for x in ["HEAD", "LEAD", "MANAGER"]):
                return 3
            return 4

        people.sort(key=_title_rank)

        return people[:self.max_people]

    def _parse_result(self, result: dict) -> Optional[Dict[str, str]]:
        """Parse a DDG search result into a person dict."""
        title_text = result.get("title", "")
        body_text = result.get("body", "")
        href = result.get("href", "")

        # Must be a LinkedIn profile URL
        linkedin_url = _extract_linkedin_url(href)
        if not linkedin_url:
            return None

        # Extract name
        name = _parse_linkedin_name(title_text)
        if not name:
            return None

        # Reject if name looks like a company or generic page
        name_lower = name.lower()
        if any(x in name_lower for x in ["linkedin", "company", "page", "profile"]):
            return None

        # Extract title
        job_title = _parse_linkedin_title(title_text)
        if not job_title:
            # Try extracting from body text
            for keyword in TITLE_KEYWORDS:
                if keyword.lower() in body_text.lower():
                    # Extract a chunk around the keyword
                    idx = body_text.lower().index(keyword.lower())
                    chunk = body_text[idx:idx+40].split('.')[0].split(',')[0]
                    job_title = chunk.strip()
                    break

        return {
            "name": name,
            "title": job_title or "N/A",
            "linkedin": linkedin_url,
        }
