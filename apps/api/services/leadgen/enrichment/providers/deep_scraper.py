"""
Deep Website Scraper — Clay-tier company intelligence from public websites.

Goes far beyond the basic website_scraper by crawling multiple pages and
extracting structured data that premium providers charge $0.02-0.10/lookup for.

What this extracts (for FREE):
  - Company size from /about, /careers, team pages
  - Founding year from /about
  - Team members (names + titles) from /team, /about, /leadership
  - Address from /contact, footer, structured data
  - Industry from meta tags, og:description, structured data
  - Email patterns (info@, contact@, hello@, support@)
  - Phone from tel: links, structured data, footer
  - Social links (LinkedIn, Twitter, Facebook, Instagram)
  - Tech stack (via Wappalyzer-style detection, delegated to tech_stack)
  - Description from meta, og, structured data

Inspired by:
  - theHarvester (github.com/laramies/theHarvester) — email enumeration
  - CrossLinked (github.com/m8sec/CrossLinked) — LinkedIn enumeration
  - SpiderFoot (github.com/smicallef/spiderfoot) — OSINT framework
  - Photon (github.com/s0md3v/Photon) — web crawler + OSINT
  - Recon-ng (github.com/lanmaster53/recon-ng) — reconnaissance framework

Free, unlimited, no API key needed.
"""

import asyncio
import json
import re
import time
import logging
from typing import Dict, List, Optional, Tuple
from urllib.parse import urljoin, urlparse

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

try:
    from bs4 import BeautifulSoup
except ImportError:
    BeautifulSoup = None

logger = logging.getLogger("leadgen.deep_scraper")


# ── Page paths to crawl (ordered by value) ────────────────────────────────
PAGES_TO_CRAWL = [
    "/",                    # Homepage
    "/about",               # About page (company info, founding year)
    "/about-us",
    "/about-us/",
    "/contact",             # Contact page (address, phone, email)
    "/contact-us",
    "/team",                # Team page (people + titles)
    "/our-team",
    "/leadership",
    "/people",
    "/management",
    "/careers",             # Careers (company size signal, hiring)
    "/jobs",
    "/privacy",             # Privacy page (legal entity name, address)
]

# ── Email pattern templates ──────────────────────────────────────────────
EMAIL_PATTERNS = [
    "{first}.{last}@{domain}",
    "{first}{last}@{domain}",
    "{f}{last}@{domain}",
    "{first}_{last}@{domain}",
    "{first}@{domain}",
    "{last}@{domain}",
    "{f}.{last}@{domain}",
]

# ── Company size indicators ──────────────────────────────────────────────
SIZE_PATTERNS = [
    # Direct employee count mentions
    (r'(\d[\d,]+)\+?\s*(?:employees|team members|people|staff|professionals)', "count"),
    (r'(?:team|staff|employees?)\s*(?:of|:)\s*(\d[\d,]+)', "count"),
    (r'(?:over|more than|approximately|about|~)\s*(\d[\d,]+)\s*(?:employees|people|team)', "count"),
    # Range-based
    (r'(\d+)\s*-\s*(\d+)\s*(?:employees|people)', "range"),
    # Explicit size categories
    (r'(?:small|startup|early.?stage)', "1-10"),
    (r'(?:mid.?size|medium|growing)', "11-50"),
    (r'(?:large|enterprise|global)', "200+"),
]

# ── Founding year patterns ───────────────────────────────────────────────
YEAR_PATTERNS = [
    r'(?:founded|established|since|started|incorporated|est\.?)\s*(?:in\s*)?(\d{4})',
    r'(?:since|est\.?)\s*(\d{4})',
    r'©\s*(\d{4})',
]

# ── Address patterns ─────────────────────────────────────────────────────
ADDRESS_INDICATORS = [
    "address", "headquarters", "hq", "office", "location",
    "registered office", "corporate office", "head office",
]

# ── Person title keywords (for team page extraction) ─────────────────────
LEADERSHIP_TITLES = [
    "ceo", "cto", "coo", "cfo", "cmo", "cio", "chro", "cpo",
    "founder", "co-founder", "cofounder",
    "president", "vice president", "vp",
    "director", "managing director",
    "head of", "head -", "chief",
    "partner", "principal", "owner",
]


def _extract_domain(url: str) -> str:
    """Get domain from URL."""
    if not url:
        return ""
    url = url.strip()
    for prefix in ("https://", "http://", "www."):
        url = url.removeprefix(prefix)
    return url.split("/")[0].split("?")[0].lower()


def _extract_structured_data(soup) -> Dict:
    """Extract JSON-LD structured data (Schema.org) from page.

    Many company websites embed Organization, LocalBusiness, or
    ContactPoint schema — this is premium-quality structured data
    that Clay's Clearbit/Crunchbase providers charge for.
    """
    data = {}
    if not soup:
        return data

    for script in soup.find_all("script", type="application/ld+json"):
        try:
            ld = json.loads(script.string or "")
            # Handle @graph arrays
            items = ld if isinstance(ld, list) else [ld]
            if isinstance(ld, dict) and "@graph" in ld:
                items = ld["@graph"]

            for item in items:
                if not isinstance(item, dict):
                    continue
                item_type = (item.get("@type", "") or "").lower()

                if item_type in ("organization", "corporation", "localbusiness",
                                 "store", "restaurant", "company"):
                    if item.get("name"):
                        data["legal_name"] = item["name"]
                    if item.get("description"):
                        data["description"] = str(item["description"])[:500]
                    if item.get("email"):
                        data["email"] = item["email"]
                    if item.get("telephone"):
                        data["phone"] = item["telephone"]
                    if item.get("numberOfEmployees"):
                        emp = item["numberOfEmployees"]
                        if isinstance(emp, dict):
                            data["employee_count"] = emp.get("value", str(emp))
                        else:
                            data["employee_count"] = str(emp)
                    if item.get("foundingDate"):
                        data["founding_year"] = str(item["foundingDate"])[:4]
                    if item.get("industry"):
                        data["industry"] = item["industry"]

                    # Address
                    addr = item.get("address", {})
                    if isinstance(addr, dict):
                        parts = [
                            addr.get("streetAddress", ""),
                            addr.get("addressLocality", ""),
                            addr.get("addressRegion", ""),
                            addr.get("postalCode", ""),
                            addr.get("addressCountry", ""),
                        ]
                        full_addr = ", ".join(p for p in parts if p)
                        if full_addr:
                            data["address"] = full_addr

                    # Social links
                    same_as = item.get("sameAs", [])
                    if isinstance(same_as, str):
                        same_as = [same_as]
                    for link in same_as:
                        if isinstance(link, str):
                            ll = link.lower()
                            if "linkedin.com" in ll:
                                data["linkedin"] = link
                            elif "twitter.com" in ll or "x.com" in ll:
                                data["twitter"] = link
                            elif "facebook.com" in ll:
                                data["facebook"] = link
                            elif "instagram.com" in ll:
                                data["instagram"] = link

                # ContactPoint
                if item_type == "contactpoint" or "contactPoint" in item:
                    cp = item if item_type == "contactpoint" else item.get("contactPoint", {})
                    if isinstance(cp, list):
                        cp = cp[0] if cp else {}
                    if isinstance(cp, dict):
                        if cp.get("email"):
                            data.setdefault("email", cp["email"])
                        if cp.get("telephone"):
                            data.setdefault("phone", cp["telephone"])

        except (json.JSONDecodeError, TypeError):
            continue

    return data


def _extract_team_members(soup, text: str) -> List[Dict[str, str]]:
    """Extract team/leadership members from page content.

    Looks for common patterns in /team, /about, /leadership pages:
    - <h3>Name</h3><p>Title</p> cards
    - <div class="team-member">... patterns
    - Inline "Name, Title" or "Name - Title" patterns
    """
    people = []
    seen_names = set()
    if not soup:
        return people

    # Strategy 1: Look for team member cards (common HTML patterns)
    card_selectors = [
        {"class": re.compile(r"team|member|leader|executive|people|staff|person", re.I)},
        {"class": re.compile(r"card|profile|bio", re.I)},
    ]

    for selector in card_selectors:
        for el in soup.find_all(["div", "li", "article", "section"], attrs=selector):
            # Look for name in heading tags
            name_el = el.find(["h2", "h3", "h4", "h5", "strong"])
            if not name_el:
                continue
            name = name_el.get_text(strip=True)
            if not name or len(name) > 60 or len(name.split()) < 2:
                continue
            # Filter out non-person names (navigation items, CTA buttons, etc.)
            noise_words = ['explore', 'learn', 'discover', 'see', 'view', 'our', 'your',
                          'read', 'more', 'find', 'get', 'try', 'start', 'click', 'download',
                          'subscribe', 'join', 'sign', 'products', 'services', 'solutions',
                          'contact', 'flagship', 'features', 'platform']
            name_lower = name.lower()
            if any(w in name_lower for w in noise_words):
                continue
            # Name should start with a capital letter and have at least one space
            if not name[0].isupper() or not any(c == ' ' for c in name):
                continue

            # Look for title in p, span, or following text
            title = ""
            title_el = el.find(["p", "span", "small"], class_=re.compile(r"title|role|position|designation", re.I))
            if title_el:
                title = title_el.get_text(strip=True)
            else:
                # Fallback: next sibling p tag
                for sibling in [el.find("p"), name_el.find_next("p")]:
                    if sibling:
                        t = sibling.get_text(strip=True)
                        if t and len(t) < 80 and any(kw in t.lower() for kw in LEADERSHIP_TITLES):
                            title = t
                            break

            if name and name.lower() not in seen_names:
                seen_names.add(name.lower())
                people.append({"name": name, "title": title or "N/A"})

    # Strategy 2: Regex for "Name — Title" or "Name, Title" patterns
    if len(people) < 3:
        inline_patterns = [
            r'([A-Z][a-z]+ [A-Z][a-z]+)\s*[-–—|,]\s*((?:CEO|CTO|COO|CFO|CMO|Founder|Co-Founder|Director|VP|Vice President|Head of|Managing Director|Partner|Owner)[^<\n]{0,40})',
        ]
        for pat in inline_patterns:
            for m in re.finditer(pat, text):
                name, title = m.group(1).strip(), m.group(2).strip()
                if name.lower() not in seen_names and len(name.split()) >= 2:
                    seen_names.add(name.lower())
                    people.append({"name": name, "title": title})

    return people[:10]  # Cap at 10


def _extract_company_size(text: str) -> Optional[str]:
    """Extract employee count or size category from page text."""
    for pattern, ptype in SIZE_PATTERNS:
        m = re.search(pattern, text, re.I)
        if m:
            if ptype == "count":
                count_str = m.group(1).replace(",", "")
                try:
                    count = int(count_str)
                    if 1 <= count <= 1_000_000:
                        return str(count)
                except ValueError:
                    pass
            elif ptype == "range":
                return f"{m.group(1)}-{m.group(2)}"
            else:
                return ptype
    return None


def _extract_founding_year(text: str) -> Optional[str]:
    """Extract founding/establishment year."""
    for pattern in YEAR_PATTERNS:
        m = re.search(pattern, text, re.I)
        if m:
            year = int(m.group(1))
            if 1800 <= year <= 2026:
                return str(year)
    return None


def _extract_address(soup, text: str) -> Optional[str]:
    """Extract physical address from structured data or page content."""
    if not soup:
        return None

    # Strategy 1: Look for address-tagged elements
    addr_el = soup.find(["address"])
    if addr_el:
        addr = addr_el.get_text(separator=" ", strip=True)
        if 10 < len(addr) < 200:
            return addr

    # Strategy 2: Elements with address-like class/id
    for indicator in ADDRESS_INDICATORS:
        for el in soup.find_all(attrs={"class": re.compile(indicator, re.I)}):
            addr = el.get_text(separator=" ", strip=True)
            if 10 < len(addr) < 300:
                return addr
        for el in soup.find_all(attrs={"id": re.compile(indicator, re.I)}):
            addr = el.get_text(separator=" ", strip=True)
            if 10 < len(addr) < 300:
                return addr

    return None


def _extract_industry_from_meta(soup) -> Optional[str]:
    """Extract industry/category from meta tags and OG data."""
    if not soup:
        return None

    # og:type, article:section, keywords
    for tag in soup.find_all("meta"):
        name = (tag.get("name") or tag.get("property") or "").lower()
        content = tag.get("content", "")
        if name == "keywords" and content:
            # Return first 2-3 keywords as industry indicator
            kw = [k.strip() for k in content.split(",")[:3]]
            return ", ".join(kw)
        if name == "article:section" and content:
            return content

    return None


def _generate_email_candidates(first: str, last: str, domain: str) -> List[str]:
    """Generate likely email patterns for a person at a company."""
    if not first or not last or not domain:
        return []

    first = first.lower().strip()
    last = last.lower().strip()
    f = first[0] if first else ""

    candidates = []
    for pattern in EMAIL_PATTERNS:
        email = pattern.format(first=first, last=last, f=f, domain=domain)
        candidates.append(email)

    return candidates


class DeepScraperProvider(EnrichmentProvider):
    """Deep website intelligence — extracts Clay-tier data for free.

    Crawls multiple pages on the target website and extracts:
    - Company info (size, founding year, industry, address)
    - Team members (names + titles from /team, /leadership pages)
    - All contact data (emails, phones, social links)
    - Structured data (JSON-LD Schema.org)
    - Email pattern candidates for decision makers
    """

    name = "deep_scraper"
    capabilities = [
        "email", "phone", "description", "company_size",
        "founding_year", "industry_tags", "address",
        "contact_person", "contact_title", "decision_makers",
        "linkedin_url", "twitter_url", "facebook_url",
    ]
    default_confidence = 0.70

    def __init__(self, max_pages: int = 8, timeout: int = 10):
        self.max_pages = max_pages
        self.timeout = timeout

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()

        if not lead.website:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No website URL available",
                duration_ms=(time.time() - t0) * 1000,
            )

        try:
            from apps.api.services.leadgen.enrichment.website_scraper import normalize_website_url
            import httpx

            base_url = normalize_website_url(lead.website)
            domain = _extract_domain(base_url)

            fields = {}
            all_emails = []
            all_phones = []
            social = {}
            team_members = []
            structured = {}

            headers = {
                "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
                "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
                "Accept-Language": "en-US,en;q=0.9",
                "Accept-Encoding": "gzip, deflate",
            }

            async with httpx.AsyncClient(
                timeout=self.timeout,
                follow_redirects=True,
                headers=headers,
                verify=False,
            ) as client:
                # Crawl up to max_pages
                pages_fetched = 0
                all_text = ""

                for path in PAGES_TO_CRAWL:
                    if pages_fetched >= self.max_pages:
                        break

                    url = urljoin(base_url, path)
                    try:
                        resp = await asyncio.wait_for(
                            client.get(url),
                            timeout=self.timeout,
                        )
                        if resp.status_code != 200:
                            continue
                        # Skip non-HTML responses
                        ct = resp.headers.get("content-type", "")
                        if "text/html" not in ct and "application/xhtml" not in ct:
                            continue
                    except Exception:
                        continue

                    pages_fetched += 1
                    html = resp.text[:300_000]  # Cap at 300KB

                    if not BeautifulSoup:
                        continue

                    soup = BeautifulSoup(html, "html.parser")
                    page_text = soup.get_text(separator=" ", strip=True)
                    all_text += " " + page_text

                    # ── Extract structured data (JSON-LD) ──
                    if path == "/":
                        sd = _extract_structured_data(soup)
                        structured.update(sd)

                    # ── Extract emails ──
                    # From mailto: links
                    for a in soup.find_all("a", href=True):
                        href = a["href"]
                        if href.startswith("mailto:"):
                            email = href.replace("mailto:", "").split("?")[0].strip()
                            if email and "@" in email:
                                all_emails.append(email)
                        # Phone from tel: links
                        elif href.startswith("tel:"):
                            phone = href.replace("tel:", "").strip()
                            phone = re.sub(r'[^\d+\-\s()]', '', phone)
                            if phone and len(re.sub(r'\D', '', phone)) >= 7:
                                all_phones.append(phone)

                    # Regex emails from page text
                    email_matches = re.findall(
                        r'[\w.\-+]+@[\w.\-]+\.(?:com|org|net|io|co|in|ai|dev|tech|biz|info|us|uk|eu)',
                        page_text, re.I,
                    )
                    for em in email_matches:
                        em_lower = em.lower()
                        if not any(x in em_lower for x in [
                            'example.com', 'domain.com', 'email.com',
                            '.png', '.jpg', '.css', '.js', 'wixpress',
                            'sentry.io', 'webpack',
                        ]):
                            all_emails.append(em)

                    # Regex phones from page text
                    phone_patterns = [
                        r'\+?(?:91|1|44|61|49|33|971)[\s\-.]?\(?\d{2,4}\)?[\s\-.]?\d{3,4}[\s\-.]?\d{3,4}',
                        r'\b\d{3}[\s\-.]?\d{3}[\s\-.]?\d{4}\b',
                        r'1800[\s\-.]?\d{2,3}[\s\-.]?\d{4,6}',
                    ]
                    for pat in phone_patterns:
                        for m in re.finditer(pat, page_text):
                            phone = m.group().strip()
                            digits = re.sub(r'\D', '', phone)
                            if 7 <= len(digits) <= 15:
                                all_phones.append(phone)

                    # ── Extract social links ──
                    for a in soup.find_all("a", href=True):
                        href = a["href"]
                        hl = href.lower()
                        if "linkedin.com/company" in hl or "linkedin.com/in/" in hl:
                            social.setdefault("linkedin", href)
                        elif ("twitter.com/" in hl or "x.com/" in hl) and "/intent/" not in hl:
                            social.setdefault("twitter", href)
                        elif "facebook.com/" in hl and "facebook.com/sharer" not in hl:
                            social.setdefault("facebook", href)
                        elif "instagram.com/" in hl and "instagram.com/p/" not in hl:
                            social.setdefault("instagram", href)

                    # ── Extract team members from team/about/leadership pages ──
                    if any(p in path for p in ["/team", "/leadership", "/people", "/management", "/about"]):
                        members = _extract_team_members(soup, page_text)
                        team_members.extend(members)

                    # ── Description from meta ──
                    if path == "/":
                        meta_desc = soup.find("meta", attrs={"name": "description"})
                        if meta_desc and meta_desc.get("content"):
                            fields.setdefault("description", meta_desc["content"][:500])
                        og_desc = soup.find("meta", attrs={"property": "og:description"})
                        if og_desc and og_desc.get("content"):
                            fields.setdefault("description", og_desc["content"][:500])

                        # Industry from meta
                        industry = _extract_industry_from_meta(soup)
                        if industry:
                            fields["industry_tags"] = industry

            # ── Post-processing: merge all extracted data ──

            # Deduplicate emails and phones
            all_emails = list(dict.fromkeys(e.lower() for e in all_emails))
            all_phones = list(dict.fromkeys(all_phones))

            # Best email: prefer structured data > generic contact > first found
            if structured.get("email"):
                fields["email"] = structured["email"]
            elif all_emails:
                # Rank: personal/role emails > generic
                ranked = sorted(all_emails, key=lambda e: (
                    0 if any(p in e for p in ["ceo@", "founder@", "director@"]) else
                    1 if any(p in e for p in ["info@", "contact@", "hello@", "sales@"]) else
                    2 if any(p in e for p in ["support@", "help@", "admin@"]) else 3
                ))
                fields["email"] = ranked[0].rstrip('.')

            # Phone
            if structured.get("phone"):
                fields["phone"] = structured["phone"]
            elif all_phones:
                fields["phone"] = all_phones[0]

            # Company size
            if structured.get("employee_count"):
                emp_val = structured["employee_count"]
                # Validate: reject unreasonable counts (Schema.org sometimes has huge numbers)
                try:
                    if isinstance(emp_val, (int, float)):
                        count = int(emp_val)
                    elif isinstance(emp_val, str) and emp_val.replace(',', '').isdigit():
                        count = int(emp_val.replace(',', ''))
                    else:
                        count = None
                    if count and count <= 500_000:
                        fields["company_size"] = str(count)
                except (ValueError, TypeError):
                    fields["company_size"] = str(emp_val)[:20]
            else:
                size = _extract_company_size(all_text)
                if size:
                    fields["company_size"] = size

            # Founding year
            if structured.get("founding_year"):
                fields["founding_year"] = structured["founding_year"]
            else:
                year = _extract_founding_year(all_text)
                if year:
                    fields["founding_year"] = year

            # Address
            if structured.get("address"):
                fields["address"] = structured["address"]

            # Description
            if structured.get("description"):
                fields.setdefault("description", structured["description"])

            # Industry
            if structured.get("industry"):
                fields["industry_tags"] = structured["industry"]

            # Social links
            social.update({k: v for k, v in structured.items() if k in ("linkedin", "twitter", "facebook", "instagram")})
            if social.get("linkedin"):
                fields["linkedin_url"] = social["linkedin"]
            if social.get("twitter"):
                fields["twitter_url"] = social["twitter"]
            if social.get("facebook"):
                fields["facebook_url"] = social["facebook"]

            # Team members → decision_makers + primary contact
            # Deduplicate by name
            seen = set()
            unique_team = []
            for m in team_members:
                if m["name"].lower() not in seen:
                    seen.add(m["name"].lower())
                    unique_team.append(m)

            if unique_team:
                # Sort by title seniority
                def _rank(p):
                    t = p.get("title", "").lower()
                    if any(x in t for x in ["ceo", "founder", "owner", "md"]):
                        return 0
                    if any(x in t for x in ["cto", "coo", "cfo", "cmo", "cio"]):
                        return 1
                    if any(x in t for x in ["vp", "vice president", "director"]):
                        return 2
                    if any(x in t for x in ["head", "lead", "manager"]):
                        return 3
                    return 4

                unique_team.sort(key=_rank)
                fields["decision_makers"] = json.dumps(unique_team[:5])
                fields["contact_person"] = unique_team[0]["name"]
                fields["contact_title"] = unique_team[0]["title"]

                # Try to generate email for the primary contact
                if domain and "email" not in fields:
                    name_parts = unique_team[0]["name"].split()
                    if len(name_parts) >= 2:
                        candidates = _generate_email_candidates(
                            name_parts[0], name_parts[-1], domain
                        )
                        if candidates:
                            # Store the most common pattern as candidate
                            fields["email"] = candidates[0]
                            fields.setdefault("email_confidence", "pattern_guess")

            if fields:
                return EnrichmentResult(
                    provider=self.name, success=True,
                    fields=fields,
                    confidence=self.default_confidence,
                    duration_ms=(time.time() - t0) * 1000,
                )

            return EnrichmentResult(
                provider=self.name, success=False,
                error=f"No data extracted from {pages_fetched} pages",
                duration_ms=(time.time() - t0) * 1000,
            )

        except Exception as e:
            logger.warning(f"Deep scraper error for {lead.website}: {e}")
            return EnrichmentResult(
                provider=self.name, success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
