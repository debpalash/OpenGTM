"""
JSON-LD / OpenGraph Firmographics Provider — free structured company data.

Most directory and company pages embed schema.org data in
`<script type="application/ld+json">` blocks (Organization / LocalBusiness /
Corporation) plus OpenGraph meta tags. This provider lifts that *already-present*
structured data — company name, address, phone, email, founding date, logo, and
social `sameAs` links — with zero LLM cost and no brittle HTML parsing.

Inspired by scrapinghub/extruct (cloned in research/enrichment-waterfall/extruct).
Kept dependency-free (stdlib json + regex) so it adds no install footprint.

Capabilities: company, address, phone, email, founded_year, linkedin_url,
twitter_url, facebook_url, description.
"""

import html as _html
import json
import logging
import re
import time
from typing import Any, Dict, List, Optional

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.jsonld")

_ORG_TYPES = {
    "organization", "corporation", "localbusiness", "company",
    "ngo", "educationalorganization", "governmentorganization",
    "professionalservice", "store", "ITService".lower(), "softwareapplication",
}

_SOCIAL_MAP = {
    "linkedin.com": "linkedin_url",
    "twitter.com": "twitter_url",
    "x.com": "twitter_url",
    "facebook.com": "facebook_url",
}

_JSONLD_RE = re.compile(
    r'<script[^>]+type=["\']application/ld\+json["\'][^>]*>(.*?)</script>',
    re.I | re.S,
)
_OG_RE = re.compile(
    r'<meta\s+[^>]*(?:property|name)=["\']og:([\w:]+)["\'][^>]*content=["\']([^"\']*)["\']',
    re.I,
)


def _iter_jsonld(html_text: str):
    """Yield every JSON object found in ld+json blocks (handles @graph + arrays)."""
    for m in _JSONLD_RE.finditer(html_text or ""):
        raw = m.group(1).strip()
        if not raw:
            continue
        try:
            data = json.loads(raw)
        except (json.JSONDecodeError, ValueError):
            continue
        stack = [data]
        while stack:
            node = stack.pop()
            if isinstance(node, list):
                stack.extend(node)
            elif isinstance(node, dict):
                if "@graph" in node and isinstance(node["@graph"], list):
                    stack.extend(node["@graph"])
                yield node


def _types_of(node: Dict) -> List[str]:
    t = node.get("@type", "")
    if isinstance(t, list):
        return [str(x).lower() for x in t]
    return [str(t).lower()]


def _flatten_address(addr: Any) -> str:
    if isinstance(addr, str):
        return addr.strip()
    if isinstance(addr, list) and addr:
        return _flatten_address(addr[0])
    if isinstance(addr, dict):
        parts = [
            addr.get("streetAddress"), addr.get("addressLocality"),
            addr.get("addressRegion"), addr.get("postalCode"),
            addr.get("addressCountry") if isinstance(addr.get("addressCountry"), str) else None,
        ]
        return ", ".join(str(p).strip() for p in parts if p)
    return ""


def _locality(addr: Any) -> Dict[str, str]:
    if isinstance(addr, list) and addr:
        addr = addr[0]
    if isinstance(addr, dict):
        return {
            "city": str(addr.get("addressLocality", "") or "").strip(),
            "state": str(addr.get("addressRegion", "") or "").strip(),
        }
    return {}


def _year(value: Any) -> str:
    m = re.search(r"(\d{4})", str(value or ""))
    return m.group(1) if m else ""


def extract_firmographics(html_text: str) -> Dict[str, str]:
    """Pull company fields from JSON-LD (preferred) + OpenGraph (fallback)."""
    fields: Dict[str, str] = {}
    socials: Dict[str, str] = {}

    # 1) JSON-LD organization nodes (highest quality).
    org = None
    for node in _iter_jsonld(html_text):
        if _ORG_TYPES & set(_types_of(node)):
            org = node
            break
    if org:
        name = org.get("name") or org.get("legalName")
        if isinstance(name, str) and name.strip():
            fields["company"] = name.strip()

        for tel_key in ("telephone", "phone"):
            tel = org.get(tel_key)
            if isinstance(tel, str) and tel.strip():
                fields["phone"] = tel.strip()
                break
        cp = org.get("contactPoint")
        if "phone" not in fields and isinstance(cp, (list, dict)):
            cp0 = cp[0] if isinstance(cp, list) and cp else cp
            if isinstance(cp0, dict) and cp0.get("telephone"):
                fields["phone"] = str(cp0["telephone"]).strip()

        email = org.get("email")
        if isinstance(email, str) and "@" in email:
            fields["email"] = email.strip().lstrip("mailto:")

        addr_str = _flatten_address(org.get("address"))
        if addr_str:
            fields["address"] = addr_str
        loc = _locality(org.get("address"))
        if loc.get("city"):
            fields["city"] = loc["city"]
        if loc.get("state"):
            fields["state"] = loc["state"]

        fy = _year(org.get("foundingDate") or org.get("foundingdate"))
        if fy:
            fields["founded_year"] = fy

        desc = org.get("description")
        if isinstance(desc, str) and desc.strip():
            fields["description"] = _html.unescape(desc.strip())[:500]

        same = org.get("sameAs")
        if isinstance(same, str):
            same = [same]
        if isinstance(same, list):
            for url in same:
                if not isinstance(url, str):
                    continue
                for dom, key in _SOCIAL_MAP.items():
                    if dom in url and key not in socials:
                        socials[key] = url.strip()

    # 2) OpenGraph fallback for anything still missing.
    og = {k.lower(): _html.unescape(v) for k, v in _OG_RE.findall(html_text or "")}
    if "company" not in fields and og.get("site_name"):
        fields["company"] = og["site_name"].strip()
    if "description" not in fields and og.get("description"):
        fields["description"] = og["description"].strip()[:500]

    fields.update(socials)
    return {k: v for k, v in fields.items() if v}


class JsonLdFirmographicsProvider(EnrichmentProvider):
    """Extract embedded structured company data from a lead's website."""

    name = "jsonld_firmographics"
    capabilities = [
        "company", "address", "phone", "email", "city", "state",
        "founded_year", "founding_year",  # codebase uses both names for the field
        "linkedin_url", "twitter_url", "facebook_url", "description",
    ]
    default_confidence = 0.85  # schema.org data is publisher-declared → high trust

    def __init__(self, timeout: float = 15.0):
        self.timeout = timeout

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()
        if not lead.website:
            return EnrichmentResult(
                provider=self.name, success=False, error="no_website",
                duration_ms=(time.time() - t0) * 1000,
            )
        try:
            import httpx
            from apps.api.services.leadgen.enrichment.website_scraper import normalize_website_url

            url = normalize_website_url(lead.website)
            async with httpx.AsyncClient(
                timeout=self.timeout, follow_redirects=True, verify=False,
                headers={
                    "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
                                  "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
                    "Accept": "text/html,application/xhtml+xml",
                },
            ) as client:
                resp = await client.get(url)
                html_text = resp.text[:300_000]

            fields = extract_firmographics(html_text)
            # Don't overwrite an existing, more-specific company name with a generic one.
            if lead.company and "company" in fields:
                fields.pop("company", None)
            # The codebase targets this field under both names — expose both.
            if "founded_year" in fields:
                fields["founding_year"] = fields["founded_year"]

            if fields:
                return EnrichmentResult(
                    provider=self.name, success=True, fields=fields,
                    confidence=self.default_confidence,
                    duration_ms=(time.time() - t0) * 1000,
                )
            return EnrichmentResult(
                provider=self.name, success=False, error="no_structured_data",
                duration_ms=(time.time() - t0) * 1000,
            )
        except Exception as e:
            logger.warning("jsonld firmographics error for %s: %s", lead.website, e)
            return EnrichmentResult(
                provider=self.name, success=False, error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
