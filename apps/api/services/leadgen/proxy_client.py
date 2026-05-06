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


def get_ddgs(proxy: str = None):
    """Get a DDGS instance with proxy rotation.

    If proxy manager is available, uses a rotating proxy.
    Falls back to direct connection if unavailable.
    """
    from ddgs import DDGS

    if proxy is None:
        proxy = get_proxy()

    if proxy:
        logger.debug(f"DDGS using proxy: {proxy}")
        return DDGS(proxy=proxy)
    else:
        return DDGS()


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
