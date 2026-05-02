"""
Stealth HTTP Client — Unified fetch with automatic tier escalation.

Usage:
    from apps.api.services.leadgen.http import StealthClient

    client = StealthClient()
    resp = await client.fetch("https://example.com")
    print(resp.text, resp.emails, resp.status_code)

Tier 2 (default): stealth_requests — Chrome TLS impersonation via curl_cffi
Tier 3 (fallback): patchright browser — full JS rendering for challenge pages

Challenge detection: Cloudflare, Datadome, reCAPTCHA markers
"""

import asyncio
import re
from dataclasses import dataclass, field
from typing import Optional
from urllib.parse import urlparse

from apps.api.services.leadgen.proxy_pool import ProxyPool
from apps.api.services.leadgen.rate_limiter import RateLimiter


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

BLOCKED_STATUS_CODES = {403, 429, 503, 520, 521, 522, 523, 524}


def _is_challenge(status_code: int, text: str) -> bool:
    """Detect if response is a bot challenge page."""
    if status_code in BLOCKED_STATUS_CODES:
        return True
    for marker in CHALLENGE_MARKERS:
        if marker in text[:5000]:
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

class StealthClient:
    """
    Unified stealth HTTP client with tier escalation.

    Tier 2: stealth_requests (curl_cffi) — fast, no browser
    Tier 3: patchright browser — full JS rendering

    Auto-escalates from Tier 2 → Tier 3 on challenge detection.
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
        Fetch a URL with stealth.

        Args:
            url: Target URL
            tier: Starting tier (2=HTTP, 3=browser)
            use_proxy: Whether to use proxy rotation
            timeout: Request timeout in seconds

        Returns:
            FetchResult with response data
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

        # Tier 2: HTTP stealth
        if tier <= 2:
            result = await self._fetch_tier2(url, proxy, timeout)
            if result.ok:
                self.rate_limiter.report_success(domain)
                if proxy:
                    self.proxy_pool.report_success(proxy, domain)
                return result

            # Challenge detected — escalate to Tier 3
            if _is_challenge(result.status_code, result.text):
                if proxy:
                    proxy = self.proxy_pool.get_proxy(domain, tier=3)
                result = await self._fetch_tier3(url, proxy, timeout)

        # Tier 3: Browser
        elif tier == 3:
            result = await self._fetch_tier3(url, proxy, timeout)

        # Report results
        if result.ok:
            self.rate_limiter.report_success(domain)
            if proxy:
                self.proxy_pool.report_success(proxy, domain)
        else:
            self.rate_limiter.report_failure(domain)
            if proxy:
                self.proxy_pool.report_blocked(proxy, domain)

        return result

    async def _fetch_tier2(self, url: str, proxy: Optional[str], timeout: int) -> FetchResult:
        """Tier 2: stealth HTTP request via stealth_requests (curl_cffi)."""
        result = FetchResult(url=url, tier_used=2, proxy_used=proxy or "")

        try:
            from stealth_requests.session import AsyncStealthSession

            async with AsyncStealthSession(timeout=timeout) as session:
                kwargs = {}
                if proxy:
                    kwargs["proxy"] = proxy
                resp = await session.get(url, retry=self.max_retries, **kwargs)

                result.status_code = resp.status_code
                result.text = resp.text

                # Use built-in extractors if available
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

    async def _fetch_tier3(self, url: str, proxy: Optional[str], timeout: int) -> FetchResult:
        """Tier 3: browser-based fetch via patchright."""
        result = FetchResult(url=url, tier_used=3, proxy_used=proxy or "")

        try:
            from patchright.async_api import async_playwright

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

        except ImportError:
            result.error = "patchright not installed"
        except Exception as e:
            result.error = str(e)

        return result

    async def fetch_many(self, urls: list[str], concurrency: int = 3, **kwargs) -> list[FetchResult]:
        """Fetch multiple URLs with controlled concurrency."""
        sem = asyncio.Semaphore(concurrency)

        async def _limited_fetch(url):
            async with sem:
                return await self.fetch(url, **kwargs)

        return await asyncio.gather(*[_limited_fetch(u) for u in urls])
