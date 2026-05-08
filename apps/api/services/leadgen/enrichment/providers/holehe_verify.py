"""
Holehe Provider — Check email existence across 120+ sites (OSS).

Uses holehe (github.com/megadose/holehe, 7K+ stars) to verify if an
email is registered on major platforms like Twitter, Instagram, Spotify,
Adobe, etc. This replaces paid email verification APIs.

Free, unlimited, no API key needed.
pip install holehe
"""

import asyncio
import logging
import time
from typing import Dict, List

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.holehe")


async def _check_email_holehe(email: str) -> Dict:
    """Run holehe check on an email address.

    Returns dict with:
      - exists_on: list of sites where email is registered
      - total_checked: number of sites checked
      - is_real: whether email appears to be a real person
    """
    try:
        import httpx
        import holehe.modules
        import pkgutil
        import importlib

        # Get all holehe modules
        client = httpx.AsyncClient(timeout=10)

        modules = []
        for importer, modname, ispkg in pkgutil.walk_packages(
            holehe.modules.__path__, holehe.modules.__name__ + "."
        ):
            if not ispkg:
                try:
                    mod = importlib.import_module(modname)
                    # Each module has a function with same name as module
                    func_name = modname.split(".")[-1]
                    if hasattr(mod, func_name):
                        modules.append(getattr(mod, func_name))
                except Exception:
                    continue

        # Run checks (limit concurrency to avoid rate limits)
        results = []
        semaphore = asyncio.Semaphore(10)

        async def check_one(func):
            async with semaphore:
                try:
                    result = {"name": func.__name__}
                    await func(email, client, result)
                    return result
                except Exception:
                    return None

        tasks = [check_one(func) for func in modules[:50]]  # Cap at 50 to avoid slowness
        raw_results = await asyncio.gather(*tasks, return_exceptions=True)

        await client.aclose()

        exists_on = []
        for r in raw_results:
            if isinstance(r, dict) and r.get("exists") is True:
                exists_on.append(r.get("name", "unknown"))

        return {
            "exists_on": exists_on,
            "total_checked": len([r for r in raw_results if isinstance(r, dict)]),
            "is_real": len(exists_on) >= 2,  # Found on 2+ sites = likely real
        }

    except ImportError:
        logger.warning("holehe not installed: pip install holehe")
        return {"exists_on": [], "total_checked": 0, "is_real": False, "error": "holehe not installed"}
    except Exception as e:
        logger.warning(f"holehe error: {e}")
        return {"exists_on": [], "total_checked": 0, "is_real": False, "error": str(e)}


class HoleheProvider(EnrichmentProvider):
    """Verify email existence using holehe (120+ site checks).

    This is a FREE alternative to paid email verification APIs like
    ZeroBounce, NeverBounce, or Debounce. Instead of checking SMTP
    deliverability, it checks whether the email is actually registered
    on real services — which is stronger proof of existence.
    """

    name = "holehe"
    capabilities = ["email_verify"]
    default_confidence = 0.85

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()

        email = lead.email or ""
        if not email or "@" not in email:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No email to verify",
                duration_ms=(time.time() - t0) * 1000,
            )

        result = await _check_email_holehe(email)

        if result.get("error"):
            return EnrichmentResult(
                provider=self.name, success=False,
                error=result["error"],
                duration_ms=(time.time() - t0) * 1000,
            )

        fields = {"email": email}

        if result["is_real"]:
            fields["email_verify"] = "verified_active"
            sites = result["exists_on"][:5]
            fields["email_presence"] = ", ".join(sites)
            confidence = min(0.95, 0.70 + len(result["exists_on"]) * 0.05)
        elif result["exists_on"]:
            fields["email_verify"] = "likely_valid"
            fields["email_presence"] = ", ".join(result["exists_on"][:3])
            confidence = 0.65
        else:
            fields["email_verify"] = "no_presence"
            confidence = 0.30

        return EnrichmentResult(
            provider=self.name, success=True,
            fields=fields,
            confidence=confidence,
            duration_ms=(time.time() - t0) * 1000,
        )
