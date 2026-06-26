"""GLEIF provider — keyless, CC0 legal-entity + corporate-hierarchy registry.

The Global Legal Entity Identifier Foundation (GLEIF) publishes the worldwide
LEI register: an authoritative, openly-licensed (CC0) directory of legal
entities and the parent/child ownership links between them. Banks, regulators
and Clay/ZoomInfo-tier vendors all resell this exact data. We tap it directly,
keyless and free.

Resolution pipeline (works from the company NAME):
  1. Fuzzy-match the name → candidate LEI via the fuzzycompletions endpoint
     (field=entity.legalName). This tolerates the small spelling/suffix drift
     between a lead's name and the registered legal name.
  2. Fallback: exact legalName filter on lei-records (catches names the fuzzy
     index ranks poorly, and verifies the fuzzy candidate's full record).
  3. Pull the candidate's full LEI record → canonical legal name, legal-address
     country/city/region, entity status, registration authority, jurisdiction.
  4. Follow the direct-parent and ultimate-parent relationship links to map the
     corporate hierarchy (parent LEI + parent legal name).

Emits (all flat scalars per the EnrichmentResult data contract, except the
designated JSON-string field `corporate_hierarchy`):
  lei, legal_name, country, city, region, entity_status,
  registration_authority, jurisdiction, parent_lei, ultimate_parent_lei,
  ultimate_parent_name, corporate_hierarchy (JSON string),
  and gleif_source / gleif_license for per-fact CC0 provenance.

Skeptic constraints (mandatory):
  * No API key, ever — the LEI register is public and CC0-licensed.
  * Self-rate-limited (process-wide async throttle) to stay well under GLEIF's
    documented public ceiling (~60 req/min/IP for unauthenticated callers).
  * 404 / no-match / parse errors return an empty (success=False) result and
    NEVER raise — enrichment must degrade gracefully.

Refs:
  https://www.gleif.org/en/lei-data/gleif-api
  https://www.gleif.org/en/about-lei/common-data-file-format  (CC0 license)
"""
import asyncio
import json
import logging
import re
import time as _time
from typing import Dict, List, Optional

import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("enrichment.gleif")

# Keyless public endpoints (no API key, ever — CC0 open register).
_BASE = "https://api.gleif.org/api/v1"
_FUZZY_URL = f"{_BASE}/fuzzycompletions"
_RECORDS_URL = f"{_BASE}/lei-records"

# JSON:API requires this Accept header on the GLEIF endpoints.
_HEADERS = {
    "Accept": "application/vnd.api+json",
    "User-Agent": "Yupcha Enrichment admin@yupcha.com",
}
_HTTP_TIMEOUT = httpx.Timeout(8.0, connect=5.0)

# CC0 provenance tags surfaced on every successful result.
_SOURCE = "gleif"
_LICENSE = "CC0-1.0"

# Legal-suffix / punctuation noise stripped before name comparison.
_SUFFIX_RE = re.compile(
    r"\b(inc|incorporated|corp|corporation|llc|l\.l\.c|llp|ltd|limited|co|company|"
    r"plc|lp|gmbh|ag|sa|nv|bv|oy|ab|holdings|group|the)\b",
    re.IGNORECASE,
)
_NON_ALNUM_RE = re.compile(r"[^a-z0-9 ]+")


# ── Self rate-limiter (process-wide) ────────────────────────────────────────

class _RateLimiter:
    """Async token-spacer enforcing a minimum interval between requests across
    ALL concurrent enrichments in this process. GLEIF's public (unauthenticated)
    ceiling is ~60 req/min; we target ~1 req/s (a generous safety margin)."""

    def __init__(self, max_per_sec: float = 1.0):
        self._min_interval = 1.0 / max_per_sec
        self._lock = asyncio.Lock()
        self._last = 0.0

    async def acquire(self) -> None:
        async with self._lock:
            now = _time.monotonic()
            wait = self._min_interval - (now - self._last)
            if wait > 0:
                await asyncio.sleep(wait)
            self._last = _time.monotonic()


# Module-level singleton so the limit is global, not per-instance.
_LIMITER = _RateLimiter(max_per_sec=1.0)


class GleifProvider(EnrichmentProvider):
    name = "gleif"
    capabilities = [
        "lei", "legal_name", "country", "city", "region",
        "entity_status", "registration_authority", "jurisdiction",
        "parent_lei", "ultimate_parent_lei", "ultimate_parent_name",
        "corporate_hierarchy",
    ]
    # Authoritative CC0 registry: high trust when a confident name match is
    # found. Fuzzy name->LEI carries some false-positive risk, so it sits as a
    # free, high-quality registry leg rather than a 0.95.
    default_confidence = 0.85
    requires_api_key = False
    free_tier_limit = 0       # 0 = unlimited (public register)
    cost_per_lookup = 0.0     # CC0 open data — always free
    source_license = "CC0-1.0"  # LEI register is explicitly CC0 (per-fact provenance)

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        company = (lead.company or "").strip()
        if not company:
            return EnrichmentResult(provider=self.name, success=False, error="no_company")
        try:
            async with httpx.AsyncClient(timeout=_HTTP_TIMEOUT, headers=_HEADERS,
                                         follow_redirects=True) as client:
                record = await self._resolve_record(client, company)
                if not record:
                    return EnrichmentResult(provider=self.name, success=False,
                                            error="no_lei_match")
                fields = _record_to_fields(record)
                # Corporate hierarchy: follow the parent relationship links.
                hierarchy = await self._fetch_hierarchy(client, record)
                fields.update(hierarchy)
        except Exception as e:  # never crash the waterfall
            logger.debug(f"gleif enrich failed for {company!r}: {e}")
            return EnrichmentResult(provider=self.name, success=False, error=str(e)[:120])

        # Drop empties; require an LEI to count as a hit.
        fields = {k: v for k, v in fields.items() if v not in (None, "", [])}
        if "lei" not in fields:
            return EnrichmentResult(provider=self.name, success=False, error="no_lei_match")

        # CC0 provenance tags (per-fact source/license).
        fields["gleif_source"] = _SOURCE
        fields["gleif_license"] = _LICENSE
        return EnrichmentResult(provider=self.name, success=True, fields=fields,
                                confidence=self.default_confidence)

    # ── LEI resolution ──────────────────────────────────────────────────────

    async def _resolve_record(self, client: httpx.AsyncClient,
                              company: str) -> Optional[dict]:
        """Resolve a company name to a full LEI record dict.

        Path 1: fuzzycompletions on entity.legalName → candidate LEI, then fetch
                the full record by LEI (fuzzy only returns the id).
        Path 2: exact legalName filter on lei-records (returns full records)."""
        lei = await self._fuzzy_lei(client, company)
        if lei:
            rec = await self._record_by_lei(client, lei)
            if rec:
                return rec
        return await self._record_by_exact_name(client, company)

    async def _fuzzy_lei(self, client: httpx.AsyncClient,
                         company: str) -> Optional[str]:
        params = {"field": "entity.legalName", "q": company}
        data = await self._get_json(client, _FUZZY_URL, params=params)
        rows = (data or {}).get("data") or []
        target = _norm_name(company)
        best: Optional[str] = None
        for row in rows:
            lei = (((row.get("relationships") or {}).get("lei-records") or {})
                   .get("data") or {}).get("id")
            if not lei:
                continue
            value = ((row.get("attributes") or {}).get("value")) or ""
            # Prefer an exact normalized-name completion; else keep the top hit.
            if _norm_name(value) == target:
                return lei
            if best is None:
                best = lei
        return best

    async def _record_by_lei(self, client: httpx.AsyncClient,
                             lei: str) -> Optional[dict]:
        data = await self._get_json(client, f"{_RECORDS_URL}/{lei}")
        return _first_record(data)

    async def _record_by_exact_name(self, client: httpx.AsyncClient,
                                    company: str) -> Optional[dict]:
        params = {"filter[entity.legalName]": company, "page[size]": "1"}
        data = await self._get_json(client, _RECORDS_URL, params=params)
        return _first_record(data)

    # ── Corporate hierarchy ─────────────────────────────────────────────────

    async def _fetch_hierarchy(self, client: httpx.AsyncClient,
                               record: dict) -> Dict[str, object]:
        """Follow the direct-parent and ultimate-parent lei-record links.

        Each link, when present, resolves to the parent's full LEI record. A
        missing parent (reporting exception / no link) is normal and silently
        skipped — most ultimate parents have no parent of their own."""
        rels = (record.get("relationships") or {})
        out: Dict[str, object] = {}
        hierarchy: Dict[str, object] = {}

        direct = await self._parent_record(client, rels, "direct-parent")
        if direct:
            out["parent_lei"] = direct["lei"]
            hierarchy["direct_parent"] = {"lei": direct["lei"],
                                          "legal_name": direct.get("legal_name", "")}

        ultimate = await self._parent_record(client, rels, "ultimate-parent")
        if ultimate:
            out["ultimate_parent_lei"] = ultimate["lei"]
            if ultimate.get("legal_name"):
                out["ultimate_parent_name"] = ultimate["legal_name"]
            hierarchy["ultimate_parent"] = {"lei": ultimate["lei"],
                                            "legal_name": ultimate.get("legal_name", "")}

        if hierarchy:
            hierarchy["source"] = _SOURCE
            hierarchy["license"] = _LICENSE
            # JSON string per the EnrichmentResult data contract — never a raw cell.
            out["corporate_hierarchy"] = json.dumps(hierarchy)
        return out

    async def _parent_record(self, client: httpx.AsyncClient, rels: dict,
                             key: str) -> Optional[Dict[str, str]]:
        link = (((rels.get(key) or {}).get("links") or {}).get("lei-record"))
        if not link:
            return None
        data = await self._get_json(client, link)
        rec = _first_record(data)
        if not rec:
            return None
        lei = rec.get("id")
        if not lei:
            return None
        return {"lei": lei, "legal_name": _legal_name(rec)}

    # ── Rate-limited HTTP helper (graceful on any non-200 / error) ───────────

    async def _get_json(self, client: httpx.AsyncClient, url: str,
                        params: Optional[dict] = None) -> Optional[dict]:
        await _LIMITER.acquire()
        try:
            r = await client.get(url, params=params)
            if r.status_code != 200:
                return None
            return r.json()
        except (httpx.HTTPError, ValueError, json.JSONDecodeError):
            return None


# ── LEI record → fields ─────────────────────────────────────────────────────

def _first_record(data: Optional[dict]) -> Optional[dict]:
    """Normalize a GLEIF JSON:API payload to a single record dict.

    lei-records (collection) → data is a list; lei-records/{lei} and the
    parent lei-record links → data is an object."""
    if not isinstance(data, dict):
        return None
    node = data.get("data")
    if isinstance(node, list):
        return node[0] if node else None
    if isinstance(node, dict):
        return node
    return None


def _legal_name(record: dict) -> str:
    return (((record.get("attributes") or {}).get("entity") or {})
            .get("legalName") or {}).get("name") or ""


def _record_to_fields(record: dict) -> Dict[str, object]:
    """Map a single LEI record to enrichment fields."""
    out: Dict[str, object] = {}
    lei = record.get("id")
    if lei:
        out["lei"] = lei

    entity = ((record.get("attributes") or {}).get("entity")) or {}

    name = (entity.get("legalName") or {}).get("name") or ""
    if name:
        out["legal_name"] = name

    addr = entity.get("legalAddress") or {}
    if addr.get("country"):
        out["country"] = addr["country"]
    if addr.get("city"):
        out["city"] = addr["city"]
    if addr.get("region"):
        out["region"] = addr["region"]

    status = entity.get("status") or ""
    if status:
        out["entity_status"] = status

    ra = (entity.get("registeredAt") or {}).get("id") or ""
    if ra:
        out["registration_authority"] = ra

    jurisdiction = entity.get("jurisdiction") or ""
    if jurisdiction:
        out["jurisdiction"] = jurisdiction

    return out


# ── name helpers ─────────────────────────────────────────────────────────────

def _norm_name(name: str) -> str:
    """Lowercase, strip legal suffixes + punctuation, collapse whitespace."""
    s = (name or "").lower()
    s = _NON_ALNUM_RE.sub(" ", s)
    s = _SUFFIX_RE.sub(" ", s)
    return re.sub(r"\s+", " ", s).strip()
