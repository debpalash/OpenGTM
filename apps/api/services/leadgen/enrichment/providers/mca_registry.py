"""MCA registry provider — authoritative Indian company firmographics, no website.

Source: India's Ministry of Corporate Affairs "RoC-wise Company Master Data"
published on data.gov.in (~3.67M registered companies). Works from the company
NAME — exactly the case where the target's own website is dead.

Resolution pipeline (robust to informal lead names like "CIEL HR"):
  1. Find the company's CIN via our SearXNG search backend (CINs appear on
     registry pages indexed by Bing/Qwant). CIN → exact, reliable MCA lookup.
  2. Fallback: exact-name match against MCA with normalized name + legal suffixes
     ("PRIVATE LIMITED" / "LIMITED" / "LLP").

Fills: address (registered office), founded_year (incorporation), industry_tags,
description (synthesized firmographic summary incl. CIN + status). MCA status is
NOT written to Lead.status (that field is the sales pipeline state).

API: https://api.data.gov.in/resource/<mca-resource>?api-key=...&filters[CIN]=...
Keyless by default (public sample key); set DATA_GOV_IN_KEY for higher limits.
"""
import asyncio
import logging
import os
import re
import time as _time
from typing import Dict, List, Optional

import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("enrichment.mca")

# "RoC-wise Company Master Data" (Ministry of Corporate Affairs) on data.gov.in.
_RESOURCE = os.getenv("MCA_RESOURCE_ID", "4dbe5667-7b6b-41d7-82af-211562424d9a")
_API = f"https://api.data.gov.in/resource/{_RESOURCE}"
# data.gov.in public sample key works out of the box (rate-limited); override for prod.
_KEY = os.getenv("DATA_GOV_IN_KEY", "579b464db66ec23bdd000001cdd3946e44ce4aad7209ff7b23ac571b")

# CIN = 1 letter (Listing) + 5-digit industry + 2-letter state + 4-digit year
#       + 3-letter ownership + 6-digit registration number  (21 chars).
_CIN_RE = re.compile(r"\b[LUF]\d{5}[A-Z]{2}\d{4}[A-Z]{3}\d{6}\b")


class MCARegistryProvider(EnrichmentProvider):
    name = "mca_registry"
    capabilities = ["address", "founded_year", "industry_tags", "description"]
    default_confidence = 0.85  # government registry data; lowered for name-only matches
    requires_api_key = False

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        company = (lead.company or "").strip()
        if not company:
            return EnrichmentResult(provider=self.name, success=False, error="no_company")
        try:
            record, conf = await self._lookup(company, lead.city or "")
            if not record:
                return EnrichmentResult(provider=self.name, success=False, error="not_in_mca")
            fields = self._to_fields(record)
            if not fields:
                return EnrichmentResult(provider=self.name, success=False, error="no_fields")
            return EnrichmentResult(provider=self.name, success=True, fields=fields, confidence=conf)
        except Exception as e:
            logger.debug(f"mca enrich failed for {company!r}: {e}")
            return EnrichmentResult(provider=self.name, success=False, error=str(e)[:120])

    async def _lookup(self, company: str, city: str):
        """Return (record, confidence), bounded to stay inside the per-provider
        time budget (the killable worker kills us at ~10s).

        Fast path: exact match on the normalized name + legal suffixes — parallel
        ~0.5s registry calls. Only if that misses, and only if we still have time,
        do we pay for the slower CIN-via-web-search path.
        """
        deadline = _time.monotonic() + 7.0  # leave headroom under the 10s kill

        # 1) Exact registered-name match — sequential with short-circuit. Most
        #    leads hit an early variant (~0.7s each), and this avoids the parallel
        #    burst that data.gov.in throttles on the shared key.
        for variant in _name_variants(company):
            if _time.monotonic() >= deadline:
                break
            rec = await self._mca_query("CompanyName", variant)
            if rec:
                return rec, 0.8

        # 2) CIN-via-search fallback — only if there's budget left.
        if _time.monotonic() < deadline:
            cin = await self._find_cin(company, city)
            if cin:
                rec = await self._mca_query("CIN", cin)
                if rec:
                    return rec, 0.85
        return None, 0.0

    async def _find_cin(self, company: str, city: str) -> Optional[str]:
        """Search the web for the company's CIN via our SearXNG-backed shim."""
        from apps.api.services.leadgen.enrichment.web_search import DDGS

        def _search(query: str) -> List[Dict]:
            with DDGS() as d:
                return d.text(query, max_results=8)

        # One registry-biased query — CINs surface on zaubacorp/tofler snippets.
        q = f"{company} {city} CIN zaubacorp tofler".strip()
        try:
            hits = await asyncio.wait_for(asyncio.to_thread(_search, q), timeout=5.0)
        except Exception:
            return None
        for h in hits:
            blob = f"{h.get('title','')} {h.get('body','')} {h.get('href','')}"
            m = _CIN_RE.search(blob.upper())
            if m:
                return m.group(0)
        return None

    async def _mca_query(self, field: str, value: str) -> Optional[dict]:
        params = {
            "api-key": _KEY, "format": "json", "limit": 1,
            f"filters[{field}]": value,
        }
        # data.gov.in hangs on browser-like User-Agents (anti-bot) but answers a
        # plain curl-style UA in ~0.5s — without this header httpx times out.
        # Fail FAST (4s, no retry): the shared sample key can be throttled, and a
        # slow call must not eat the per-provider time budget. Set a real free
        # DATA_GOV_IN_KEY for consistent sub-second responses in production.
        headers = {"User-Agent": "curl/8.4.0", "Accept": "application/json"}
        try:
            async with httpx.AsyncClient(timeout=httpx.Timeout(4.0, connect=4.0),
                                         headers=headers) as client:
                r = await client.get(_API, params=params)
            if r.status_code != 200:
                return None
            recs = (r.json() or {}).get("records") or []
            return recs[0] if recs else None
        except (httpx.TimeoutException, httpx.TransportError):
            return None

    def _to_fields(self, r: dict) -> Dict[str, str]:
        out: Dict[str, str] = {}
        addr = _clean(r.get("Registered_Office_Address"))
        if addr:
            out["address"] = addr
        year = _year(r.get("CompanyRegistrationdate_date"))
        if year:
            out["founded_year"] = year
        ind = _clean(r.get("CompanyIndustrialClassification"))
        if ind:
            out["industry_tags"] = ind

        # Synthesized, human-readable firmographic summary for the description cell.
        bits = []
        status = _clean(r.get("CompanyStatus"))
        klass = _clean(r.get("CompanyClass"))
        cin = _clean(r.get("CIN"))
        roc = _clean(r.get("CompanyROCcode"))
        name = _clean(r.get("CompanyName"))
        if name:
            lead_in = name.title()
            parts = []
            if klass and status:
                parts.append(f"{status} {klass.lower()} company")
            elif status:
                parts.append(f"{status} company")
            if year:
                parts.append(f"incorporated {year}")
            if roc:
                parts.append(f"registered under {roc}")
            if ind:
                parts.append(f"industry: {ind.lower()}")
            summary = f"{lead_in} — " + ", ".join(parts) if parts else lead_in
            if cin:
                summary += f" (CIN {cin})"
            out["description"] = summary
        return out


def _clean(v) -> str:
    if not v:
        return ""
    s = re.sub(r"\s+", " ", str(v)).strip().strip(",")
    return s if s and s.upper() not in ("NA", "N/A", "NIL", "-") else ""


def _year(v) -> str:
    if not v:
        return ""
    m = re.search(r"(19|20)\d{2}", str(v))
    return m.group(0) if m else ""


def _name_variants(company: str) -> List[str]:
    """Normalized uppercase name + common Indian legal-suffix variants for exact
    MCA matching. A wrong suffix simply returns no rows (no false positives), so
    trying the common ones only adds coverage. Order = most-likely first."""
    base = re.sub(r"[^\w\s&]", " ", company).upper()
    base = re.sub(r"\s+", " ", base).strip()
    # strip any suffix already present so we can re-add canonical forms
    base = re.sub(r"\s+(PVT|PRIVATE)?\s*(LTD|LIMITED|LLP|INC|CORP)\.?$", "", base).strip()
    if not base:
        return []
    # Indian IT/staffing firms overwhelmingly register as "<NAME> [WORD] PRIVATE
    # LIMITED" where WORD ∈ {SERVICES, TECHNOLOGIES, SOLUTIONS, CONSULTING, ...}.
    candidates = [
        f"{base} PRIVATE LIMITED",
        f"{base} LIMITED",
        f"{base} SERVICES PRIVATE LIMITED",
        f"{base} TECHNOLOGIES PRIVATE LIMITED",
        f"{base} SOLUTIONS PRIVATE LIMITED",
        f"{base} CONSULTING PRIVATE LIMITED",
        f"{base} LLP",
        base,
    ]
    seen, out = set(), []
    for v in candidates:
        if v not in seen:
            seen.add(v)
            out.append(v)
    return out
