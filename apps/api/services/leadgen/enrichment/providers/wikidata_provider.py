"""Wikidata firmographics provider — keyless, website-independent.

Resolves a company by NAME against Wikidata's free, keyless API and extracts
firmographics directly from the knowledge graph, so it works even when the
company's own website is dead. Good coverage for companies notable enough to
have a Wikidata item (large/established firms); thin for tiny SMBs — sits late
in the waterfall as a free fallback.

Fills: description, website, industry_tags, founded_year, company_size (bucketed
from employee count), address (HQ), linkedin_url, twitter_url, facebook_url.

API: https://www.wikidata.org/w/api.php  (wbsearchentities + wbgetentities)
"""
import asyncio
import logging
from typing import Dict, List, Optional

import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("enrichment.wikidata")

_API = "https://www.wikidata.org/w/api.php"
_UA = "Mozilla/5.0 (compatible; yupcha-enrichment/1.0; +https://yupcha.com)"

# Wikidata property IDs we read off a company item.
_P_WEBSITE = "P856"
_P_HQ = "P159"          # headquarters location (→ QID, resolve label)
_P_INDUSTRY = "P452"    # industry (→ QID, resolve label)
_P_INCEPTION = "P571"   # founding date
_P_EMPLOYEES = "P1128"  # number of employees
_P_TWITTER = "P2002"    # Twitter username
_P_FACEBOOK = "P2013"   # Facebook ID
_P_LINKEDIN = "P4264"   # LinkedIn company ID

# Descriptions that signal the item is actually a company/org (not a person,
# award, product, etc.) — used to disambiguate name search hits.
_ORG_HINTS = (
    "company", "corporation", "business", "enterprise", "firm", "organization",
    "organisation", "manufacturer", "consultancy", "consulting", "agency",
    "services", "technology", "software", "staffing", "recruitment", "holding",
    "startup", "subsidiary", "conglomerate", "vendor", "provider",
)
_NEG_HINTS = ("award", "prize", "human settlement", "given name", "surname", "family name")


class WikidataProvider(EnrichmentProvider):
    name = "wikidata"
    capabilities = [
        "description", "website", "industry_tags", "founded_year",
        "company_size", "address", "linkedin_url", "twitter_url", "facebook_url",
    ]
    default_confidence = 0.6
    requires_api_key = False
    source_license = "CC0-1.0"  # Wikidata is CC0 (per-fact provenance)

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        company = (lead.company or "").strip()
        if not company:
            return EnrichmentResult(provider=self.name, success=False, error="no_company")
        try:
            async with httpx.AsyncClient(timeout=12, headers={"User-Agent": _UA}) as client:
                qid = await self._resolve_entity(client, company)
                if not qid:
                    return EnrichmentResult(provider=self.name, success=False, error="not_on_wikidata")
                fields = await self._extract(client, qid)
            fields = {k: v for k, v in fields.items() if v}
            if not fields:
                return EnrichmentResult(provider=self.name, success=False, error="no_fields")
            return EnrichmentResult(
                provider=self.name, success=True, fields=fields, confidence=self.default_confidence
            )
        except Exception as e:
            logger.debug(f"wikidata enrich failed for {company!r}: {e}")
            return EnrichmentResult(provider=self.name, success=False, error=str(e)[:120])

    async def _resolve_entity(self, client: httpx.AsyncClient, company: str) -> Optional[str]:
        """Search by name and pick the best company-like item."""
        r = await client.get(_API, params={
            "action": "wbsearchentities", "search": company, "language": "en",
            "type": "item", "format": "json", "limit": 7,
        })
        hits = r.json().get("search", [])
        if not hits:
            return None
        best = None
        for h in hits:
            desc = (h.get("description") or "").lower()
            if any(n in desc for n in _NEG_HINTS):
                continue
            if any(o in desc for o in _ORG_HINTS):
                return h["id"]  # confident company match
            if best is None:
                best = h["id"]
        return best  # fall back to the top non-negative hit

    async def _extract(self, client: httpx.AsyncClient, qid: str) -> Dict[str, str]:
        r = await client.get(_API, params={
            "action": "wbgetentities", "ids": qid,
            "props": "claims|descriptions", "languages": "en", "format": "json",
        })
        ent = r.json().get("entities", {}).get(qid, {})
        claims = ent.get("claims", {})
        out: Dict[str, str] = {}

        desc = (ent.get("descriptions", {}).get("en", {}) or {}).get("value", "")
        if desc:
            out["description"] = desc

        out["website"] = _first_string(claims.get(_P_WEBSITE))

        twitter = _first_string(claims.get(_P_TWITTER))
        if twitter:
            out["twitter_url"] = f"https://twitter.com/{twitter.lstrip('@')}"
        fb = _first_string(claims.get(_P_FACEBOOK))
        if fb:
            out["facebook_url"] = f"https://www.facebook.com/{fb}"
        li = _first_string(claims.get(_P_LINKEDIN))
        if li:
            out["linkedin_url"] = f"https://www.linkedin.com/company/{li}"

        year = _first_time_year(claims.get(_P_INCEPTION))
        if year:
            out["founded_year"] = year

        emp = _first_quantity(claims.get(_P_EMPLOYEES))
        if emp:
            out["company_size"] = _bucket_size(emp)

        # Industry + HQ are entity references — resolve their labels in one batch.
        ref_qids = []
        ind_q = _first_entity_id(claims.get(_P_INDUSTRY))
        hq_q = _first_entity_id(claims.get(_P_HQ))
        ref_qids = [q for q in (ind_q, hq_q) if q]
        if ref_qids:
            labels = await self._labels(client, ref_qids)
            if ind_q and labels.get(ind_q):
                out["industry_tags"] = labels[ind_q]
            if hq_q and labels.get(hq_q):
                out["address"] = labels[hq_q]
        return out

    async def _labels(self, client: httpx.AsyncClient, qids: List[str]) -> Dict[str, str]:
        r = await client.get(_API, params={
            "action": "wbgetentities", "ids": "|".join(qids),
            "props": "labels", "languages": "en", "format": "json",
        })
        ents = r.json().get("entities", {})
        return {q: (ents.get(q, {}).get("labels", {}).get("en", {}) or {}).get("value", "") for q in qids}


# ── claim value extractors (Wikidata's nested snak structure) ──

def _datavalue(claim_list) -> Optional[dict]:
    if not claim_list:
        return None
    try:
        return claim_list[0]["mainsnak"]["datavalue"]["value"]
    except (KeyError, IndexError, TypeError):
        return None


def _first_string(claim_list) -> str:
    v = _datavalue(claim_list)
    return v if isinstance(v, str) else ""


def _first_entity_id(claim_list) -> str:
    v = _datavalue(claim_list)
    if isinstance(v, dict) and v.get("id"):
        return v["id"]
    return ""


def _first_time_year(claim_list) -> str:
    v = _datavalue(claim_list)
    if isinstance(v, dict) and isinstance(v.get("time"), str):
        # format: +1981-01-01T00:00:00Z
        t = v["time"].lstrip("+")
        return t[:4] if t[:4].isdigit() else ""
    return ""


def _first_quantity(claim_list) -> Optional[int]:
    v = _datavalue(claim_list)
    if isinstance(v, dict) and v.get("amount"):
        try:
            return int(float(str(v["amount"]).lstrip("+")))
        except ValueError:
            return None
    return None


def _bucket_size(n: int) -> str:
    if n <= 50:
        return "1-50"
    if n <= 200:
        return "51-200"
    if n <= 500:
        return "201-500"
    if n <= 1000:
        return "501-1000"
    return "1000+"
