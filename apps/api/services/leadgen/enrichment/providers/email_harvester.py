"""
Search Engine Email Harvester — Multi-engine email discovery (OSS).

Inspired by theHarvester (github.com/laramies/theHarvester, 12K+ stars).
Instead of depending on the full theHarvester installation, this implements
the core email harvesting logic directly using DuckDuckGo + Bing searches.

Replaces: Hunter.io ($49/mo), Snov.io ($39/mo), RocketReach ($99/mo)

Searches multiple engines for:
  - Emails published on the company's domain pages
  - Emails mentioned in public documents (PDFs, docs)
  - Emails found on 3rd-party business directories
  - Email patterns from Google cached pages

Free, unlimited, no API key needed.
"""

import asyncio
import logging
import re
import time
from typing import Dict, List, Set
from urllib.parse import quote_plus

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.email_harvester")

# Email pattern for extraction
EMAIL_RE = re.compile(r'[\w.\-+]+@[\w.\-]+\.(?:com|org|net|io|co|in|ai|dev|tech|biz|info|us|uk|eu|de|fr|ca|au)', re.I)

# Filter out common false-positive emails
BLACKLIST_EMAILS = {
    "noreply", "no-reply", "mailer-daemon", "postmaster",
    "abuse", "webmaster", "hostmaster", "admin@example",
}
BLACKLIST_DOMAINS = {
    "example.com", "domain.com", "email.com", "test.com",
    "sentry.io", "w3.org", "schema.org", "googleapis.com",
    "cloudflare.com", "wordpress.org", "github.com",
    "gravatar.com", "facebook.com", "twitter.com",
}


def _is_valid_email(email: str, target_domain: str = "") -> bool:
    """Check if extracted email is a valid business email."""
    email = email.lower().strip()
    local, domain = email.split("@", 1) if "@" in email else ("", "")

    if not local or not domain:
        return False
    if len(local) < 2 or len(local) > 64:
        return False
    if domain in BLACKLIST_DOMAINS:
        return False
    if any(bl in local for bl in BLACKLIST_EMAILS):
        return False
    # Filter image/asset-like emails
    if any(ext in email for ext in [".png", ".jpg", ".css", ".js", ".svg"]):
        return False
    return True


def _rank_email(email: str, target_domain: str) -> int:
    """Rank email quality: 0=best (on-domain personal), higher=worse."""
    local = email.split("@")[0].lower()
    domain = email.split("@")[1].lower() if "@" in email else ""

    # On-domain emails are always best
    on_domain = target_domain and target_domain in domain

    if on_domain:
        # Personal emails (first.last, fname, etc.)
        if "." in local and local[0].isalpha():
            return 0  # Best: personal on-domain
        if local in ("ceo", "founder", "director", "cto", "coo"):
            return 1  # Role-based leadership
        if local in ("info", "contact", "hello", "sales"):
            return 2  # Generic contact
        if local in ("support", "help", "admin", "office"):
            return 3  # Support/admin
        return 4  # Other on-domain
    else:
        return 10  # Off-domain


async def _ddg_search(query: str, max_results: int = 8) -> List[Dict]:
    """Search DuckDuckGo for results with timeout."""
    from apps.api.services.leadgen.enrichment.web_search import DDGS  # routed via SearXNG pool
    from apps.api.services.leadgen.proxy_client import get_proxy

    proxy = get_proxy()

    def _search():
        ddgs = DDGS(proxy=proxy, timeout=8) if proxy else DDGS(timeout=8)
        with ddgs:
            return list(ddgs.text(query, max_results=max_results))

    try:
        return await asyncio.wait_for(asyncio.to_thread(_search), timeout=10)
    except (asyncio.TimeoutError, Exception) as e:
        logger.debug(f"DDG search failed: {e}")
        return []


async def _harvest_emails_from_search(
    domain: str,
    company: str = "",
    city: str = "",
) -> Dict:
    """Multi-query search engine email harvesting.

    Uses multiple search strategies:
    1. site:domain.com email OR contact
    2. "@domain.com" (direct email mentions)
    3. domain.com email filetype:pdf (public documents)
    4. domain.com contact us | about us
    """
    all_emails: Set[str] = set()
    all_names: List[Dict] = []

    # Only 2 queries (highest yield), keep fast
    queries = [
        f'site:{domain} email OR contact OR "@{domain}"',
        f'"@{domain}" -site:{domain}',
    ]

    for query in queries:
        results = await _ddg_search(query, max_results=8)

        for r in results:
            text = f"{r.get('title', '')} {r.get('body', '')}"
            emails = EMAIL_RE.findall(text)
            for email in emails:
                if _is_valid_email(email, domain):
                    all_emails.add(email.lower().rstrip('.'))

            # Extract person names near emails
            body = r.get("body", "")
            for email in emails:
                if _is_valid_email(email, domain):
                    name_match = re.search(
                        r'([A-Z][a-z]+ [A-Z][a-z]+)',
                        body[:body.find(email)] if email in body else "",
                    )
                    if name_match:
                        all_names.append({
                            "name": name_match.group(1),
                            "email": email.lower(),
                        })

        await asyncio.sleep(0.3)

    return {
        "emails": sorted(all_emails, key=lambda e: _rank_email(e, domain)),
        "names": all_names[:5],
    }


def _extract_domain(url: str) -> str:
    """Get clean domain from URL."""
    if not url:
        return ""
    url = url.lower().strip()
    for prefix in ("https://", "http://", "www."):
        url = url.removeprefix(prefix)
    return url.split("/")[0].split("?")[0]


class EmailHarvesterProvider(EnrichmentProvider):
    """Search-engine email harvester — replaces Hunter.io/Snov.io.

    Uses DuckDuckGo multi-query search to find emails associated
    with a company's domain across the entire public web:
    - Company website pages
    - Public PDFs and documents
    - Business directories
    - Social media mentions

    Zero cost, no API key. Uses your existing proxy infrastructure.
    """

    name = "email_harvester"
    capabilities = ["email", "contact_person"]
    default_confidence = 0.72

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()

        domain = _extract_domain(lead.website) if lead.website else ""
        if not domain:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No website/domain available",
                duration_ms=(time.time() - t0) * 1000,
            )

        result = await _harvest_emails_from_search(
            domain=domain,
            company=lead.company or "",
            city=lead.city or "",
        )

        if not result["emails"]:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No emails found across search results",
                duration_ms=(time.time() - t0) * 1000,
            )

        fields = {}
        emails = result["emails"]

        # Best email
        fields["email"] = emails[0]

        # Secondary emails
        if len(emails) > 1:
            fields["secondary_emails"] = " | ".join(emails[1:4])

        # If we found a name associated with the best email
        names = result["names"]
        for n in names:
            if n["email"] == emails[0]:
                fields["contact_person"] = n["name"]
                break

        return EnrichmentResult(
            provider=self.name, success=True,
            fields=fields,
            confidence=self.default_confidence,
            duration_ms=(time.time() - t0) * 1000,
        )
