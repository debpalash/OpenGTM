"""
MailScout SMTP Verification Provider.

Ported from batuhanaky/mailscout (80+ stars).
Provides SMTP RCPT TO verification and catch-all domain detection.

This is the highest-confidence email verification — it checks if the
actual mailbox exists on the mail server, not just if the domain has MX.
"""

import asyncio
import dns.resolver
import logging
import random
import smtplib
import string
from time import time
from typing import List, Optional, Tuple

from apps.api.services.leadgen.enrichment.provider import (
    EnrichmentProvider, EnrichmentResult,
)
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.mailscout")

# Cache catch-all results to avoid redundant checks
_catchall_cache: dict = {}


async def smtp_verify_email(email: str, timeout: int = 5) -> Tuple[bool, int]:
    """SMTP RCPT TO verification — checks if mailbox actually exists.

    Ported from mailscout Scout.check_smtp().

    Returns:
        (is_valid, smtp_code) — e.g. (True, 250) or (False, 550)
    """
    domain = email.split('@')[1]

    def _verify():
        try:
            records = dns.resolver.resolve(domain, 'MX', lifetime=timeout)
            mx_record = str(records[0].exchange)

            with smtplib.SMTP(mx_record, 25, timeout=timeout) as server:
                server.set_debuglevel(0)
                server.ehlo("verify.yupcha.com")
                server.mail('verify@yupcha.com')
                code, message = server.rcpt(email)
                return code == 250, code

        except dns.resolver.NoAnswer:
            return False, 0
        except dns.resolver.NXDOMAIN:
            return False, 0
        except smtplib.SMTPServerDisconnected:
            return False, 0
        except smtplib.SMTPConnectError:
            return False, 0
        except Exception as e:
            logger.debug(f"SMTP verify error for {email}: {e}")
            return False, 0

    return await asyncio.to_thread(_verify)


async def check_catchall(domain: str, timeout: int = 5) -> bool:
    """Check if a domain accepts all emails (catch-all).

    Ported from mailscout Scout.check_email_catchall().

    If a domain is catch-all, SMTP verification is useless because
    it accepts everything. We mark emails as "pattern" confidence instead.
    """
    if domain in _catchall_cache:
        return _catchall_cache[domain]

    # Generate a random, very unlikely email address
    random_prefix = ''.join(random.choices(string.ascii_lowercase + string.digits, k=12))
    random_email = f"{random_prefix}verify@{domain}"

    is_catchall, _ = await smtp_verify_email(random_email, timeout=timeout)
    _catchall_cache[domain] = is_catchall

    if is_catchall:
        logger.info(f"Domain {domain} is catch-all — SMTP verification will be skipped")

    return is_catchall


class MailScoutVerifyProvider(EnrichmentProvider):
    """SMTP-level email verification with catch-all detection.

    This provider doesn't discover new emails — it verifies existing ones.
    It should be placed AFTER pattern generators in the waterfall chain.

    Usage in waterfall:
        email chain: [PatternProvider, MailScoutVerifyProvider, ...]
    """

    name = "mailscout"
    capabilities = ["email", "email_confidence"]
    default_confidence = 0.95  # SMTP verified = highest confidence

    def __init__(self, timeout: int = 5, max_verify: int = 3):
        self.timeout = timeout
        self.max_verify = max_verify  # Max emails to verify per lead

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        """Verify and upgrade the confidence of an existing email."""
        start = time()

        if not lead.email or '@' not in lead.email:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="no_email_to_verify",
                duration_ms=(time() - start) * 1000,
            )

        domain = lead.email.split('@')[1]

        # Step 1: Check catch-all
        try:
            is_catchall = await check_catchall(domain, self.timeout)
        except Exception:
            is_catchall = False

        if is_catchall:
            # Can't trust SMTP for catch-all domains
            return EnrichmentResult(
                provider=self.name,
                success=True,
                confidence=0.5,
                fields={
                    "email": lead.email,
                    "email_confidence": "pattern",  # Downgrade — can't verify
                },
                duration_ms=(time() - start) * 1000,
            )

        # Step 2: SMTP verify the current email
        is_valid, smtp_code = await smtp_verify_email(lead.email, self.timeout)

        if is_valid:
            return EnrichmentResult(
                provider=self.name,
                success=True,
                confidence=0.95,
                fields={
                    "email": lead.email,
                    "email_confidence": "smtp_verified",
                },
                duration_ms=(time() - start) * 1000,
            )

        # Step 3: If primary email failed, try secondary patterns
        if lead.contact_person and lead.website:
            from apps.api.services.leadgen.enrichment.email_finder import (
                generate_personal_patterns, _domain_from_url, _split_name,
            )

            email_domain = _domain_from_url(lead.website)
            if email_domain:
                first, last = _split_name(lead.contact_person)
                patterns = generate_personal_patterns(first, last, email_domain)

                for candidate in patterns[:self.max_verify]:
                    is_valid, _ = await smtp_verify_email(candidate, self.timeout)
                    if is_valid:
                        return EnrichmentResult(
                            provider=self.name,
                            success=True,
                            confidence=0.9,
                            fields={
                                "email": candidate,
                                "email_confidence": "smtp_verified",
                            },
                            duration_ms=(time() - start) * 1000,
                        )

        return EnrichmentResult(
            provider=self.name,
            success=False,
            confidence=0.0,
            error=f"smtp_rejected_{smtp_code}",
            duration_ms=(time() - start) * 1000,
        )


async def bulk_verify_emails(emails: List[str], timeout: int = 5) -> List[dict]:
    """Verify multiple emails concurrently.

    Returns list of {email, is_valid, smtp_code, is_catchall}.
    """
    results = []

    # Group by domain to check catch-all once per domain
    domains_checked: dict = {}

    for email in emails:
        domain = email.split('@')[1]

        if domain not in domains_checked:
            domains_checked[domain] = await check_catchall(domain, timeout)

        if domains_checked[domain]:
            results.append({
                "email": email,
                "is_valid": None,  # Can't determine for catch-all
                "smtp_code": 0,
                "is_catchall": True,
            })
            continue

        is_valid, smtp_code = await smtp_verify_email(email, timeout)
        results.append({
            "email": email,
            "is_valid": is_valid,
            "smtp_code": smtp_code,
            "is_catchall": False,
        })

    return results
