"""Per-fact provenance: license vocabulary + central provider→license map.

This is the single source of truth for the *license* dimension of per-fact
provenance (see features/research-per-fact-provenance-spec.md). A fact's
provenance is the 4-tuple ``{source, license, confidence, fetched_at}``; this
module resolves the ``license`` token and builds the canonical provenance dict.

License resolution precedence (``resolve_license``):
  1. A license a provider declares on itself (``EnrichmentProvider.source_license``)
     — when it is a real vocabulary token (not the ``"unknown"`` default).
  2. The central :data:`PROVIDER_LICENSE` fallback map, keyed by provider name.
  3. ``"unknown"`` — never raises.

The vocabulary is a fixed, SPDX-ish token set so the UI can render a stable
colour chip and (eventually) a future ETL can group facts by license.
"""

from __future__ import annotations

import json
from datetime import datetime, timezone
from typing import Any, Dict, Optional

# ── Fixed license vocabulary (D3) ────────────────────────────────────────────
# Open / redistributable .......... CC0-1.0, CC-BY-4.0
# Government / open registries ..... public-record
# Derived / scraped (use at risk) .. scraped
# Vendor APIs (ToS: no resale) ..... proprietary-api
# Self-supplied (import / manual) .. user-provided
# Unmappable ....................... unknown
LICENSE_VOCAB = frozenset({
    "CC0-1.0",
    "CC-BY-4.0",
    "public-record",
    "scraped",
    "proprietary-api",
    "user-provided",
    "unknown",
})

# Sentinel sources that are never an enrichment provider.
SOURCE_USER_PROVIDED = "user-provided"
SOURCE_CSV = "csv"


# ── Central provider → license fallback map ──────────────────────────────────
# Keyed by the provider's registry name. A provider class may override this by
# declaring its own ``source_license`` attr (resolve_license prefers that).
PROVIDER_LICENSE: Dict[str, str] = {
    # ── Open / CC0 government & open-data registries ──
    "gleif": "CC0-1.0",                 # LEI register — explicitly CC0
    "wikidata": "CC0-1.0",              # Wikidata dumps are CC0

    # ── Public-record government registries ──
    "mca_registry": "public-record",   # India MCA registered-office data
    "companies_house": "public-record",
    "sec": "public-record",
    "sec_edgar": "public-record",

    # ── Proprietary vendor APIs (ToS typically forbids resale/redistribution) ──
    "hunter_io": "proprietary-api",
    "apollo_io": "proprietary-api",
    "snovio": "proprietary-api",
    "prospeo": "proprietary-api",
    "people_data_labs": "proprietary-api",
    "clearbit": "proprietary-api",
    "abstract_api": "proprietary-api",
    "debounce": "proprietary-api",

    # ── Scraped / derived data (use-at-own-risk) ──
    "deep_scraper": "scraped",
    "website_scraper": "scraped",
    "jsonld_firmographics": "scraped",
    "email_harvester": "scraped",
    "ddg_email": "scraped",
    "ddg_company": "scraped",
    "mailscout": "scraped",
    "holehe": "scraped",
    "social_finder": "scraped",
    "crosslinked": "scraped",
    "staffspy": "scraped",
    "decision_maker": "scraped",
    "local_business": "scraped",
    "facebook_pages": "scraped",
    "google_maps": "scraped",
    "tech_stack": "scraped",
    "company_intel": "scraped",
    "jobspy": "scraped",
    "ats_hiring": "scraped",
    "company_size_heuristic": "scraped",
    "lead_scorer": "scraped",

    # ── Self-supplied ──
    SOURCE_USER_PROVIDED: "user-provided",
    SOURCE_CSV: "user-provided",
    "manual": "user-provided",
    "csv_import": "user-provided",
}


def resolve_license(provider_name: Optional[str], declared: Optional[str] = None) -> str:
    """Resolve a fact's license token. Never raises; unknown → ``"unknown"``.

    ``declared`` is the provider-declared ``source_license`` (when threaded from
    the provider). It wins when it is a real vocabulary token; otherwise we fall
    back to the central map keyed by ``provider_name``.
    """
    if declared and declared != "unknown" and declared in LICENSE_VOCAB:
        return declared
    if not provider_name:
        return "unknown"
    # Strip a "cache:hunter_io" style prefix the waterfall may stamp on a winner.
    name = provider_name.split(":", 1)[-1] if ":" in provider_name else provider_name
    lic = PROVIDER_LICENSE.get(provider_name) or PROVIDER_LICENSE.get(name)
    return lic if lic in LICENSE_VOCAB else "unknown"


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def provenance_for(
    source: Optional[str],
    *,
    license: Optional[str] = None,
    confidence: Optional[float] = None,
    declared_license: Optional[str] = None,
    fetched_at: Optional[str] = None,
) -> Dict[str, Any]:
    """Build the canonical per-fact provenance dict ``{source, license,
    confidence, fetched_at}``.

    ``license`` may be passed explicitly; otherwise it is resolved from
    ``source`` (+ optional ``declared_license``). ``fetched_at`` defaults to now
    (UTC, ISO-8601). ``confidence`` is left as-is (may be ``None`` for sources
    that don't compute one, e.g. an AI/formula column or a manual edit).
    """
    lic = license if (license in LICENSE_VOCAB) else resolve_license(source, declared_license)
    prov: Dict[str, Any] = {
        "source": source or "unknown",
        "license": lic,
        "confidence": (round(float(confidence), 4) if confidence is not None else None),
        "fetched_at": fetched_at or _now_iso(),
    }
    return prov


def merge_field_provenance(current: Optional[str], updates: Dict[str, Dict[str, Any]]) -> str:
    """Read-modify-write merge of a lead's ``field_provenance`` JSON string.

    ``current`` is the stored JSON (a string, possibly empty/None); ``updates``
    is ``{field_name: provenance_dict}``. Returns the new JSON string. Tolerant
    of malformed/empty input (treated as an empty object).
    """
    d: Dict[str, Any] = {}
    if current:
        try:
            parsed = json.loads(current)
            if isinstance(parsed, dict):
                d = parsed
        except (json.JSONDecodeError, TypeError):
            d = {}
    d.update(updates)
    return json.dumps(d, default=str)
