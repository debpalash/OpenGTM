"""
Stealth HTTP Client — Unified fetch with automatic browser-tier escalation.

Usage:
    from apps.api.services.leadgen.http import StealthClient

    client = StealthClient()
    resp = await client.fetch("https://example.com")
    print(resp.text, resp.emails, resp.status_code, resp.tier_used)

Tiers (auto-escalating, Tier1 → Tier2 → Tier3):
    Tier 1: plain HTTP (httpx/aiohttp) with realistic headers — fast, cheap.
    Tier 2: curl_cffi — real Chrome TLS/JA3 fingerprint (defeats fingerprint
            blocks). Falls back to stealth_requests, then plain requests/httpx
            if curl_cffi isn't installed.
    Tier 3: headless browser (patchright/playwright) — full JS rendering for
            JS-empty / challenge pages. OPTIONAL: if the library or browser
            binary is missing, we log ONCE and degrade gracefully (the caller
            keeps the best lower-tier result rather than crashing).
    Tier 3.5: FlareSolverr — last-resort Cloudflare/DDoS-Guard solver, inert
            unless FLARESOLVERR_URL is set.

Escalation triggers (see ``_is_challenge``): blocked status codes, Cloudflare /
Datadome / reCAPTCHA markers, "enable javascript" pages, and suspiciously tiny
HTML bodies (JS-rendered shells that returned almost no content).
"""

import asyncio
import logging
import re
from dataclasses import dataclass, field
from typing import Optional
from urllib.parse import urlparse

from apps.api.services.leadgen.proxy_pool import ProxyPool
from apps.api.services.leadgen.rate_limiter import RateLimiter

logger = logging.getLogger("leadgen.http")

# Optional-dependency "log once" guard. We never want to spam logs (or crash)
# when an optional browser/TLS library is absent on a given deployment.
_warned_missing: set[str] = set()


def _warn_once(key: str, message: str) -> None:
    """Emit ``message`` at most once per process for a given ``key``."""
    if key not in _warned_missing:
        _warned_missing.add(key)
        logger.warning(message)


# ── Challenge Detection ──────────────────────────────────────────────

CHALLENGE_MARKERS = [
    "Just a moment",
    "cf-challenge",
    "cf_clearance",
    "__cf_bm",
    "challenge-platform",
    "Checking your browser",
    "Attention Required",
    "Access Denied",
    "Please verify you are a human",
    "captcha-delivery.com",
    "geo.captcha-delivery.com",
]

# Markers that signal a JS-gated page that returned no usable content over HTTP.
# These require a real browser (Tier 3) to render.
JS_REQUIRED_MARKERS = [
    "enable javascript",
    "please enable javascript",
    "javascript is required",
    "javascript is disabled",
    "you need to enable javascript",
    "<noscript>",
]

BLOCKED_STATUS_CODES = {403, 429, 503, 520, 521, 522, 523, 524}

# Below this body size, a 2xx HTML response is almost certainly a JS shell /
# bot wall rather than real content — worth escalating to a browser.
TINY_BODY_THRESHOLD = 500


def _is_challenge(status_code: int, text: str) -> bool:
    """Detect a bot-challenge or JS-empty page that warrants browser escalation.

    Returns True when:
      * the status code is a known block (403/429/503/52x), OR
      * the body contains a Cloudflare/Datadome/captcha marker, OR
      * the body contains an "enable javascript" marker, OR
      * a 2xx response has a suspiciously tiny body (JS-rendered shell).
    """
    if status_code in BLOCKED_STATUS_CODES:
        return True

    head = text[:5000]
    for marker in CHALLENGE_MARKERS:
        if marker in head:
            return True

    lower = head.lower()
    for marker in JS_REQUIRED_MARKERS:
        if marker in lower:
            return True

    # Tiny-body heuristic: a 2xx HTML page with almost no content is usually a
    # JS shell that needs a browser to populate the DOM.
    if 200 <= status_code < 300 and 0 < len(text.strip()) < TINY_BODY_THRESHOLD:
        return True

    return False


# ── Response Wrapper ─────────────────────────────────────────────────

@dataclass
class FetchResult:
    """Unified response from any tier."""
    url: str = ""
    status_code: int = 0
    text: str = ""
    tier_used: int = 0
    proxy_used: str = ""
    error: str = ""
    emails: list = field(default_factory=list)
    phones: list = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return 200 <= self.status_code < 400 and not self.error

    def extract_emails(self) -> list[str]:
        """Extract emails from response text."""
        if self.emails:
            return self.emails
        pattern = r'[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,24}'
        matches = re.findall(pattern, self.text)
        # Filter out false positives
        self.emails = [
            e for e in set(matches)
            if not any(x in e.lower() for x in [
                'example.com', 'domain.com', '.png', '.jpg', '.css', '.js',
                'sentry.io', 'wixpress', 'cloudflare',
            ])
        ]
        return self.emails

    def extract_phones(self) -> list[str]:
        """Extract phone numbers from response text."""
        if self.phones:
            return self.phones
        patterns = [
            r'\+?91[\-\s]?\d{5}[\-\s]?\d{5}',
            r'\+?91[\-\s]?\d{10}',
            r'1800[\-\s]?\d{2,3}[\-\s]?\d{4,6}',
            r'\b\d{10}\b',
        ]
        found = []
        for pat in patterns:
            for m in re.finditer(pat, self.text):
                digits = re.sub(r'\D', '', m.group())
                if 7 <= len(digits) <= 13:
                    found.append(m.group().strip())
        self.phones = list(dict.fromkeys(found))[:10]
        return self.phones


# ── Stealth Client ───────────────────────────────────────────────────

_DEFAULT_HEADERS = {
    "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
    "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
    "Accept-Language": "en-US,en;q=0.9",
    "Accept-Encoding": "gzip, deflate",
    "DNT": "1",
}


class StealthClient:
    """
    Unified stealth HTTP client with automatic browser-tier escalation.

    Tier 1: plain HTTP (httpx/aiohttp) with realistic headers — fast, cheap.
    Tier 2: curl_cffi — real Chrome TLS/JA3 fingerprint (stealth_requests and
            plain requests/httpx as graceful fallbacks).
    Tier 3: patchright/playwright headless browser — full JS rendering.

    ``fetch`` tries each tier in order and escalates only when a tier returns a
    challenge / JS-empty page (see ``_is_challenge``). The browser tier is
    OPTIONAL: when the library or browser binary is missing it is skipped with a
    one-time log line, and the best lower-tier result is returned.
    """

    def __init__(
        self,
        proxy_pool: Optional[ProxyPool] = None,
        rate_limiter: Optional[RateLimiter] = None,
        max_retries: int = 2,
    ):
        self.proxy_pool = proxy_pool or ProxyPool()
        self.rate_limiter = rate_limiter or RateLimiter()
        self.max_retries = max_retries

    async def fetch(
        self,
        url: str,
        tier: int = 2,
        use_proxy: bool = True,
        timeout: int = 15,
    ) -> FetchResult:
        """
        Fetch a URL, escalating Tier1 → Tier2 → Tier3 as needed.

        Args:
            url: Target URL.
            tier: Highest tier to start at. ``tier<=2`` runs the cheap HTTP
                tiers first and escalates to the browser on a challenge.
                ``tier>=3`` jumps straight to the browser.
            use_proxy: Whether to use proxy rotation.
            timeout: Per-tier request timeout, in seconds. The browser tier
                gets a slightly larger budget since it must render JS.

        Returns:
            FetchResult with response data and ``tier_used`` set to whichever
            tier produced the returned body.
        """
        domain = urlparse(url).netloc
        result = FetchResult(url=url)

        # Rate limiting
        if self.rate_limiter.is_blocked(domain):
            result.error = f"Circuit breaker tripped for {domain}"
            return result

        await self.rate_limiter.acquire(domain)

        # Get proxy
        proxy = None
        if use_proxy and self.proxy_pool and len(self.proxy_pool) > 0:
            proxy = self.proxy_pool.get_proxy(domain, tier=tier)

        # ── Tiers 1 & 2: cheap HTTP, escalate on challenge ──────────────
        if tier <= 2:
            result = await self._fetch_http(url, proxy, timeout)
            if result.ok and not _is_challenge(result.status_code, result.text):
                logger.debug("fetch %s succeeded at tier %d", url, result.tier_used)
                self._report_ok(domain, proxy)
                return result

            # Challenge / JS-empty detected — escalate to the browser tier.
            if _is_challenge(result.status_code, result.text):
                logger.info(
                    "fetch %s: tier %d hit a challenge/JS-empty page (status=%s) — "
                    "escalating to browser tier 3",
                    url, result.tier_used, result.status_code,
                )
                browser_proxy = proxy
                if proxy:
                    browser_proxy = self.proxy_pool.get_proxy(domain, tier=3)
                browser_result = await self._fetch_tier3(url, browser_proxy, timeout + 10)
                # Only adopt the browser result if it actually improved things;
                # otherwise keep the lower-tier body for the caller to inspect.
                if browser_result.ok or not result.text:
                    result = browser_result
                proxy = browser_proxy

        # ── Tier 3: browser only ────────────────────────────────────────
        else:
            result = await self._fetch_tier3(url, proxy, timeout + 10)

        # ── Tier 3.5: FlareSolverr — last resort for Cloudflare/DDoS-Guard.
        # Inert unless FLARESOLVERR_URL is set; only fires when still challenged.
        if (not result.ok) and _is_challenge(result.status_code, result.text):
            fs_result = await self._fetch_flaresolverr(url, proxy)
            if fs_result is not None:
                result = fs_result

        # Report results
        if result.ok:
            logger.debug("fetch %s succeeded at tier %d", url, result.tier_used)
            self._report_ok(domain, proxy)
        else:
            logger.info(
                "fetch %s failed at all tiers (last tier=%d status=%s error=%s)",
                url, result.tier_used, result.status_code, result.error or "-",
            )
            self.rate_limiter.report_failure(domain)
            if proxy:
                self.proxy_pool.report_blocked(proxy, domain)

        return result

    def _report_ok(self, domain: str, proxy: Optional[str]) -> None:
        self.rate_limiter.report_success(domain)
        if proxy:
            self.proxy_pool.report_success(proxy, domain)

    async def _fetch_http(self, url: str, proxy: Optional[str], timeout: int) -> FetchResult:
        """Tiers 1-2: HTTP fetch with the best available TLS-fingerprint stack.

        Order of preference (all optional except the final httpx fallback):
          1. curl_cffi  — impersonates a real Chrome TLS/JA3 handshake (Tier 2).
          2. stealth_requests — curl_cffi-based session with built-in extractors.
          3. requests / httpx / aiohttp — plain HTTP with realistic headers (Tier 1).

        We degrade silently (one-time log) so deployments without curl_cffi keep
        working via plain HTTP.
        """
        # 1) curl_cffi — real browser TLS/JA3 fingerprint. (Tier 2)
        cffi_result = await self._fetch_curl_cffi(url, proxy, timeout)
        if cffi_result is not None:
            return cffi_result

        # 2) stealth_requests — curl_cffi session wrapper with email/phone extractors.
        sr_result = await self._fetch_stealth_requests(url, proxy, timeout)
        if sr_result is not None:
            return sr_result

        # 3) plain HTTP fallback. (Tier 1)
        return await self._fetch_plain(url, proxy, timeout)

    async def _fetch_curl_cffi(self, url: str, proxy: Optional[str], timeout: int) -> Optional[FetchResult]:
        """Tier 2: fetch via curl_cffi with a Chrome TLS/JA3 impersonation.

        Returns None when curl_cffi isn't installed (so the caller can fall
        back to a cheaper tier). curl_cffi is a synchronous client, so we run it
        in a thread to keep the event loop responsive.
        """
        try:
            from curl_cffi import requests as cffi_requests
        except ImportError:
            _warn_once(
                "curl_cffi",
                "curl_cffi not installed — Tier 2 TLS impersonation unavailable; "
                "falling back to plain HTTP. Install with: uv add curl-cffi",
            )
            return None

        result = FetchResult(url=url, tier_used=2, proxy_used=proxy or "")

        def _do_request() -> tuple[int, str]:
            proxies = {"http": proxy, "https": proxy} if proxy else None
            resp = cffi_requests.get(
                url,
                impersonate="chrome",
                timeout=timeout,
                proxies=proxies,
                allow_redirects=True,
            )
            return resp.status_code, resp.text

        try:
            status_code, text = await asyncio.to_thread(_do_request)
            result.status_code = status_code
            result.text = text
        except Exception as e:  # network error, bad proxy, etc.
            result.error = str(e)
            result.status_code = 0
        return result

    async def _fetch_stealth_requests(self, url: str, proxy: Optional[str], timeout: int) -> Optional[FetchResult]:
        """Optional Tier 2 variant: stealth_requests (curl_cffi session wrapper).

        Returns None when not installed.
        """
        try:
            from stealth_requests.session import AsyncStealthSession
        except ImportError:
            return None

        result = FetchResult(url=url, tier_used=2, proxy_used=proxy or "")
        try:
            async with AsyncStealthSession(timeout=timeout) as session:
                kwargs = {}
                if proxy:
                    kwargs["proxy"] = proxy
                resp = await session.get(url, retry=self.max_retries, **kwargs)
                result.status_code = resp.status_code
                result.text = resp.text
                try:
                    result.emails = list(resp.emails or [])
                except Exception:
                    pass
                try:
                    result.phones = list(resp.phone_numbers or [])
                except Exception:
                    pass
        except Exception as e:
            result.error = str(e)
            result.status_code = 0
        return result

    async def _fetch_plain(self, url: str, proxy: Optional[str], timeout: int) -> FetchResult:
        """Tier 1: plain HTTP with realistic headers via httpx, then aiohttp."""
        result = FetchResult(url=url, tier_used=1, proxy_used=proxy or "")

        # Prefer httpx (a root dependency).
        try:
            import httpx

            async with httpx.AsyncClient(
                headers=_DEFAULT_HEADERS,
                timeout=timeout,
                follow_redirects=True,
                proxy=proxy or None,
                verify=False,
            ) as client:
                resp = await client.get(url)
                result.status_code = resp.status_code
                result.text = resp.text
            return result
        except ImportError:
            pass
        except Exception as e:
            result.error = str(e)
            result.status_code = 0
            return result

        # Final fallback: aiohttp.
        try:
            import aiohttp

            conn = aiohttp.TCPConnector(ssl=False)
            async with aiohttp.ClientSession(connector=conn, headers=_DEFAULT_HEADERS) as session:
                async with session.get(
                    url,
                    timeout=aiohttp.ClientTimeout(total=timeout),
                    allow_redirects=True,
                    proxy=proxy or None,
                ) as resp:
                    result.status_code = resp.status
                    result.text = await resp.text(errors="replace")
        except Exception as e:
            result.error = str(e)
            result.status_code = 0
        return result

    # Backwards-compatible alias — older callers / tests may reference _fetch_tier2.
    async def _fetch_tier2(self, url: str, proxy: Optional[str], timeout: int) -> FetchResult:
        """Deprecated alias for the HTTP tier stack (Tiers 1-2)."""
        return await self._fetch_http(url, proxy, timeout)

    async def _fetch_flaresolverr(self, url: str, proxy: Optional[str]) -> Optional[FetchResult]:
        """Tier 3.5: solve a Cloudflare/DDoS-Guard challenge via FlareSolverr.

        Returns None when the service isn't configured (callers keep the prior result).
        """
        try:
            from apps.api.services.leadgen.scrapers import flaresolverr_client as fs
        except Exception:
            return None
        if not fs.is_available():
            return None

        sol = await fs.solve(url, proxy=proxy)
        if not sol.ok:
            return None
        result = FetchResult(url=sol.url or url, tier_used=4, proxy_used=proxy or "")
        result.status_code = sol.status_code or 200
        result.text = sol.html
        return result

    @staticmethod
    def _load_async_playwright():
        """Return an ``async_playwright`` callable, preferring patchright.

        Patchright is a stealth-patched Playwright fork; if it isn't installed we
        fall back to vanilla playwright (a root dependency). Returns None when
        neither is available so the caller can degrade gracefully.
        """
        try:
            from patchright.async_api import async_playwright
            return async_playwright
        except ImportError:
            pass
        try:
            from playwright.async_api import async_playwright
            return async_playwright
        except ImportError:
            return None

    async def _fetch_tier3(self, url: str, proxy: Optional[str], timeout: int) -> FetchResult:
        """Tier 3: headless-browser fetch via patchright (or playwright).

        OPTIONAL tier. If neither browser library is importable, or the browser
        binary hasn't been installed, we log ONCE and return an errored result
        (status 0) — never raising — so the caller keeps its lower-tier body.
        """
        result = FetchResult(url=url, tier_used=3, proxy_used=proxy or "")

        async_playwright = self._load_async_playwright()
        if async_playwright is None:
            _warn_once(
                "browser",
                "No headless browser available (patchright/playwright not installed) — "
                "Tier 3 escalation disabled; keeping HTTP-tier results. "
                "Install with: uv add patchright && uv run patchright install chromium",
            )
            result.error = "browser not installed"
            return result

        try:
            async with async_playwright() as p:
                launch_args = {}
                if proxy:
                    # Parse proxy for Playwright format
                    parsed = urlparse(proxy)
                    launch_args["proxy"] = {
                        "server": f"{parsed.scheme}://{parsed.hostname}:{parsed.port}",
                    }
                    if parsed.username:
                        launch_args["proxy"]["username"] = parsed.username
                        launch_args["proxy"]["password"] = parsed.password or ""

                browser = await p.chromium.launch(headless=True, **launch_args)
                context = await browser.new_context(
                    viewport={"width": 1280, "height": 900},
                    locale="en-US",
                )
                page = await context.new_page()

                try:
                    resp = await page.goto(url, wait_until="domcontentloaded", timeout=timeout * 1000)
                    # Wait a bit for JS to settle
                    await asyncio.sleep(2)

                    result.status_code = resp.status if resp else 0
                    result.text = await page.content()
                except Exception as e:
                    result.error = str(e)
                finally:
                    await browser.close()

        except Exception as e:
            # Most commonly the browser *binary* isn't installed (the Python lib
            # is). Log once so we don't crash the whole fetch on every URL.
            msg = str(e)
            if "Executable doesn't exist" in msg or "playwright install" in msg:
                _warn_once(
                    "browser_binary",
                    "Headless browser binary missing — Tier 3 disabled. "
                    "Run: uv run patchright install chromium",
                )
            result.error = msg

        return result

    async def fetch_many(self, urls: list[str], concurrency: int = 3, **kwargs) -> list[FetchResult]:
        """Fetch multiple URLs with controlled concurrency."""
        sem = asyncio.Semaphore(concurrency)

        async def _limited_fetch(url):
            async with sem:
                return await self.fetch(url, **kwargs)

        return await asyncio.gather(*[_limited_fetch(u) for u in urls])
