"""Chromium lifecycle: one shared browser process, one fresh context per API
request (no cookies, storage or cache shared between requests), every context
routed through the egress proxy with that request's own credentials."""

from __future__ import annotations

import asyncio
import importlib
import logging
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from typing import Any

from .egress import EgressProxy, EgressSession

logger = logging.getLogger("browser_service.browser")

DEFAULT_UA = (
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/125.0.0.0 Safari/537.36"
)

# Flags that keep Chromium from opening connections outside the proxy and make
# the headless browser a little less obviously automated.
CHROMIUM_ARGS = [
    "--proxy-bypass-list=<-loopback>",  # loopback goes through the proxy too
    "--disable-quic",
    "--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
    "--webrtc-ip-handling-policy=disable_non_proxied_udp",
    "--disable-background-networking",
    "--disable-component-update",
    "--disable-domain-reliability",
    "--disable-sync",
    "--no-first-run",
    "--no-default-browser-check",
    "--metrics-recording-only",
    "--disable-features=DnsOverHttps,NetworkPrediction,MediaRouter,OptimizationHints",
    "--disable-blink-features=AutomationControlled",
]


# patchright patches the automation tells itself and asks for no extra
# anti-detection flags; it keeps every flag that pins traffic to the proxy.
STEALTH_ARGS = [a for a in CHROMIUM_ARGS if not a.startswith("--disable-blink-features")]

DRIVERS = {"playwright": "playwright.async_api", "patchright": "patchright.async_api"}


class BrowserUnavailable(Exception):
    pass


class BrowserPool:
    def __init__(self, proxy: EgressProxy, *, headless: bool = True, driver: str = "playwright") -> None:
        if driver not in DRIVERS:
            raise ValueError("unknown browser driver")
        self.proxy = proxy
        self.headless = headless
        self.driver = driver
        self._pw: Any = None
        self._browser: Any = None
        self._lock = asyncio.Lock()

    @property
    def ready(self) -> bool:
        return self._browser is not None and self._browser.is_connected()

    async def start(self) -> None:
        try:
            await self._ensure()
        except BrowserUnavailable:
            logger.warning("chromium did not start; browser endpoints answer 503 until it does")

    async def stop(self) -> None:
        async with self._lock:
            if self._browser is not None:
                try:
                    await self._browser.close()
                except Exception:  # pragma: no cover
                    logger.debug("browser close failed", exc_info=True)
                self._browser = None
            if self._pw is not None:
                await self._pw.stop()
                self._pw = None

    async def _ensure(self) -> Any:
        if self.ready:
            return self._browser
        async with self._lock:
            if self.ready:
                return self._browser
            try:
                if self._pw is None:
                    mod = importlib.import_module(DRIVERS[self.driver])
                    self._pw = await mod.async_playwright().start()
                self._browser = await self._pw.chromium.launch(
                    headless=self.headless,
                    proxy={"server": self.proxy.server_url},
                    args=STEALTH_ARGS if self.driver == "patchright" else CHROMIUM_ARGS,
                )
            except Exception as e:
                logger.error("chromium launch failed: %s", type(e).__name__)
                raise BrowserUnavailable from e
            return self._browser

    @asynccontextmanager
    async def page(
        self,
        session: EgressSession,
        *,
        user_agent: str | None = None,
        locale: str = "en-US",
        viewport: tuple[int, int] = (1280, 900),
        cookies: list[dict] | None = None,
    ) -> AsyncIterator[Any]:
        browser = await self._ensure()
        try:
            ctx = await browser.new_context(
                proxy={"server": self.proxy.server_url, "username": session.sid, "password": session.secret},
                # patchright's own fingerprint is consistent; overriding the UA would break it
                user_agent=user_agent or (None if self.driver == "patchright" else DEFAULT_UA),
                locale=locale,
                viewport={"width": viewport[0], "height": viewport[1]},
                service_workers="block",
                accept_downloads=False,
            )
        except Exception as e:
            raise BrowserUnavailable from e
        try:
            if cookies:
                await ctx.add_cookies(cookies)
            page = await ctx.new_page()
            yield page
        finally:
            try:
                await ctx.close()
            except Exception:  # pragma: no cover
                logger.debug("context close failed", exc_info=True)
