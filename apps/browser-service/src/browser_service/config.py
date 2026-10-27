"""Service settings, read from the environment once at start.

The service holds no secrets other than its own bearer token: credentials a
scrape needs (cookies) arrive in the request and are never stored or logged.
"""

from __future__ import annotations

import ipaddress
import os
from collections.abc import Mapping
from dataclasses import dataclass

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

    def __repr__(self) -> str:  # keep the token out of logs and tracebacks
        return (
            f"Settings(host={self.host!r}, port={self.port}, max_concurrency={self.max_concurrency}, token=[redacted])"
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

        cidrs_raw = env.get("BROWSER_EGRESS_ALLOW_CIDRS", "").strip()
        cidrs: tuple = ()
        if cidrs_raw:
            if env.get("APP_ENV", "").strip().lower() != "test":
                raise ConfigError("BROWSER_EGRESS_ALLOW_CIDRS is only honoured with APP_ENV=test")
            try:
                cidrs = tuple(ipaddress.ip_network(c.strip(), strict=False) for c in cidrs_raw.split(",") if c.strip())
            except ValueError as e:
                raise ConfigError("BROWSER_EGRESS_ALLOW_CIDRS is not a list of CIDRs") from e

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
            flaresolverr_url=env.get("FLARESOLVERR_URL", "").strip().rstrip("/"),
            search_backends=env.get("BROWSER_SEARCH_BACKENDS", cls.search_backends),
            search_timeout_s=num("BROWSER_SEARCH_TIMEOUT_S", 10, 1, 60),
        )
