"""
Search Enricher — Fallback enrichment via DuckDuckGo search.

Searches for missing fields (phone, website, email) using web search
when website scraping doesn't yield results.
"""

import re
import time
from typing import List

from ddgs import DDGS
from leadgen.models import Lead


def _extract_phone(text: str) -> str:
    """Extract first phone number from text."""
    patterns = [
        r'\+?91[\-\s]?\d{5}[\-\s]?\d{5}',
        r'\+?91[\-\s]?\d{10}',
        r'1800[\-\s]?\d{2,3}[\-\s]?\d{4,6}',
        r'0\d{2,4}[\-\s]?\d{6,8}',
        r'\b\d{10}\b',
    ]
    for pat in patterns:
        match = re.search(pat, text)
        if match:
            return match.group().strip()
    return ""


def _extract_email(text: str) -> str:
    """Extract first email from text."""
    matches = re.findall(r'[\w\.\-]+@[\w\.\-]+\.\w{2,}', text)
    for email in matches:
        if not any(x in email.lower() for x in ['example', 'domain.com', '.png', '.jpg']):
            return email
    return ""


def enrich_via_search(
    leads: List[Lead],
    delay: float = 1.5,
    max_results: int = 5,
) -> List[Lead]:
    """
    Enrich leads using DuckDuckGo search as a fallback.

    For each lead missing phone, website, or email, searches for
    "{company} {city} official website contact number email" and
    extracts data from search result snippets.

    Updates leads in-place.
    """
    needs_enrichment = [
        l for l in leads
        if l.company and (not l.has_phone or not l.has_website or not l.has_email)
    ]
    print(f"  🔍 Search-enriching {len(needs_enrichment)} leads...")

    with DDGS() as ddgs:
        for lead in needs_enrichment:
            parts = [lead.company]
            if lead.city:
                parts.append(lead.city)

            if not lead.has_website:
                parts.append("official website")
            if not lead.has_phone:
                parts.append("contact number")
            if not lead.has_email:
                parts.append("email")

            query = " ".join(parts)

            try:
                results = list(ddgs.text(query, max_results=max_results))

                for r in results:
                    text = f"{r.get('title', '')} {r.get('body', '')}"
                    href = r.get("href", "")

                    # Website
                    if not lead.has_website and href and "http" in href:
                        # Prefer the company's own domain, not directory listings
                        from urllib.parse import urlparse
                        domain = urlparse(href).netloc.lower()
                        skip_domains = [
                            "justdial", "sulekha", "indiamart", "linkedin",
                            "facebook", "twitter", "glassdoor", "ambitionbox",
                            "wikipedia", "youtube",
                        ]
                        if not any(sd in domain for sd in skip_domains):
                            lead.website = href
                            print(f"    🌐 {lead.company}: {href}")

                    # Phone
                    if not lead.has_phone:
                        phone = _extract_phone(text)
                        if phone:
                            lead.phone = phone
                            print(f"    📞 {lead.company}: {phone}")

                    # Email
                    if not lead.has_email:
                        email = _extract_email(text)
                        if email:
                            lead.email = email
                            print(f"    📧 {lead.company}: {email}")

            except Exception as e:
                print(f"    ⚠ Search error for {lead.company}: {e}")

            time.sleep(delay)

    return leads
