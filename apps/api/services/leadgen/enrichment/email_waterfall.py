"""
Email-Finder Waterfall — discover a business email by trying providers in order.

This is the "find an email when we don't have one yet" counterpart to
``email_verify`` (which only *verifies* an email we already hold). It runs AFTER
website discovery in the pipeline and only for leads that still lack an email.

Provider order (cheapest/free first, stops at the first hit):

    1. website  — regex/scrape the company website (free, no API key)
    2. hunter   — Hunter.io domain/email-finder API  (optional HUNTER_API_KEY)
    3. snovio   — Snov.io domain email search        (optional SNOVIO_* creds)

Graceful degradation: each provider self-reports availability. With NO API keys
configured the waterfall still runs the free website step and otherwise no-ops —
it never raises, and a lead that already has an email is skipped entirely.

Provenance (NO new schema): the winning provider is recorded on the EXISTING
``Lead.email_provider`` field, and the full ordered attempt log is appended to the
EXISTING ``Lead.enrichment_waterfall`` JSON field under an ``"email_finder"`` entry.
``Lead.enrichment_attempts`` is incremented by the number of providers tried.
"""

import json
import logging
import re
from dataclasses import dataclass, field
from typing import Awaitable, Callable, List, Optional, Tuple
from urllib.parse import urlparse

from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.email_waterfall")

_EMAIL_RE = re.compile(r"[\w.\-+]+@[\w.\-]+\.\w{2,}")

# Local-parts / domains we never want to ship as a "found" email.
_JUNK_LOCALS = {"example", "test", "noreply", "no-reply", "donotreply"}
_JUNK_DOMAINS = {
    "example.com", "domain.com", "email.com", "sentry.io", "wixpress.com",
    "w3.org", "schema.org", "googleapis.com", "gravatar.com", "wordpress.org",
    "cloudflare.com", "hunter.io", "snov.io",
}


def _domain_from_url(url: str) -> str:
    if not url:
        return ""
    url = url.strip().lower()
    if not url.startswith("http"):
        url = "https://" + url
    netloc = urlparse(url).netloc
    return netloc[4:] if netloc.startswith("www.") else netloc


def _is_usable_email(email: str) -> bool:
    """Cheap sanity gate so we don't store obvious junk as a discovered email."""
    if not email or "@" not in email:
        return False
    email = email.strip().lower()
    if not _EMAIL_RE.fullmatch(email):
        return False
    local, _, domain = email.partition("@")
    if local in _JUNK_LOCALS or domain in _JUNK_DOMAINS:
        return False
    if any(email.endswith(ext) for ext in (".png", ".jpg", ".svg", ".gif", ".webp")):
        return False
    return True


@dataclass
class WaterfallStep:
    """One provider attempt in the email-finder waterfall."""
    name: str
    success: bool = False
    email: str = ""
    skipped: bool = False     # provider unavailable (no key) — not counted as a real try
    error: str = ""

    def to_dict(self) -> dict:
        d = {"provider": self.name, "success": self.success}
        if self.email:
            d["email"] = self.email
        if self.skipped:
            d["skipped"] = True
        if self.error:
            d["error"] = self.error[:120]
        return d


@dataclass
class WaterfallOutcome:
    email: str = ""
    provider: str = ""
    steps: List[WaterfallStep] = field(default_factory=list)

    @property
    def found(self) -> bool:
        return bool(self.email)

    @property
    def attempts(self) -> int:
        """Number of providers actually invoked (skipped ones don't count)."""
        return sum(1 for s in self.steps if not s.skipped)


# ── Provider finders: each returns (email, contact fields) or ("", {}) ──────
# A finder may raise; the waterfall converts that into a recorded error step.

async def _find_via_website(lead: Lead, domain: str) -> Tuple[str, dict]:
    """Free step: scrape the company website for a contact email (regex).

    Reuses the pipeline's StealthClient (proxy/rate-limit aware) and the shared
    ``_scrape_via_http`` page walker (homepage + /contact + /about).
    """
    if not lead.website:
        return "", {}
    try:
        from apps.api.services.leadgen.enrichment.website_scraper import _scrape_via_http
        from apps.api.services.leadgen.http import StealthClient

        client = StealthClient()
        data = await _scrape_via_http(client, lead.website)
        for e in (data.get("emails") or []):
            if _is_usable_email(e):
                return e, {}
    except Exception as exc:  # noqa: BLE001 — never let scraping break the chain
        raise RuntimeError(str(exc))
    return "", {}


async def _find_via_hunter(lead: Lead, domain: str) -> Tuple[str, dict]:
    from apps.api.services.leadgen.enrichment.providers.hunter_io import HunterProvider
    result = await HunterProvider().enrich(lead)
    if result.success and result.fields.get("email"):
        return result.fields["email"], _contact_fields(result.fields)
    if result.error:
        raise RuntimeError(result.error)
    return "", {}


async def _find_via_snovio(lead: Lead, domain: str) -> Tuple[str, dict]:
    from apps.api.services.leadgen.enrichment.providers.snovio import SnovioProvider
    result = await SnovioProvider().enrich(lead)
    if result.success and result.fields.get("email"):
        return result.fields["email"], _contact_fields(result.fields)
    if result.error:
        raise RuntimeError(result.error)
    return "", {}


def _contact_fields(fields: dict) -> dict:
    """Carry across any bonus contact identity a provider returned."""
    out = {}
    for k in ("contact_person", "contact_title"):
        if fields.get(k):
            out[k] = fields[k]
    return out


# ── Availability gates (no network) ─────────────────────────────────────────

def _hunter_available() -> bool:
    try:
        from apps.api.services.leadgen.enrichment.providers.hunter_io import _get_api_key
        return bool(_get_api_key())
    except Exception:
        return False


def _snovio_available() -> bool:
    try:
        from apps.api.services.leadgen.enrichment.providers.snovio import _get_credentials
        cid, secret = _get_credentials()
        return bool(cid and secret)
    except Exception:
        return False


# (name, finder, availability_gate). Website finder gated on having a website.
def _build_chain() -> List[Tuple[str, Callable[[Lead, str], Awaitable[Tuple[str, dict]]], Callable[[], bool]]]:
    return [
        ("website", _find_via_website, lambda: True),
        ("hunter", _find_via_hunter, _hunter_available),
        ("snovio", _find_via_snovio, _snovio_available),
    ]


async def find_email_waterfall(lead: Lead) -> WaterfallOutcome:
    """Run the email-finder waterfall for a single lead.

    Tries providers in order, stops at the first usable email, records full
    provenance on the lead, and returns the outcome. Mutates ``lead`` in place
    (sets ``email`` / ``email_provider`` only when one is found; always updates
    ``enrichment_waterfall`` / ``enrichment_attempts``).
    """
    outcome = WaterfallOutcome()

    if lead.has_email:
        # Already have one — nothing to find. (No-op, no provenance churn.)
        return outcome

    domain = _domain_from_url(lead.website)

    for name, finder, available in _build_chain():
        if not available():
            outcome.steps.append(WaterfallStep(name=name, skipped=True))
            continue

        step = WaterfallStep(name=name)
        try:
            email, extra = await finder(lead, domain)
        except Exception as exc:  # noqa: BLE001
            step.error = str(exc)
            outcome.steps.append(step)
            logger.debug("email_waterfall %s failed for %s: %s", name, lead.company, exc)
            continue

        if email and _is_usable_email(email):
            step.success = True
            step.email = email
            outcome.steps.append(step)
            outcome.email = email
            outcome.provider = name
            # Apply to the lead.
            lead.email = email
            lead.email_provider = name
            # Carry bonus contact identity if the lead lacks it.
            for k, v in extra.items():
                if not getattr(lead, k, ""):
                    setattr(lead, k, v)
            break

        outcome.steps.append(step)

    _record_provenance(lead, outcome)
    return outcome


def _record_provenance(lead: Lead, outcome: WaterfallOutcome) -> None:
    """Append the attempt log to the EXISTING enrichment_waterfall JSON field."""
    entry = {
        "field": "email_finder",
        "winner": outcome.provider,
        "value": outcome.email or None,
        "attempts": [s.to_dict() for s in outcome.steps],
    }
    try:
        existing = json.loads(lead.enrichment_waterfall) if lead.enrichment_waterfall else []
        if not isinstance(existing, list):
            existing = [existing]
    except (json.JSONDecodeError, TypeError):
        existing = []
    existing.append(entry)
    lead.enrichment_waterfall = json.dumps(existing, default=str)
    lead.enrichment_attempts = (lead.enrichment_attempts or 0) + outcome.attempts


async def enrich_emails_waterfall(
    leads: List[Lead],
    limit: Optional[int] = None,
) -> List[Lead]:
    """Run the waterfall over a batch of leads that still lack an email.

    Only the first ``limit`` candidates are processed (budget guard). Returns the
    same list (leads mutated in place). Never raises on a single-lead failure.
    """
    candidates = [l for l in leads if l.company and not l.has_email]
    if limit is not None:
        candidates = candidates[:limit]
    for lead in candidates:
        try:
            await find_email_waterfall(lead)
        except Exception as exc:  # noqa: BLE001
            logger.debug("email_waterfall errored for %s: %s", lead.company, exc)
    return leads
