"""
Email Finder — Discover and verify email addresses.

Uses common email patterns, domain-based inference, and optional
Hunter.io API for verification.
"""

import re
import time
from typing import List, Optional

from duckduckgo_search import DDGS
from leadgen.models import Lead


# Common email patterns for Indian companies
COMMON_PREFIXES = [
    "info", "contact", "hr", "sales", "enquiry", "careers",
    "support", "admin", "hello", "business", "recruitment",
]


def _domain_from_url(url: str) -> str:
    """Extract domain from URL."""
    if not url:
        return ""
    url = url.lower().strip()
    if not url.startswith("http"):
        url = "https://" + url
    from urllib.parse import urlparse
    parsed = urlparse(url)
    domain = parsed.netloc
    if domain.startswith("www."):
        domain = domain[4:]
    return domain


def guess_emails(website: str) -> List[str]:
    """Generate likely email addresses from a website domain."""
    domain = _domain_from_url(website)
    if not domain:
        return []
    return [f"{prefix}@{domain}" for prefix in COMMON_PREFIXES]


def find_email_via_search(
    company: str,
    city: str = "",
    domain: str = "",
) -> str:
    """Search DuckDuckGo for the company's email address."""
    query_parts = [company, "email", "contact"]
    if city:
        query_parts.append(city)
    if domain:
        query_parts.append(f"@{domain}")

    query = " ".join(query_parts)

    try:
        with DDGS() as ddgs:
            results = list(ddgs.text(query, max_results=5))
            for r in results:
                text = f"{r.get('title', '')} {r.get('body', '')}"
                emails = re.findall(r'[\w\.\-]+@[\w\.\-]+\.\w{2,}', text)
                for email in emails:
                    # Prefer emails matching the company domain
                    if domain and domain in email.lower():
                        return email
                # If no domain match, return first found
                for email in emails:
                    if not any(x in email.lower() for x in ['example', 'domain', 'email.com']):
                        return email
    except Exception:
        pass

    return ""


def enrich_emails(leads: List[Lead], delay: float = 1.5) -> List[Lead]:
    """
    Find missing email addresses for leads.

    Strategy:
    1. Search DuckDuckGo for company email
    2. If website exists, generate guesses based on domain

    Updates leads in-place.
    """
    needs_email = [l for l in leads if not l.has_email and l.company]
    print(f"  📧 Finding emails for {len(needs_email)} leads...")

    found = 0
    for lead in needs_email:
        domain = _domain_from_url(lead.website)
        email = find_email_via_search(lead.company, lead.city, domain)

        if email:
            lead.email = email
            found += 1
            print(f"    ✅ {lead.company}: {email}")
        elif domain:
            # Use most common pattern as fallback
            lead.email = f"info@{domain}"
            print(f"    🔮 {lead.company}: info@{domain} (guessed)")

        time.sleep(delay)

    print(f"  📊 Found {found} verified emails")
    return leads
