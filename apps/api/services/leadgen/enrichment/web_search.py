"""Drop-in `DDGS` replacement that routes web search through the self-hosted
SearXNG stack instead of hitting DuckDuckGo directly (which CAPTCHA/rate-limits
our single server IP → the dominant `no_data` cause).

Resolution order, each a strict fallback to the next:
  1. SearXNG **pool** gateway  (apps/searxng-pool, SEARXPOOL_URL) — concurrent,
     health-checked across our instances.
  2. **Direct** self-hosted SearXNG (SEARXNG_URL) with working engines.
  3. The real **ddgs** library (legacy behavior) — so this is never a regression.

Interface matches `ddgs.DDGS`: `with DDGS() as d: d.text(query, max_results=N)`
returns a list of `{"title","href","body"}`. Providers swap one import line and
nothing else changes.
"""
import logging
import os
from typing import Dict, List, Optional

logger = logging.getLogger("enrichment.web_search")

_POOL_URL = os.getenv("SEARXPOOL_URL", "http://localhost:8889").rstrip("/")
_SEARXNG_URL = os.getenv("SEARXNG_URL", "http://localhost:8888").rstrip("/")
_ENGINES = os.getenv("SEARXPOOL_DEFAULT_ENGINES", "bing,qwant,yahoo")
_DISABLED = os.getenv("SEARXNG_DISABLE", "").lower() in ("1", "true", "yes")
_TIMEOUT = float(os.getenv("SEARXNG_HTTP_TIMEOUT", "12"))


def _to_ddgs_shape(results: List[Dict], max_results: int) -> List[Dict]:
    out = []
    for r in results:
        url = r.get("url") or r.get("href") or ""
        if not url:
            continue
        out.append({
            "title": r.get("title", ""),
            "href": url,
            "body": r.get("content", "") or r.get("body", ""),
        })
        if len(out) >= max_results:
            break
    return out


def _searx_search(query: str, max_results: int) -> Optional[List[Dict]]:
    """Try the pool, then direct SearXNG. Returns None if neither yields results."""
    if _DISABLED:
        return None
    import httpx

    attempts = [
        (_POOL_URL + "/search", {"q": query, "n": "4"}),
        (_SEARXNG_URL + "/search", {"q": query, "format": "json", "engines": _ENGINES}),
    ]
    for url, params in attempts:
        try:
            with httpx.Client(timeout=_TIMEOUT) as c:
                resp = c.get(url, params=params)
            if resp.status_code != 200:
                continue
            data = resp.json()
            hits = _to_ddgs_shape(data.get("results", []) or [], max_results)
            if hits:
                return hits
        except Exception as e:
            logger.debug(f"searx search via {url} failed: {e}")
    return None


class DDGS:
    """Source-compatible stand-in for `ddgs.DDGS`."""

    def __init__(self, proxy: Optional[str] = None, timeout: int = 8, **_kw):
        self.proxy = proxy
        self.timeout = timeout

    def __enter__(self):
        return self

    def __exit__(self, *_a):
        return False

    def text(self, query: str, max_results: int = 10, **kwargs) -> List[Dict]:
        hits = _searx_search(query, max_results)
        if hits:
            return hits
        # Final fallback: the real library (keeps legacy behavior alive).
        try:
            from ddgs import DDGS as _RealDDGS
            d = _RealDDGS(proxy=self.proxy, timeout=self.timeout)
            return list(d.text(query, max_results=max_results, **kwargs))
        except Exception as e:
            logger.debug(f"ddgs fallback failed for {query!r}: {e}")
            return []
