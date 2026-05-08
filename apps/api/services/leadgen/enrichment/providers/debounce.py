"""
Debounce.io — Cheap bulk email verification provider.

BYOK provider: requires Debounce API key.
Pricing: $2/1,000 verifications — 10x cheaper than NeverBounce, ZeroBounce.
Free tier: 100 verifications.
Docs: https://debounce.io/api/

Returns:
  - deliverability status (deliverable, undeliverable, risky, unknown)
  - disposable email detection
  - role-based email detection (info@, support@)
  - free provider detection (gmail, yahoo, etc.)
"""

import os
import time
import logging
import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.debounce")


def _get_api_key() -> str:
    """Read Debounce API key from settings DB or environment."""
    try:
        from apps.api.database import get_setting
        return get_setting("DEBOUNCE_API_KEY", "") or os.getenv("DEBOUNCE_API_KEY", "")
    except Exception:
        return os.getenv("DEBOUNCE_API_KEY", "")


class DebounceProvider(EnrichmentProvider):
    name = "debounce"
    capabilities = ["email_verify"]
    default_confidence = 0.92  # Professional verification service

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        api_key = _get_api_key()
        if not api_key:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No Debounce API key configured",
            )

        email = lead.email or ""
        if not email or "@" not in email:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No email to verify",
            )

        t0 = time.time()

        try:
            async with httpx.AsyncClient(timeout=15) as client:
                resp = await client.get(
                    "https://api.debounce.io/v1/",
                    params={"api": api_key, "email": email},
                )

                if resp.status_code != 200:
                    return EnrichmentResult(
                        provider=self.name, success=False,
                        error=f"Debounce API error: {resp.status_code}",
                        duration_ms=(time.time() - t0) * 1000,
                    )

                data = resp.json().get("debounce", {})
                fields = {"email": email}

                # Main deliverability result
                code = data.get("code", "")
                result_text = data.get("result", "")
                reason = data.get("reason", "")
                send_safely = data.get("send_transactional", "")

                # Map Debounce result to our standard
                if code == "5":
                    fields["email_verify"] = "valid"
                    confidence = 0.95
                elif code in ("6", "7"):
                    fields["email_verify"] = "risky"
                    confidence = 0.60
                elif code in ("1", "2"):
                    fields["email_verify"] = "invalid"
                    confidence = 0.10
                else:
                    fields["email_verify"] = "unknown"
                    confidence = 0.40

                # Additional flags
                if data.get("free_email", "false") == "true":
                    fields["email_type"] = "free_provider"
                if data.get("disposable_email", "false") == "true":
                    fields["email_type"] = "disposable"
                if data.get("role", "false") == "true":
                    fields["email_role"] = "role_based"

                return EnrichmentResult(
                    provider=self.name, success=True,
                    fields=fields,
                    confidence=confidence,
                    duration_ms=(time.time() - t0) * 1000,
                )

        except Exception as e:
            logger.warning(f"Debounce error: {e}")
            return EnrichmentResult(
                provider=self.name, success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
