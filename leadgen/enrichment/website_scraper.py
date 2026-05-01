"""
Website Scraper — Extract contact info from company websites.

Enhanced version of the original enrich_patchright.py. Navigates to
company websites and extracts phone numbers, emails, social links,
and company details from the main page + /contact, /about pages.
"""

import asyncio
import re
from typing import Dict, List, Optional
from urllib.parse import urljoin, urlparse

from leadgen.models import Lead

try:
    from patchright.async_api import async_playwright
    from bs4 import BeautifulSoup
except ImportError:
    async_playwright = None
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


async def scrape_website(context, url: str) -> Dict:
    """
    Scrape a single company website for contact information.

    Checks the main page, then /contact and /about pages for
    phone numbers, emails, and social links.

    Returns dict with: phones, emails, social, description
    """
    result = {
        "phones": [],
        "emails": [],
        "social": {},
        "description": "",
    }

    if not url or url in ("N/A", "nan", ""):
        return result

    if not url.startswith("http"):
        url = "https://" + url

    pages_to_check = [
        url,
        urljoin(url, "/contact"),
        urljoin(url, "/contact-us"),
        urljoin(url, "/about"),
        urljoin(url, "/about-us"),
    ]

    for page_url in pages_to_check:
        page = await context.new_page()
        try:
            await page.goto(page_url, wait_until="domcontentloaded", timeout=12000)
            content = await page.content()
            soup = BeautifulSoup(content, "html.parser")
            text = soup.get_text(separator=" ")

            # Extract phones from text
            phones = _extract_phones(text)
            result["phones"].extend(phones)

            # Extract phones from tel: links
            for a in soup.find_all("a", href=True):
                if a["href"].startswith("tel:"):
                    phone = a["href"].replace("tel:", "").strip()
                    phone = re.sub(r'[^\d\+\-\s]', '', phone)
                    if phone:
                        result["phones"].append(phone)

            # Extract emails from text
            emails = _extract_emails(text)
            result["emails"].extend(emails)

            # Extract emails from mailto: links
            for a in soup.find_all("a", href=True):
                if a["href"].startswith("mailto:"):
                    email = a["href"].replace("mailto:", "").split("?")[0].strip()
                    if email and "@" in email:
                        result["emails"].append(email)

            # Extract social links (only from main page)
            if page_url == url:
                result["social"] = _extract_social_links(soup)

                # Try to get meta description
                meta = soup.find("meta", attrs={"name": "description"})
                if meta and meta.get("content"):
                    result["description"] = meta["content"][:300]

        except Exception:
            pass
        finally:
            await page.close()

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

    Only scrapes leads that are missing phone or email.
    Updates leads in-place and returns them.
    """
    if async_playwright is None:
        print("  ⚠ patchright not installed. Run: pip install patchright")
        return leads

    # Filter to leads that need enrichment
    needs_enrichment = [
        l for l in leads
        if l.has_website and (not l.has_phone or not l.has_email)
    ]
    print(f"  🌐 Enriching {len(needs_enrichment)} leads from websites...")

    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=headless)
        context = await browser.new_context(
            viewport={"width": 1280, "height": 900},
        )

        for i in range(0, len(needs_enrichment), batch_size):
            batch = needs_enrichment[i:i + batch_size]
            tasks = [(lead, scrape_website(context, lead.website)) for lead in batch]
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

        await browser.close()

    enriched_count = sum(1 for l in needs_enrichment if l.has_phone or l.has_email)
    print(f"  ✅ Enriched {enriched_count}/{len(needs_enrichment)} leads with contact info")
    return leads
