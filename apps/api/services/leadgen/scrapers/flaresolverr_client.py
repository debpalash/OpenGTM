"""
FlareSolverr client — recover leads lost to Cloudflare/DDoS-Guard challenge pages.

FlareSolverr (research/scraping-engines/FlareSolverr) runs as a separate proxy
service that solves JS/Cloudflare challenges in a real browser and returns the
final HTML + cookies. This is a Tier-3 fallback: try it when the stealth HTTP tier
returns a challenge page (see http.py CHALLENGE_MARKERS).

ACTIVATION: set FLARESOLVERR_URL to a running instance, e.g.
    docker run -d -p 8191:8191 ghcr.io/flaresolverr/flaresolverr:latest
    export FLARESOLVERR_URL=http://localhost:8191
Without it, `is_available()` is False and callers skip this tier.
"""

import logging
import os
from dataclasses import dataclass, field
from typing import Dict, List, Optional

logger = logging.getLogger("leadgen.flaresolverr")


def _endpoint() -> str:
    return (os.getenv("FLARESOLVERR_URL", "") or "").rstrip("/")


def is_available() -> bool:
    return bool(_endpoint())


@dataclass
class SolveResult:
    ok: bool = False
    status_code: int = 0
    html: str = ""
    url: str = ""
    user_agent: str = ""
    cookies: List[Dict] = field(default_factory=list)
    error: str = ""

    def cookie_header(self) -> str:
        """Cookies as a `Cookie:` header value, for reuse on cheaper tiers."""
        return "; ".join(f"{c.get('name')}={c.get('value')}" for c in self.cookies if c.get("name"))


async def solve(url: str, max_timeout_ms: int = 60_000, proxy: Optional[str] = None) -> SolveResult:
    """Fetch `url` through FlareSolverr, solving any Cloudflare challenge.

    Returns SolveResult(ok=False, error=...) when the service is unset/unreachable —
    callers should treat that as "tier unavailable", not a hard failure.
    """
    base = _endpoint()
    if not base:
        return SolveResult(ok=False, error="FLARESOLVERR_URL not configured")

    payload: Dict = {"cmd": "request.get", "url": url, "maxTimeout": max_timeout_ms}
    if proxy:
        payload["proxy"] = {"url": proxy}

    try:
        import httpx
        async with httpx.AsyncClient(timeout=(max_timeout_ms / 1000) + 15) as client:
            resp = await client.post(f"{base}/v1", json=payload)
            data = resp.json()
    except Exception as e:
        logger.warning("FlareSolverr request failed for %s: %s", url, e)
        return SolveResult(ok=False, error=str(e)[:200])

    if data.get("status") != "ok":
        return SolveResult(ok=False, error=data.get("message", "flaresolverr_error")[:200])

    sol = data.get("solution", {}) or {}
    return SolveResult(
        ok=True,
        status_code=int(sol.get("status", 0) or 0),
        html=sol.get("response", "") or "",
        url=sol.get("url", url),
        user_agent=sol.get("userAgent", "") or "",
        cookies=sol.get("cookies", []) or [],
    )
