"""
Proxy Pool Manager — Rotates proxies with domain bucketing and health tracking.

Sources:
- research/fresh-proxy-list/ (auto-updated txt files)
- Custom proxies from environment (PROXY_LIST env var)

Strategy:
- Same proxy sticks to same domain (builds trust)
- Health checks with cooldown on blocked proxies
- Tier-aware: free proxies for HTTP, better for browser
"""

import os
import random
import time
from collections import defaultdict
from pathlib import Path
from typing import Optional


class ProxyPool:
    """Manages proxy rotation with domain affinity and health tracking."""

    def __init__(self, proxy_dir: Optional[str] = None):
        self._proxies = {"http": [], "socks5": [], "socks4": []}
        self._domain_map: dict[str, str] = {}          # domain → assigned proxy
        self._blocked: dict[str, float] = {}            # proxy → cooldown_until
        self._cooldown_seconds = 1800                    # 30 min cooldown

        # Load from fresh-proxy-list repo
        if proxy_dir is None:
            proxy_dir = str(Path(__file__).parent.parent / "research" / "fresh-proxy-list")

        self._load_file(proxy_dir, "http.txt", "http")
        self._load_file(proxy_dir, "https.txt", "http")
        self._load_file(proxy_dir, "socks5.txt", "socks5")
        self._load_file(proxy_dir, "socks4.txt", "socks4")

        # Load custom proxies from env
        custom = os.getenv("PROXY_LIST", "")
        if custom:
            for p in custom.split(","):
                p = p.strip()
                if p:
                    if "socks5" in p:
                        self._proxies["socks5"].append(p)
                    elif "socks4" in p:
                        self._proxies["socks4"].append(p)
                    else:
                        self._proxies["http"].append(p)

        self._all = (
            [f"http://{p}" for p in self._proxies["http"]]
            + [f"socks5://{p}" for p in self._proxies["socks5"]]
            + [f"socks4://{p}" for p in self._proxies["socks4"]]
        )

    def _load_file(self, directory: str, filename: str, proto: str):
        path = Path(directory) / filename
        if not path.exists():
            return
        with open(path) as f:
            for line in f:
                line = line.strip()
                if line and ":" in line:
                    self._proxies[proto].append(line)

    def __len__(self):
        return len(self._all)

    def _is_available(self, proxy: str) -> bool:
        """Check if proxy is not in cooldown."""
        cooldown_until = self._blocked.get(proxy)
        if cooldown_until is None:
            return True
        if time.time() > cooldown_until:
            del self._blocked[proxy]
            return True
        return False

    def get_proxy(self, domain: str = "", tier: int = 2) -> Optional[str]:
        """
        Get a proxy for the given domain.

        Args:
            domain: Target domain (for affinity)
            tier: 1-2 = free proxies fine, 3 = prefer socks5
        """
        if not self._all:
            return None

        # Check if domain already has an assigned proxy
        if domain and domain in self._domain_map:
            assigned = self._domain_map[domain]
            if self._is_available(assigned):
                return assigned

        # Pick pool based on tier
        if tier >= 3 and self._proxies["socks5"]:
            pool = [f"socks5://{p}" for p in self._proxies["socks5"]]
        else:
            pool = self._all

        # Filter available proxies
        available = [p for p in pool if self._is_available(p)]
        if not available:
            # All blocked — try any
            available = pool

        proxy = random.choice(available)

        # Assign to domain
        if domain:
            self._domain_map[domain] = proxy

        return proxy

    def report_blocked(self, proxy: str, domain: str = ""):
        """Mark proxy as blocked — enters cooldown."""
        self._blocked[proxy] = time.time() + self._cooldown_seconds
        # Remove domain assignment so it gets a new proxy
        if domain and self._domain_map.get(domain) == proxy:
            del self._domain_map[domain]

    def report_success(self, proxy: str, domain: str = ""):
        """Mark proxy as working for this domain."""
        if domain:
            self._domain_map[domain] = proxy
        # Remove from blocked if it was there
        self._blocked.pop(proxy, None)

    def stats(self) -> dict:
        """Return pool statistics."""
        return {
            "total": len(self._all),
            "http": len(self._proxies["http"]),
            "socks5": len(self._proxies["socks5"]),
            "socks4": len(self._proxies["socks4"]),
            "blocked": len(self._blocked),
            "domain_assignments": len(self._domain_map),
        }
