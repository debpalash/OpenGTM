"""
Website Scraper — Extract contact info from company websites.

Enhanced version using stealth HTTP client. Tries fast Tier 2 (HTTP) first,
falls back to browser (Tier 3) only when challenge pages are encountered.
"""

import asyncio
import re
from typing import Dict, List, Optional
from urllib.parse import urljoin, urlparse

from apps.api.services.leadgen.models import Lead

try:
    from bs4 import BeautifulSoup
except ImportError:
    BeautifulSoup = None


def _extract_phones(text: str) -> List[str]:
    """Extract all phone numbers from text."""
    patterns = [
        r'\+?91[\-\s]?\d{5}[\-\s]?\d{5}',
        r'\+?91[\-\s]?\d{10}',
        r'1800[\-\s]?\d{2,3}[\-\s]?\d{4,6}',
        r'0\d{2,4}[\-\s]?\d{6,8}',
        r'\b\d{10}\b',
    ]
    phones = []
    for pat in patterns:
        for match in re.finditer(pat, text):
            phone = match.group().strip()
            # Filter out obviously bad matches (years, zip codes, etc.)
            digits_only = re.sub(r'\D', '', phone)
            if 7 <= len(digits_only) <= 13:
                phones.append(phone)
    return phones


def _extract_emails(text: str) -> List[str]:
    """Extract all email addresses from text."""
    matches = re.findall(r'[\w\.\-\+]+@[\w\.\-]+\.\w{2,}', text)
    # Filter out common false positives
    filtered = []
    for email in matches:
        lower = email.lower()
        if not any(x in lower for x in ['example.com', 'domain.com', 'email.com', '.png', '.jpg', '.css', '.js']):
            filtered.append(email)
    return filtered


def _extract_social_links(soup) -> Dict[str, str]:
    """Extract social media URLs from page."""
    social = {"linkedin": "", "twitter": "", "facebook": "", "instagram": ""}

    for a in soup.find_all("a", href=True):
        href = a["href"].lower()
        if "linkedin.com/company" in href or "linkedin.com/in/" in href:
            social["linkedin"] = a["href"]
        elif "twitter.com/" in href or "x.com/" in href:
            social["twitter"] = a["href"]
        elif "facebook.com/" in href:
            social["facebook"] = a["href"]
        elif "instagram.com/" in href:
            social["instagram"] = a["href"]

    return social


async def _scrape_via_http(client, url: str) -> Dict:
    """Tier 2: Scrape website using stealth HTTP (no browser)."""
    result = {
        "phones": [],
        "emails": [],
        "social": {},
        "description": "",
    }

    pages_to_check = [
        url,
        urljoin(url, "/contact"),
        urljoin(url, "/contact-us"),
        urljoin(url, "/about"),
    ]

    for page_url in pages_to_check:
        resp = await client.fetch(page_url, tier=2, timeout=12)
        if not resp.ok:
            continue

        text = resp.text
        if BeautifulSoup:
            soup = BeautifulSoup(text, "html.parser")
            plain_text = soup.get_text(separator=" ")
        else:
            plain_text = text

        # Extract phones
        result["phones"].extend(_extract_phones(plain_text))

        # Extract phones from tel: links
        if BeautifulSoup and soup:
            for a in soup.find_all("a", href=True):
                if a["href"].startswith("tel:"):
                    phone = a["href"].replace("tel:", "").strip()
                    phone = re.sub(r'[^\d\+\-\s]', '', phone)
                    if phone:
                        result["phones"].append(phone)

        # Extract emails
        result["emails"].extend(_extract_emails(plain_text))

        # Extract emails from mailto: links
        if BeautifulSoup and soup:
            for a in soup.find_all("a", href=True):
                if a["href"].startswith("mailto:"):
                    email = a["href"].replace("mailto:", "").split("?")[0].strip()
                    if email and "@" in email:
                        result["emails"].append(email)

        # Social links + description (main page only)
        if page_url == url and BeautifulSoup and soup:
            result["social"] = _extract_social_links(soup)
            meta = soup.find("meta", attrs={"name": "description"})
            if meta and meta.get("content"):
                result["description"] = meta["content"][:300]

    # Deduplicate
    result["phones"] = list(dict.fromkeys(result["phones"]))[:5]
    result["emails"] = list(dict.fromkeys(result["emails"]))[:5]

    return result


async def enrich_leads_from_websites(
    leads: List[Lead],
    batch_size: int = 5,
    headless: bool = True,
) -> List[Lead]:
    """
    Enrich a batch of leads by scraping their websites for contact info.

    Uses Tier 2 (stealth HTTP) by default — much faster than browser.
    Only scrapes leads that are missing phone or email.
    Updates leads in-place and returns them.
    """
    from apps.api.services.leadgen.http import StealthClient

    client = StealthClient()

    # Filter to leads that need enrichment
    needs_enrichment = [
        l for l in leads
        if l.has_website and (not l.has_phone or not l.has_email)
    ]
    print(f"  🌐 Enriching {len(needs_enrichment)} leads from websites (stealth HTTP)...")

    for i in range(0, len(needs_enrichment), batch_size):
        batch = needs_enrichment[i:i + batch_size]
        tasks = []
        for lead in batch:
            url = lead.website
            if not url.startswith("http"):
                url = "https://" + url
            tasks.append((lead, _scrape_via_http(client, url)))

        results = await asyncio.gather(*(t[1] for t in tasks), return_exceptions=True)

        for (lead, _), result in zip(tasks, results):
            if isinstance(result, Exception):
                print(f"    ⚠ Error scraping {lead.company}: {result}")
                continue

            if result["phones"] and not lead.has_phone:
                lead.phone = result["phones"][0]
                print(f"    📞 {lead.company}: {lead.phone}")

            if result["emails"] and not lead.has_email:
                lead.email = result["emails"][0]
                print(f"    📧 {lead.company}: {lead.email}")

            if result["social"].get("linkedin") and not lead.has_linkedin:
                lead.linkedin_url = result["social"]["linkedin"]

            if result["social"].get("twitter") and not lead.twitter_url:
                lead.twitter_url = result["social"]["twitter"]

            if result["description"] and not lead.description:
                lead.description = result["description"]

    enriched_count = sum(1 for l in needs_enrichment if l.has_phone or l.has_email)
    print(f"  ✅ Enriched {enriched_count}/{len(needs_enrichment)} leads with contact info")
    return leads

