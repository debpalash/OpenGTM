"""
StaffSpy roster provider — multiple decision-makers per account, not just one.

cullenwatson/StaffSpy (research/linkedin-people/StaffSpy) scrapes a company's full
LinkedIn staff roster (names, titles, locations, skills). Where the existing
decision_maker_finder lands a single contact, this surfaces a ranked list of
seniority-matched people to target — a big jump in contacts-per-account.

ACTIVATION (all required — provider is inert otherwise):
    export STAFFSPY_ENABLED=1                            # explicit opt-in (default OFF)
    pip install staffspy
    export STAFFSPY_SESSION_FILE=/path/to/session.pkl   # LinkedIn login cookies
On first run StaffSpy opens a browser to log in once and saves the session file
(lasts ~a week). LinkedIn ToS / rate limits apply — keep max_results modest.
This scrapes LinkedIn against its Terms of Service and can get the operator's
account throttled or banned; it is OFF by default and must be turned on knowingly.

Writes a JSON roster into Lead.decision_makers and sets contact_person/contact_title
to the most senior match.
"""

import asyncio
import json
import logging
import os
import time
from typing import List, Optional

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.staffspy")

# Title keywords ranked by seniority — higher index = more senior.
_SENIORITY = [
    "intern", "associate", "executive", "specialist", "analyst", "engineer",
    "manager", "lead", "head", "director", "vp", "vice president", "chief",
    "cxo", "cto", "cfo", "coo", "ceo", "founder", "owner", "partner",
]


def _session_file() -> str:
    return os.getenv("STAFFSPY_SESSION_FILE", "") or ""


def _enabled() -> bool:
    """Explicit opt-in flag (default OFF).

    LinkedIn roster scraping runs against LinkedIn's ToS and can get the
    operator's account rate-limited or banned, so it must never be reachable by
    accident. This is a deliberate second key on top of STAFFSPY_SESSION_FILE:
    the operator has to knowingly set STAFFSPY_ENABLED=1 AND supply a session
    file AND `pip install staffspy` before the provider does anything.
    """
    return os.getenv("STAFFSPY_ENABLED", "").strip().lower() in ("1", "true", "yes", "on")


def _staffspy_installed() -> bool:
    try:
        import staffspy  # noqa: F401
        return True
    except Exception:
        return False


def is_available() -> bool:
    return _enabled() and bool(_session_file()) and _staffspy_installed()


def _seniority_rank(title: str) -> int:
    t = (title or "").lower()
    rank = -1
    for i, kw in enumerate(_SENIORITY):
        if kw in t:
            rank = max(rank, i)
    return rank


class StaffSpyRosterProvider(EnrichmentProvider):
    name = "staffspy"
    capabilities = ["decision_makers", "contact_person", "contact_title"]
    default_confidence = 0.8

    def __init__(self, max_results: int = 25, search_term: str = ""):
        self.max_results = max_results
        self.search_term = search_term  # e.g. "founder OR ceo OR head"

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()
        if not lead.company:
            return EnrichmentResult(provider=self.name, success=False, error="no_company",
                                    duration_ms=(time.time() - t0) * 1000)
        if not is_available():
            return EnrichmentResult(
                provider=self.name, success=False,
                error="staffspy_unavailable (opt in: STAFFSPY_ENABLED=1 + STAFFSPY_SESSION_FILE + pip install staffspy; LinkedIn ToS applies)",
                duration_ms=(time.time() - t0) * 1000,
            )

        try:
            roster = await asyncio.to_thread(self._scrape, lead.company, lead.city)
        except Exception as e:
            logger.warning("StaffSpy error for %s: %s", lead.company, e)
            return EnrichmentResult(provider=self.name, success=False, error=str(e)[:200],
                                    duration_ms=(time.time() - t0) * 1000)

        if not roster:
            return EnrichmentResult(provider=self.name, success=False, error="no_staff_found",
                                    duration_ms=(time.time() - t0) * 1000)

        roster.sort(key=lambda p: _seniority_rank(p.get("title", "")), reverse=True)
        top = roster[0]
        return EnrichmentResult(
            provider=self.name, success=True, confidence=self.default_confidence,
            fields={
                "decision_makers": json.dumps(roster[:self.max_results], ensure_ascii=False),
                "contact_person": top.get("name", ""),
                "contact_title": top.get("title", ""),
            },
            duration_ms=(time.time() - t0) * 1000,
        )

    def _scrape(self, company: str, location: str) -> List[dict]:
        """Blocking StaffSpy call (run in a thread)."""
        from staffspy import LinkedInAccount

        account = LinkedInAccount(session_file=_session_file(), log_level=0)
        # Map company name → LinkedIn slug heuristically (StaffSpy accepts the name).
        df = account.scrape_staff(
            company_name=company,
            search_term=self.search_term or None,
            location=location or None,
            extra_profile_data=False,
            max_results=self.max_results,
        )
        if df is None or getattr(df, "empty", True):
            return []

        people: List[dict] = []
        for _, row in df.iterrows():
            name = str(row.get("name", "") or "").strip()
            if not name:
                continue
            people.append({
                "name": name,
                "title": str(row.get("current_position", row.get("headline", "")) or "").strip(),
                "linkedin": str(row.get("profile_link", row.get("profile_id", "")) or "").strip(),
                "location": str(row.get("location", "") or "").strip(),
            })
        return people
