"""Companies House provider — authoritative UK firmographics + free decision-makers.

Source: UK Companies House Public Data API (the official, free company register).
Works from the company NAME — the case where the lead's own website is dead or
absent. Unlike paid people-data vendors, the officers (directors) and PSC
(persons with significant control / beneficial owners) endpoints are free, so
this provider yields real, sourced decision-makers and ownership at zero cost.

Resolution pipeline:
  1. Resolve the company NAME → company number via the search endpoint
     (prefer an active company; fall back to the top hit).
  2. Pull the company profile (status, incorporation date, registered address).
  3. Pull officers → director-level `decision_makers` (role + appointment date).
  4. Pull PSC → beneficial owners, appended to `decision_makers`.

Fills (EnrichmentResult.fields):
  • decision_makers   — JSON string [{name, title, role, appointed_on, source}]
  • company_number    — registered number (e.g. "01234567")
  • company_status    — e.g. "active", "dissolved"
  • incorporation_date— ISO date the company was incorporated
  • registered_address— flattened registered office address
  • address           — same registered office (canonical Lead field)
  • founded_year      — year of incorporation (canonical Lead field)
  • contact_person /
    contact_title     — top active director, for the workbook cell

API: https://api.company-information.service.gov.uk
Auth: HTTP Basic — API key as the username, empty password (free to obtain at
https://developer.company-information.service.gov.uk/). OPTIONAL: with no key
the provider skips gracefully and makes NO network call.
"""
import json
import logging
import os
import time
from typing import Dict, List, Optional

import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.companies_house")

_BASE = "https://api.company-information.service.gov.uk"
_TIMEOUT = httpx.Timeout(8.0, connect=4.0)


def _get_api_key() -> str:
    """Read the Companies House API key from the settings DB or environment.

    OPTIONAL / BYOK: returns "" when unset, in which case the provider skips
    without making any network call. Mirrors the hunter_io key-reading pattern.
    """
    try:
        from apps.api.database import get_setting  # type: ignore
        return get_setting("COMPANIES_HOUSE_API_KEY", "") or os.getenv("COMPANIES_HOUSE_API_KEY", "")
    except Exception:
        return os.getenv("COMPANIES_HOUSE_API_KEY", "")


def _clean(v) -> str:
    if not v:
        return ""
    s = " ".join(str(v).split()).strip().strip(",")
    return s if s and s.upper() not in ("NA", "N/A", "NIL", "-") else ""


def _year(iso_date: str) -> str:
    """Extract YYYY from an ISO date like '2010-04-21'."""
    d = _clean(iso_date)
    return d[:4] if len(d) >= 4 and d[:4].isdigit() else ""


def _flatten_address(addr: Optional[dict]) -> str:
    """Flatten a Companies House registered_office_address object to one line."""
    if not isinstance(addr, dict):
        return ""
    parts = [
        addr.get("premises"),
        addr.get("address_line_1"),
        addr.get("address_line_2"),
        addr.get("locality"),
        addr.get("region"),
        addr.get("postal_code"),
        addr.get("country"),
    ]
    return ", ".join(_clean(p) for p in parts if _clean(p))


# Officer roles that count as decision-makers (directors / top management).
# Companies House officer_role values are lowercase-underscored.
_DIRECTOR_ROLES = (
    "director",
    "llp-member",
    "llp-designated-member",
    "member-of-management-organ",
    "member-of-supervisory-organ",
    "managing-officer",
)


class CompaniesHouseProvider(EnrichmentProvider):
    name = "companies_house"
    capabilities = [
        "decision_makers",
        "contact_person",
        "contact_title",
        "address",
        "founded_year",
    ]
    default_confidence = 0.85  # official government register
    requires_api_key = True
    free_tier_limit = 0  # free, rate-limited (600 requests / 5 min)
    cost_per_lookup = 0.0

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()

        def _ms() -> float:
            return (time.time() - t0) * 1000

        api_key = _get_api_key()
        if not api_key:
            # OPTIONAL provider: skip gracefully, make NO network call.
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No Companies House API key configured",
                duration_ms=_ms(),
            )

        company = (lead.company or "").strip()
        if not company:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="no_company", duration_ms=_ms(),
            )

        try:
            # Companies House uses HTTP Basic auth: key as username, blank password.
            async with httpx.AsyncClient(
                base_url=_BASE,
                timeout=_TIMEOUT,
                auth=(api_key, ""),
                headers={"Accept": "application/json"},
            ) as client:
                profile = await self._resolve_company(client, company)
                if not profile:
                    return EnrichmentResult(
                        provider=self.name, success=False,
                        error="not_in_companies_house", duration_ms=_ms(),
                    )

                company_number = _clean(profile.get("company_number"))
                officers = await self._fetch_officers(client, company_number)
                pscs = await self._fetch_psc(client, company_number)

            fields = self._to_fields(profile, officers, pscs)
            if not fields:
                return EnrichmentResult(
                    provider=self.name, success=False,
                    error="no_fields", duration_ms=_ms(),
                )
            return EnrichmentResult(
                provider=self.name, success=True, fields=fields,
                confidence=self.default_confidence, duration_ms=_ms(),
            )
        except httpx.HTTPStatusError as e:
            return EnrichmentResult(
                provider=self.name, success=False,
                error=f"Companies House API error: {e.response.status_code}",
                duration_ms=_ms(),
            )
        except Exception as e:
            logger.debug(f"companies_house enrich failed for {company!r}: {e}")
            return EnrichmentResult(
                provider=self.name, success=False,
                error=str(e)[:200], duration_ms=_ms(),
            )

    # ── resolution ────────────────────────────────────────────────

    async def _resolve_company(self, client: httpx.AsyncClient, company: str) -> Optional[dict]:
        """Name → company profile. Search for the number, then fetch the profile.

        Prefers an active company among the search hits; falls back to the top
        hit. Returns the full company profile dict (which carries status,
        incorporation date and registered address) or None on no match.
        """
        r = await client.get("/search/companies", params={"q": company, "items_per_page": 20})
        if r.status_code != 200:
            return None
        items = (r.json() or {}).get("items") or []
        if not items:
            return None

        active = [it for it in items if _clean(it.get("company_status")) == "active"]
        chosen = (active or items)[0]
        number = _clean(chosen.get("company_number"))
        if not number:
            return None

        pr = await client.get(f"/company/{number}")
        if pr.status_code != 200:
            # Search hits already carry enough firmographics; degrade to them.
            return chosen if _clean(chosen.get("company_number")) else None
        return pr.json() or chosen

    async def _fetch_officers(self, client: httpx.AsyncClient, number: str) -> List[dict]:
        if not number:
            return []
        try:
            r = await client.get(f"/company/{number}/officers",
                                 params={"items_per_page": 35})
            if r.status_code != 200:
                return []
            return (r.json() or {}).get("items") or []
        except Exception:
            return []

    async def _fetch_psc(self, client: httpx.AsyncClient, number: str) -> List[dict]:
        if not number:
            return []
        try:
            r = await client.get(
                f"/company/{number}/persons-with-significant-control",
                params={"items_per_page": 35},
            )
            if r.status_code != 200:
                return []
            return (r.json() or {}).get("items") or []
        except Exception:
            return []

    # ── mapping ───────────────────────────────────────────────────

    def _to_fields(self, profile: dict, officers: List[dict],
                   pscs: List[dict]) -> Dict[str, str]:
        out: Dict[str, str] = {}

        number = _clean(profile.get("company_number"))
        status = _clean(profile.get("company_status"))
        incorporated = _clean(profile.get("date_of_creation"))
        address = _flatten_address(profile.get("registered_office_address")
                                   or profile.get("address"))

        if number:
            out["company_number"] = number
        if status:
            out["company_status"] = status
        if incorporated:
            out["incorporation_date"] = incorporated
            year = _year(incorporated)
            if year:
                out["founded_year"] = year
        if address:
            out["registered_address"] = address
            out["address"] = address

        decision_makers = self._decision_makers(officers, pscs)
        if decision_makers:
            out["decision_makers"] = json.dumps(decision_makers)
            # Surface the top active director into the workbook cell fields.
            top = decision_makers[0]
            if top.get("name"):
                out["contact_person"] = top["name"]
            if top.get("title"):
                out["contact_title"] = top["title"]

        return out

    def _decision_makers(self, officers: List[dict], pscs: List[dict]) -> List[dict]:
        """Map officers (directors) + PSC (beneficial owners) → decision_makers.

        Active directors first (sorted so resigned officers sink), then beneficial
        owners. De-duplicated by lowercased name. Shape matches the rest of the
        codebase: [{"name", "title", "role", "appointed_on", "source"}].
        """
        people: List[dict] = []
        seen = set()

        def _add(name: str, title: str, role: str, appointed_on: str, source: str):
            name = _clean(name)
            if not name:
                return
            key = name.lower()
            if key in seen:
                return
            seen.add(key)
            entry = {"name": name, "title": title or role, "role": role,
                     "source": source}
            if appointed_on:
                entry["appointed_on"] = appointed_on
            people.append(entry)

        # Directors: active before resigned, so the top entry is a live director.
        def _is_active(o: dict) -> bool:
            return not _clean(o.get("resigned_on"))

        dirs = [o for o in officers
                if _clean(o.get("officer_role")) in _DIRECTOR_ROLES]
        dirs.sort(key=lambda o: (not _is_active(o), _clean(o.get("appointed_on"))))
        for o in dirs:
            role = _clean(o.get("officer_role")) or "director"
            _add(
                name=o.get("name"),
                title=_role_label(role),
                role=role,
                appointed_on=_clean(o.get("appointed_on")),
                source="officer",
            )

        # Beneficial owners (PSC).
        for p in pscs:
            if _clean(p.get("ceased_on")):
                continue
            _add(
                name=p.get("name"),
                title="Beneficial Owner",
                role="beneficial-owner",
                appointed_on=_clean(p.get("notified_on")),
                source="psc",
            )

        return people


def _role_label(role: str) -> str:
    """Human-readable title for a Companies House officer_role."""
    mapping = {
        "director": "Director",
        "llp-member": "LLP Member",
        "llp-designated-member": "LLP Designated Member",
        "member-of-management-organ": "Management Board Member",
        "member-of-supervisory-organ": "Supervisory Board Member",
        "managing-officer": "Managing Officer",
        "secretary": "Company Secretary",
    }
    return mapping.get(role, role.replace("-", " ").title() if role else "Director")
