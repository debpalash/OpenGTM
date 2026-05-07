"""
DDG Email Provider — Email discovery via DuckDuckGo search + pattern generation.

Wraps the existing email_finder module as a formal EnrichmentProvider.
Free, unlimited, no API key needed.
"""

import time
import logging
import asyncio
from typing import Optional

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.ddg_email")


class DDGEmailProvider(EnrichmentProvider):
    name = "ddg_email"
    capabilities = ["email"]
    default_confidence = 0.6

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()

        if not lead.company and not lead.website:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No company name or website available",
                duration_ms=(time.time() - t0) * 1000,
            )

        try:
            from apps.api.services.leadgen.enrichment.email_finder import (
                find_email_via_search,
                find_personal_email,
                _domain_from_url,
            )

            domain = _domain_from_url(lead.website) if lead.website else ""
            fields = {}

            # Strategy 1: If we have a contact person, try personal email
            if lead.contact_person and domain:
                email, confidence = await asyncio.to_thread(
                    find_personal_email,
                    lead.contact_person,
                    lead.company,
                    domain,
                )
                if email:
                    fields["email"] = email
                    fields["email_confidence"] = confidence
                    return EnrichmentResult(
                        provider=self.name,
                        success=True,
                        fields=fields,
                        confidence=0.7 if confidence == "verified" else 0.4,
                        duration_ms=(time.time() - t0) * 1000,
                    )

            # Strategy 2: Company email via DDG search
            email = await asyncio.to_thread(
                find_email_via_search,
                lead.company,
                lead.city or "",
                domain,
            )
            if email:
                fields["email"] = email
                fields["email_confidence"] = "verified"
                return EnrichmentResult(
                    provider=self.name,
                    success=True,
                    fields=fields,
                    confidence=0.6,
                    duration_ms=(time.time() - t0) * 1000,
                )

            # Strategy 3: Pattern-based fallback
            if domain:
                fields["email"] = f"info@{domain}"
                fields["email_confidence"] = "generic"
                return EnrichmentResult(
                    provider=self.name,
                    success=True,
                    fields=fields,
                    confidence=0.2,
                    duration_ms=(time.time() - t0) * 1000,
                )

            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No email found",
                duration_ms=(time.time() - t0) * 1000,
            )

        except Exception as e:
            logger.warning(f"DDG Email error: {e}")
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
