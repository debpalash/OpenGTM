"""
MailScout SMTP Verification Provider.

Originally ported from batuhanaky/mailscout. The SMTP RCPT / catch-all engine now
lives in the shared `enrichment.email_verify` module (single source of truth — also
adds disposable/role/free classification and single-session catch-all calibration).
This file is a thin async adapter over it, preserving the public API:
  - MailScoutVerifyProvider   (registered in job_runner + workbook)
  - smtp_verify_email(), check_catchall(), bulk_verify_emails()  (back-compat helpers)
"""

import asyncio
import logging
from time import time
from typing import List, Tuple

from apps.api.services.leadgen.enrichment import email_verify as ev
from apps.api.services.leadgen.enrichment.provider import (
    EnrichmentProvider, EnrichmentResult,
)
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.mailscout")

# Shared catch-all cache (single source of truth).
_catchall_cache = ev._catchall_cache


async def smtp_verify_email(email: str, timeout: int = 5) -> Tuple[bool, int]:
    """SMTP RCPT TO verification — checks if a mailbox exists.

    Returns (is_valid, smtp_code): (True, 250) | (False, 550) | (False, 0=unknown).
    Delegates to email_verify.probe_domain (single SMTP session).
    """
    def _run() -> Tuple[bool, int]:
        if "@" not in email:
            return False, 0
        domain = email.split("@")[1]
        probe = ev.probe_domain(domain, [email], timeout)
        res = (probe.results or {}).get(email)
        if res is True:
            return True, 250
        if res is False:
            return False, 550
        return False, 0  # unknown / unreachable / catch-all

    return await asyncio.to_thread(_run)


async def check_catchall(domain: str, timeout: int = 5) -> bool:
    """Return True if the domain accepts any address (catch-all). Cached."""
    def _run() -> bool:
        if domain in ev._catchall_cache:
            return ev._catchall_cache[domain]
        # Probe a single throwaway address; probe_domain fills the catch-all cache.
        probe = ev.probe_domain(domain, [f"{ev._gen_random_local()}@{domain}"], timeout)
        return probe.catch_all

    return await asyncio.to_thread(_run)


class MailScoutVerifyProvider(EnrichmentProvider):
    """SMTP-level email verification with catch-all detection.

    Verifies (doesn't discover) emails — place AFTER pattern generators in the chain.
    """

    name = "mailscout"
    capabilities = ["email", "email_confidence"]
    default_confidence = 0.95  # SMTP verified = highest confidence

    def __init__(self, timeout: int = 5, max_verify: int = 3):
        self.timeout = timeout
        self.max_verify = max_verify

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        start = time()

        if not lead.email or "@" not in lead.email:
            return EnrichmentResult(
                provider=self.name, success=False, error="no_email_to_verify",
                duration_ms=(time() - start) * 1000,
            )

        # Verify the current email (classify + SMTP + catch-all in one probe).
        result = await asyncio.to_thread(ev.verify_email, lead.email, float(self.timeout))

        if result.confidence == "smtp_verified":
            return EnrichmentResult(
                provider=self.name, success=True, confidence=0.95,
                fields={"email": lead.email, "email_confidence": "smtp_verified"},
                duration_ms=(time() - start) * 1000,
            )

        if result.catch_all:
            # Can't trust SMTP on a catch-all domain — keep as pattern-grade.
            return EnrichmentResult(
                provider=self.name, success=True, confidence=0.5,
                fields={"email": lead.email, "email_confidence": "pattern"},
                duration_ms=(time() - start) * 1000,
            )

        # Current email was hard-rejected (or unknown). Try generated patterns.
        if result.deliverable is False and lead.contact_person and lead.website:
            from apps.api.services.leadgen.enrichment.email_finder import (
                generate_personal_patterns, _domain_from_url, _split_name,
            )

            email_domain = _domain_from_url(lead.website)
            if email_domain:
                first, last = _split_name(lead.contact_person)
                patterns = generate_personal_patterns(first, last, email_domain)[: self.max_verify]
                picked = await asyncio.to_thread(ev.pick_best_email, patterns, float(self.timeout))
                if picked.confidence == "smtp_verified":
                    return EnrichmentResult(
                        provider=self.name, success=True, confidence=0.9,
                        fields={"email": picked.email, "email_confidence": "smtp_verified"},
                        duration_ms=(time() - start) * 1000,
                    )

        return EnrichmentResult(
            provider=self.name, success=False, confidence=0.0,
            error="smtp_unverified",
            duration_ms=(time() - start) * 1000,
        )


async def bulk_verify_emails(emails: List[str], timeout: int = 5) -> List[dict]:
    """Verify multiple emails. Returns {email, is_valid, smtp_code, is_catchall}."""
    def _run() -> List[dict]:
        results = []
        domains_checked: dict = {}
        for email in emails:
            if "@" not in email:
                results.append({"email": email, "is_valid": False, "smtp_code": 0, "is_catchall": False})
                continue
            domain = email.split("@")[1]
            if domain not in domains_checked:
                probe = ev.probe_domain(domain, [email], timeout)
                domains_checked[domain] = probe.catch_all
                res = (probe.results or {}).get(email)
            else:
                res = (ev.probe_domain(domain, [email], timeout).results or {}).get(email)
            if domains_checked[domain]:
                results.append({"email": email, "is_valid": None, "smtp_code": 0, "is_catchall": True})
            else:
                results.append({
                    "email": email,
                    "is_valid": res if res is not None else False,
                    "smtp_code": 250 if res is True else (550 if res is False else 0),
                    "is_catchall": False,
                })
        return results

    return await asyncio.to_thread(_run)
