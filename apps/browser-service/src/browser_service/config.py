"""Service settings, read from the environment once at start.

The service holds no secrets other than its own bearer token: credentials a
scrape needs (cookies) arrive in the request and are never stored or logged.
"""

from __future__ import annotations

import ipaddress
import os
from collections.abc import Mapping
from dataclasses import dataclass
from urllib.parse import urlsplit

MIN_TOKEN_LEN = 32
MiB = 1 << 20


class ConfigError(Exception):
    """Invalid configuration; the service refuses to start."""


@dataclass(frozen=True)
class Settings:
    token: str
    host: str = "127.0.0.1"
    port: int = 8090
    max_concurrency: int = 4
    queue_wait_s: float = 5.0
    max_timeout_s: float = 180.0
    max_response_bytes: int = 5 * MiB
    max_request_bytes: int = 256 * 1024
    max_download_bytes: int = 64 * MiB
    egress_allow_cidrs: tuple[ipaddress.IPv4Network | ipaddress.IPv6Network, ...] = ()
    flaresolverr_url: str = ""
    search_backends: str = "duckduckgo, google, bing, brave, mojeek, yahoo"
    search_timeout_s: int = 10
    launch_browser: bool = True
    # Where the egress proxy listens and the URL other processes (FlareSolverr)
    # use to reach it. Loopback unless FlareSolverr is enabled.
    egress_proxy_host: str = "127.0.0.1"
    egress_proxy_port: int = 0
    egress_proxy_url: str = ""
    # /v1/extract takes page HTML in the body, so it gets its own body cap.
    max_extract_bytes: int = 8 * MiB
    # LinkedIn roster scraping (staffspy) runs against LinkedIn's ToS: off unless
    # the operator opts in (legacy STAFFSPY_ENABLED; the cookies come from Go).
    staffspy_enabled: bool = False
    # Google Maps through patchright (stealth-patched Playwright). Off by default:
    # the deployed legacy image had only vanilla Playwright (patchright was the
    # optional `browser` extra), so off matches what production ran.
    maps_stealth: bool = False
    # Chromium's own sandbox (review M1). In a container it needs a seccomp
    # profile that allows user namespaces (seccomp-chromium.json).
    chromium_sandbox: bool = True
    # Split mode: Chromium runs as its own uid behind a loopback Playwright /
    # patchright server started by scripts/entrypoint.sh (ws://127.0.0.1:port/path).
    playwright_ws: str = ""
    patchright_ws: str = ""
    # destination ports the egress proxy may open (BROWSER_EGRESS_PORTS)
    egress_ports: frozenset[int] = frozenset({80, 443})

    def __repr__(self) -> str:  # keep the token and the server paths out of logs and tracebacks
        return (
            f"Settings(host={self.host!r}, port={self.port}, max_concurrency={self.max_concurrency}, "
            f"split={'yes' if self.playwright_ws else 'no'}, token=[redacted])"
        )

    @classmethod
    def from_env(cls, env: Mapping[str, str] | None = None) -> Settings:
        env = os.environ if env is None else env
        token = env.get("BROWSER_SERVICE_TOKEN", "")
        if len(token) < MIN_TOKEN_LEN:
            raise ConfigError(f"BROWSER_SERVICE_TOKEN must be set (>= {MIN_TOKEN_LEN} characters)")

        def num(name: str, default, lo, hi, conv=int):
            raw = env.get(name, "").strip()
            if not raw:
                return default
            try:
                v = conv(raw)
            except ValueError as e:
                raise ConfigError(f"{name} must be a number") from e
            if not lo <= v <= hi:
                raise ConfigError(f"{name} must be between {lo} and {hi}")
            return v

        def flag(name: str, default: bool) -> bool:
            raw = env.get(name, "").strip().lower()
            if not raw:
                return default
            if raw in ("1", "true", "yes", "on"):
                return True
            if raw in ("0", "false", "no", "off"):
                return False
            raise ConfigError(f"{name} must be a boolean")

        cidrs_raw = env.get("BROWSER_EGRESS_ALLOW_CIDRS", "").strip()
        cidrs: tuple = ()
        if cidrs_raw:
            if env.get("APP_ENV", "").strip().lower() != "test":
                raise ConfigError("BROWSER_EGRESS_ALLOW_CIDRS is only honoured with APP_ENV=test")
            try:
                cidrs = tuple(ipaddress.ip_network(c.strip(), strict=False) for c in cidrs_raw.split(",") if c.strip())
            except ValueError as e:
                raise ConfigError("BROWSER_EGRESS_ALLOW_CIDRS is not a list of CIDRs") from e

        def loopback_ws(name: str) -> str:
            raw = env.get(name, "").strip()
            if not raw:
                return ""
            try:
                u = urlsplit(raw)
                port = u.port
            except ValueError as e:
                raise ConfigError(f"{name} must be ws://127.0.0.1:<port>/<path>") from e
            if u.scheme != "ws" or u.hostname not in ("127.0.0.1", "localhost", "::1") or not port:
                raise ConfigError(f"{name} must be ws://127.0.0.1:<port>/<path>")
            return raw

        ports_raw = env.get("BROWSER_EGRESS_PORTS", "").strip()
        egress_ports = frozenset({80, 443})
        if ports_raw:
            try:
                egress_ports = frozenset(int(p) for p in ports_raw.split(",") if p.strip())
            except ValueError as e:
                raise ConfigError("BROWSER_EGRESS_PORTS must be a comma-separated port list") from e
            if not egress_ports or not all(0 < p < 65536 for p in egress_ports):
                raise ConfigError("BROWSER_EGRESS_PORTS must be a comma-separated port list")

        flaresolverr_url = env.get("FLARESOLVERR_URL", "").strip().rstrip("/")
        proxy_host, proxy_port = "127.0.0.1", 0
        listen = env.get("BROWSER_EGRESS_PROXY_LISTEN", "").strip()
        if listen:
            h, sep, p = listen.rpartition(":")
            if not sep or not h or not p.isdigit() or not 0 < int(p) < 65536:
                raise ConfigError("BROWSER_EGRESS_PROXY_LISTEN must be host:port")
            proxy_host, proxy_port = h.strip("[]"), int(p)
        proxy_url = env.get("BROWSER_EGRESS_PROXY_URL", "").strip().rstrip("/")
        if flaresolverr_url and not (listen and proxy_url):
            # FlareSolverr runs its own browser in another container: it may only
            # fetch through our egress proxy, so the proxy must be reachable.
            raise ConfigError(
                "FLARESOLVERR_URL needs BROWSER_EGRESS_PROXY_LISTEN and BROWSER_EGRESS_PROXY_URL "
                "(FlareSolverr fetches only through the egress proxy)"
            )
        if proxy_url and not proxy_url.startswith("http://"):
            raise ConfigError("BROWSER_EGRESS_PROXY_URL must be an http:// URL")

        return cls(
            token=token,
            host=env.get("BROWSER_SERVICE_HOST", "127.0.0.1").strip() or "127.0.0.1",
            port=num("BROWSER_SERVICE_PORT", 8090, 1, 65535),
            max_concurrency=num("BROWSER_MAX_CONCURRENCY", 4, 1, 64),
            queue_wait_s=num("BROWSER_QUEUE_WAIT_S", 5.0, 0.0, 60.0, float),
            max_timeout_s=num("BROWSER_MAX_TIMEOUT_S", 180.0, 5.0, 600.0, float),
            max_response_bytes=num("BROWSER_MAX_RESPONSE_BYTES", 5 * MiB, 64 * 1024, 64 * MiB),
            max_download_bytes=num("BROWSER_MAX_DOWNLOAD_BYTES", 64 * MiB, MiB, 1024 * MiB),
            egress_allow_cidrs=cidrs,
            flaresolverr_url=flaresolverr_url,
            egress_proxy_host=proxy_host,
            egress_proxy_port=proxy_port,
            egress_proxy_url=proxy_url,
            search_backends=env.get("BROWSER_SEARCH_BACKENDS", cls.search_backends),
            search_timeout_s=num("BROWSER_SEARCH_TIMEOUT_S", 10, 1, 60),
            max_extract_bytes=num("BROWSER_MAX_EXTRACT_BYTES", 8 * MiB, 64 * 1024, 64 * MiB),
            staffspy_enabled=flag("BROWSER_STAFFSPY_ENABLED", False),
            maps_stealth=flag("BROWSER_MAPS_STEALTH", False),
            egress_ports=egress_ports,
            chromium_sandbox=flag("BROWSER_CHROMIUM_SANDBOX", True),
            playwright_ws=loopback_ws("BROWSER_PLAYWRIGHT_WS"),
            patchright_ws=loopback_ws("BROWSER_PATCHRIGHT_WS"),
        )
