"""Keyed search-engine adapters used as a fallback chain behind DuckDuckGo.

DuckDuckGo (via the ddgs library / SearXNG) is keyless and stays the default.
But for many `site:` queries DDG returns ~0 results once our server IP is
rate-limited, which silently produces `no_data`. This module provides small
adapters around commercial/keyed search APIs so that, *only when configured*,
we can fall back to them and still get results.

Every adapter returns the exact same shape the rest of the codebase expects
from `ddgs.text(...)`:

    [{"title": str, "href": str, "body": str}, ...]

Design contract:
  * All keys are OPTIONAL. With no keys set, `configured_engines()` is empty and
    behavior is identical to today (DDG only).
  * Adapters never raise to the caller — a failing engine returns `[]` and the
    chain moves on. (Exceptions are logged at debug level.)
  * Network access is wrapped in a tiny helper so tests can mock one place.

Engine order (first configured one is tried first) is controlled by
SEARCH_FALLBACK_ORDER, default "serpapi,bing,google_cse,brave".
"""
from __future__ import annotations

import logging
import os
from typing import Callable, Dict, List, Optional

logger = logging.getLogger("leadgen.search_engines")

# Per-engine HTTP timeout (seconds). Kept short so a slow engine fails fast.
_ENGINE_TIMEOUT = float(os.getenv("SEARCH_ENGINE_TIMEOUT", "10"))

# Default attempt order for keyed fallback engines.
_DEFAULT_ORDER = "serpapi,bing,google_cse,brave"


# ── Config (all optional) ────────────────────────────────────────────────
def _key(name: str) -> str:
    return (os.getenv(name) or "").strip()


def _serpapi_key() -> str:
    return _key("SERPAPI_KEY")


def _bing_key() -> str:
    return _key("BING_SEARCH_KEY")


def _google_cse() -> tuple[str, str]:
    return _key("GOOGLE_CSE_ID"), _key("GOOGLE_CSE_KEY")


def _brave_key() -> str:
    return _key("BRAVE_SEARCH_KEY")


# ── HTTP helper (single mockable seam) ───────────────────────────────────
def _http_get_json(url: str, params: Dict, headers: Optional[Dict] = None) -> Optional[Dict]:
    """GET `url` and return parsed JSON, or None on any failure.

    Isolated so tests can monkeypatch exactly one function to simulate every
    engine without real network access.
    """
    try:
        import httpx

        with httpx.Client(timeout=_ENGINE_TIMEOUT) as client:
            resp = client.get(url, params=params, headers=headers or {})
        if resp.status_code != 200:
            logger.debug("search engine %s returned HTTP %s", url, resp.status_code)
            return None
        return resp.json()
    except Exception as e:  # noqa: BLE001 — adapters must never raise to caller
        logger.debug("search engine GET %s failed: %s", url, e)
        return None


# ── Adapters ─────────────────────────────────────────────────────────────
def _serpapi(query: str, max_results: int) -> List[Dict]:
    key = _serpapi_key()
    if not key:
        return []
    data = _http_get_json(
        "https://serpapi.com/search.json",
        {"engine": "google", "q": query, "num": max_results, "api_key": key},
    )
    if not data:
        return []
    out: List[Dict] = []
    for r in data.get("organic_results", []) or []:
        href = r.get("link") or ""
        if not href:
            continue
        out.append({
            "title": r.get("title", "") or "",
            "href": href,
            "body": r.get("snippet", "") or "",
        })
        if len(out) >= max_results:
            break
    return out


def _bing(query: str, max_results: int) -> List[Dict]:
    key = _bing_key()
    if not key:
        return []
    data = _http_get_json(
        "https://api.bing.microsoft.com/v7.0/search",
        {"q": query, "count": max_results, "responseFilter": "Webpages"},
        headers={"Ocp-Apim-Subscription-Key": key},
    )
    if not data:
        return []
    out: List[Dict] = []
    for r in ((data.get("webPages") or {}).get("value") or []):
        href = r.get("url") or ""
        if not href:
            continue
        out.append({
            "title": r.get("name", "") or "",
            "href": href,
            "body": r.get("snippet", "") or "",
        })
        if len(out) >= max_results:
            break
    return out


def _google_cse_search(query: str, max_results: int) -> List[Dict]:
    cse_id, cse_key = _google_cse()
    if not (cse_id and cse_key):
        return []
    data = _http_get_json(
        "https://www.googleapis.com/customsearch/v1",
        # Google CSE caps `num` at 10 per request.
        {"q": query, "cx": cse_id, "key": cse_key, "num": min(max_results, 10)},
    )
    if not data:
        return []
    out: List[Dict] = []
    for r in data.get("items", []) or []:
        href = r.get("link") or ""
        if not href:
            continue
        out.append({
            "title": r.get("title", "") or "",
            "href": href,
            "body": r.get("snippet", "") or "",
        })
        if len(out) >= max_results:
            break
    return out


def _brave(query: str, max_results: int) -> List[Dict]:
    key = _brave_key()
    if not key:
        return []
    data = _http_get_json(
        "https://api.search.brave.com/res/v1/web/search",
        {"q": query, "count": min(max_results, 20)},
        headers={"X-Subscription-Token": key, "Accept": "application/json"},
    )
    if not data:
        return []
    out: List[Dict] = []
    for r in ((data.get("web") or {}).get("results") or []):
        href = r.get("url") or ""
        if not href:
            continue
        out.append({
            "title": r.get("title", "") or "",
            "href": href,
            "body": r.get("description", "") or "",
        })
        if len(out) >= max_results:
            break
    return out


# name → (adapter, is_configured) registry
_ADAPTERS: Dict[str, tuple[Callable[[str, int], List[Dict]], Callable[[], bool]]] = {
    "serpapi": (_serpapi, lambda: bool(_serpapi_key())),
    "bing": (_bing, lambda: bool(_bing_key())),
    "google_cse": (_google_cse_search, lambda: all(_google_cse())),
    "brave": (_brave, lambda: bool(_brave_key())),
}


def _order() -> List[str]:
    raw = os.getenv("SEARCH_FALLBACK_ORDER", _DEFAULT_ORDER)
    names = [s.strip() for s in raw.split(",") if s.strip()]
    # Drop unknown names defensively.
    return [n for n in names if n in _ADAPTERS]


def configured_engines() -> List[str]:
    """Engine names that have keys configured, in fallback order."""
    return [name for name in _order() if _ADAPTERS[name][1]()]


def search_fallback(query: str, max_results: int = 10) -> List[Dict]:
    """Try each configured keyed engine in order; return the first non-empty
    result set. Returns [] when no engine is configured or none have results.
    """
    for name in configured_engines():
        adapter, _ = _ADAPTERS[name]
        try:
            results = adapter(query, max_results)
        except Exception as e:  # noqa: BLE001 — never let one engine break the chain
            logger.debug("fallback engine %s raised for %r: %s", name, query, e)
            continue
        if results:
            logger.debug("fallback engine %s returned %d results for %r",
                         name, len(results), query)
            return results
    return []
