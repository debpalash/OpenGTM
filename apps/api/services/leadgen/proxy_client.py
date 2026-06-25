"""
Proxy Client — fetches rotating proxies from the proxy-manager service.

Usage:
    from apps.api.services.leadgen.proxy_client import get_proxy, get_ddgs, report_proxy

    # Get a raw proxy URL
    proxy = get_proxy()  # → "socks5://1.2.3.4:1080" or None

    # Get a DDGS instance with proxy
    with get_ddgs() as ddgs:
        results = ddgs.text("query")

    # Report success/failure for scoring
    report_proxy("1.2.3.4", 1080, success=True)
"""

import logging
import os
from typing import Optional

logger = logging.getLogger("leadgen.proxy")

PROXY_MANAGER_URL = os.getenv("PROXY_MANAGER_URL", "http://localhost:3050")

# Cache to avoid hammering the API for every single request
_proxy_cache: list[dict] = []
_cache_index = 0


def _fetch_proxies(count: int = 10) -> list[dict]:
    """Fetch a batch of proxies from the proxy manager."""
    global _proxy_cache, _cache_index
    try:
        import urllib.request
        import json

        url = f"{PROXY_MANAGER_URL}/api/proxy?limit={count}&maxLatency=4000"
        req = urllib.request.Request(url, headers={"Accept": "application/json"})
        with urllib.request.urlopen(req, timeout=3) as resp:
            data = json.loads(resp.read())
            if data.get("success") and data.get("proxies"):
                _proxy_cache = data["proxies"]
                _cache_index = 0
                logger.debug(f"Fetched {len(_proxy_cache)} proxies from manager")
                return _proxy_cache
    except Exception as e:
        logger.debug(f"Proxy manager unavailable: {e}")
    return []


def get_proxy(protocol: str = None) -> Optional[str]:
    """Get a proxy URL string for use with requests/DDGS.

    Returns: "socks5://host:port" or "http://host:port" or None if unavailable.
    """
    global _proxy_cache, _cache_index

    # Refill cache if empty or exhausted
    if _cache_index >= len(_proxy_cache):
        _fetch_proxies(20)

    if not _proxy_cache:
        return None

    # Round-robin through cached proxies
    proxy = _proxy_cache[_cache_index % len(_proxy_cache)]
    _cache_index += 1

    proto = proxy.get("protocol", "http")
    if protocol and proto != protocol:
        # Try to find one with the requested protocol
        for p in _proxy_cache:
            if p.get("protocol") == protocol:
                return f"{protocol}://{p['host']}:{p['port']}"

    return f"{proto}://{proxy['host']}:{proxy['port']}"


# Per-request HTTP timeout for DDG searches. Without this, a dead SOCKS proxy
# leaves connections stuck in SYN_SENT and a search stage stalls for many
# minutes. Keep it short so a bad proxy fails fast and the pipeline moves on.
DDGS_TIMEOUT = int(os.getenv("DDGS_TIMEOUT", "10"))

# Engines to query. ddgs aggregates across these keyless backends, so one
# engine's blind spot (e.g. DDG not indexing site:crunchbase.com) is covered by
# the others. Set SEARCH_BACKENDS="" to fall back to the ddgs default.
SEARCH_BACKENDS = os.getenv("SEARCH_BACKENDS", "duckduckgo, google, bing, brave, mojeek, yahoo")

# Order in which we attempt each search. "direct" = the server's own IP (fast and
# reliable); "proxy" = a rotating SOCKS proxy (anti-block, but useless when the
# pool is full of dead proxies). Direct-first means a dead proxy never blackholes
# a search; the proxy is only tried if direct returns nothing. Flip to
# "proxy,direct" via env to prioritise IP rotation under heavy volume.
SEARCH_ATTEMPT_ORDER = [
    s.strip() for s in os.getenv("SEARCH_ATTEMPT_ORDER", "direct,proxy").split(",") if s.strip()
]


def _report_proxy_url(proxy_url: str, success: bool) -> None:
    """Score a proxy by its URL (parses host:port for report_proxy)."""
    try:
        from urllib.parse import urlparse
        u = urlparse(proxy_url)
        if u.hostname and u.port:
            report_proxy(u.hostname, u.port, success)
    except Exception:
        pass


class _ResilientDDGS:
    """A drop-in DDGS replacement returned by get_ddgs().

    Adds two robustness layers, transparently, to every caller that does
    `with get_ddgs() as ddgs: ddgs.text(...)`:

      1. Multi-engine — text()/news() default to several keyless engines
         (google, bing, brave, …) instead of one, so a single engine's gap is
         covered by the rest.
      2. Connection fallback — each query is attempted over a sequence of
         connections (direct, then a rotating proxy by default). The first
         attempt that returns results wins; a dead proxy can no longer turn a
         perfectly good query into an empty result.
    """

    def __init__(self, proxy: Optional[str]):
        self._proxy = proxy

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def _attempts(self) -> list:
        seq = []
        for mode in SEARCH_ATTEMPT_ORDER:
            if mode == "direct":
                seq.append(None)
            elif mode == "proxy" and self._proxy:
                seq.append(self._proxy)
        return seq or [None]

    def _run(self, method: str, query: str, **kwargs) -> list:
        from ddgs import DDGS

        if SEARCH_BACKENDS:
            kwargs.setdefault("backend", SEARCH_BACKENDS)

        last_exc = None
        for proxy in self._attempts():
            try:
                with DDGS(proxy=proxy, timeout=DDGS_TIMEOUT) as d:
                    results = list(getattr(d, method)(query, **kwargs))
                if results:
                    if proxy:
                        _report_proxy_url(proxy, True)
                    return results
            except Exception as e:
                last_exc = e
                if proxy:
                    _report_proxy_url(proxy, False)
                continue
        if last_exc:
            logger.debug(f"search '{query[:40]}' exhausted all backends: {last_exc}")

        # Keyless DDG returned nothing (empty or every attempt raised). If any
        # commercial search engine is configured, fall back to it so a blocked /
        # rate-limited DDG doesn't silently produce zero results. With no keys
        # set this is a no-op and behaviour is identical to DDG-only.
        keyed = self._keyed_fallback(method, query, **kwargs)
        if keyed:
            return keyed
        return []

    def _keyed_fallback(self, method: str, query: str, **kwargs) -> list:
        """Try configured keyed engines (SerpAPI / Bing / Google CSE / Brave).

        Only the `text` search maps onto these web-search APIs; other methods
        (news/images/…) have no keyed fallback and return []. Import is lazy and
        guarded so a missing module can never break keyless search.
        """
        if method != "text":
            return []
        try:
            from apps.api.services.leadgen.search_engines import search_fallback
        except Exception as e:  # noqa: BLE001
            logger.debug(f"keyed search fallback unavailable: {e}")
            return []
        max_results = kwargs.get("max_results", 10) or 10
        try:
            return search_fallback(query, max_results=max_results)
        except Exception as e:  # noqa: BLE001
            logger.debug(f"keyed search fallback failed for '{query[:40]}': {e}")
            return []

    def text(self, query, **kwargs):
        return self._run("text", query, **kwargs)

    def news(self, query, **kwargs):
        return self._run("news", query, **kwargs)

    def __getattr__(self, name):
        # Any other DDGS method (images, videos, …): direct connection only.
        def _method(*args, **kwargs):
            from ddgs import DDGS
            with DDGS(timeout=DDGS_TIMEOUT) as d:
                return getattr(d, name)(*args, **kwargs)
        return _method


def get_ddgs(proxy: str = None):
    """Return a resilient, multi-engine search client (see _ResilientDDGS).

    Every search call site uses `with get_ddgs() as ddgs: ddgs.text(...)`, so
    wrapping here upgrades the whole codebase at once: multi-engine backends plus
    direct/proxy connection fallback.
    """
    if proxy is None:
        proxy = get_proxy()
    return _ResilientDDGS(proxy)


def report_proxy(host: str, port: int, success: bool = True):
    """Report proxy outcome to the manager for scoring."""
    try:
        import urllib.request
        import json

        url = f"{PROXY_MANAGER_URL}/api/report"
        data = json.dumps({
            "host": host,
            "port": port,
            "status": "success" if success else "failure",
        }).encode()
        req = urllib.request.Request(
            url,
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        urllib.request.urlopen(req, timeout=2)
    except Exception:
        pass  # Non-critical


def is_proxy_available() -> bool:
    """Check if the proxy manager is running."""
    try:
        import urllib.request
        req = urllib.request.Request(f"{PROXY_MANAGER_URL}/api/stats")
        with urllib.request.urlopen(req, timeout=2) as resp:
            return resp.status == 200
    except Exception:
        return False
