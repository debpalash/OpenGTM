"""Page engines behind POST /v1/render and /v1/screenshot.

* browser      — headless Chromium (Playwright), JS rendered, optional
                 lazy-load scrolling (legacy services/scraper.py robust path
                 and leadgen/http.py tier 3).
* impersonate  — curl_cffi with a real Chrome TLS/JA3 fingerprint, no JS
                 (legacy leadgen/http.py tier 2).
* flaresolverr — the FlareSolverr sidecar for Cloudflare/DDoS-Guard walls
                 (legacy scrapers/flaresolverr_client.py, tier 3.5).

All three fetch only through the egress proxy. FlareSolverr runs its own
browser in another container, so it is handed the proxy's network address
(BROWSER_EGRESS_PROXY_URL) plus this request's session credentials: every
connection it opens is resolved and pinned by the same policy, and the session
is revoked when the call returns. The target and final URL are additionally
DNS-vetted here (defence in depth). The service refuses to start with
FLARESOLVERR_URL but no reachable proxy address.
"""

from __future__ import annotations

import asyncio
import base64
import json
import logging
import time
from dataclasses import dataclass
from typing import Any
from urllib.parse import urljoin, urlsplit

from .browser import BrowserUnavailable
from .config import Settings
from .egress import EGRESS_HEADER, BlockedError, EgressPolicy, EgressProxy, EgressSession, ResolveError
from .errors import ServiceError
from .models import Cookie, RenderRequest, RenderResult, ScreenshotRequest, ScreenshotResult
from .textutil import html_to_text, is_challenge, page_meta, truncate_utf8

logger = logging.getLogger("browser_service.engines")


class Deadline:
    def __init__(self, seconds: float) -> None:
        self.start = time.monotonic()
        self.end = self.start + seconds

    def left(self) -> float:
        return max(0.0, self.end - time.monotonic())

    def ms(self) -> float:
        return max(1.0, self.left() * 1000)

    def elapsed_ms(self) -> int:
        return int((time.monotonic() - self.start) * 1000)


@dataclass
class Deps:
    settings: Settings
    policy: EgressPolicy
    proxy: EgressProxy
    pool: Any  # BrowserPool (or a fake in tests)
    search: Any = None  # ddgs search override (tests)
    stealth_pool: Any = None  # patchright BrowserPool for Google Maps (BROWSER_MAPS_STEALTH)
    stats: Any = None  # app.Counters


def _check(deps: Deps, url: str) -> tuple[str, int]:
    try:
        return deps.policy.check_url(url)
    except BlockedError as e:
        raise ServiceError("egress_blocked") from e


def _pw_cookies(cookies: list[Cookie]) -> list[dict]:
    return [
        {
            "name": c.name,
            "value": c.value,
            "domain": c.domain,
            "path": c.path,
            "secure": c.secure,
            "httpOnly": c.http_only,
        }
        for c in cookies
    ]


UPSTREAM_MARKERS = ("upstream_failed", "upstream_blocked")


def _nav_error(session: EgressSession, exc: BaseException) -> ServiceError:
    name = type(exc).__name__
    msg = str(exc)
    if session.upstream_failed and not session.blocked:
        return ServiceError("upstream_error")  # the caller's proxy failed, not the site
    if session.blocked and ("ERR_TUNNEL_CONNECTION_FAILED" in msg or "ERR_PROXY" in msg or "ERR_BLOCKED" in msg):
        return ServiceError("egress_blocked")
    if session.over_limit:
        return ServiceError("too_large")
    if name == "TimeoutError" or "Timeout" in name:
        return ServiceError("timeout")
    return ServiceError("navigation_failed")


async def _goto(page: Any, session: EgressSession, url: str, wait_until: str, deadline: Deadline) -> Any:
    try:
        resp = await page.goto(url, wait_until=wait_until, timeout=deadline.ms())
    except Exception as e:
        logger.info("navigation failed: %s", type(e).__name__)
        raise _nav_error(session, e) from e
    marker = (resp.headers or {}).get(EGRESS_HEADER) if resp is not None else None
    if marker == "blocked":
        raise ServiceError("egress_blocked")
    if marker in UPSTREAM_MARKERS:
        raise ServiceError("upstream_error")
    return resp


async def _autoscroll(page: Any, max_scrolls: int, deadline: Deadline) -> None:
    js_height = "document.body ? document.body.scrollHeight : 0"
    try:
        last = await page.evaluate(js_height)
        for _ in range(max_scrolls):
            if deadline.left() < 2.5:
                return
            await page.evaluate("window.scrollTo(0, document.body ? document.body.scrollHeight : 0)")
            await page.wait_for_timeout(2000)
            new = await page.evaluate(js_height)
            if new == last:
                return
            last = new
    except Exception:  # navigation during scrolling: keep what we have
        logger.debug("autoscroll interrupted", exc_info=True)


async def _build_result(
    req: RenderRequest,
    engine: str,
    final_url: str,
    status: int,
    html: str,
    blocked: int,
    deadline: Deadline,
    max_bytes: int,
) -> RenderResult:
    # Parsing is CPU work: do it off the event loop, on a bounded input.
    src, _ = truncate_utf8(html, max(max_bytes * 2, 1 << 20))
    title, desc = await asyncio.to_thread(page_meta, src)
    text = text_trunc = None
    if req.include_text:
        text, text_trunc = truncate_utf8(await asyncio.to_thread(html_to_text, src), max_bytes)
    out_html = html_trunc = None
    if req.include_html:
        out_html, html_trunc = truncate_utf8(html, max_bytes)
    return RenderResult(
        url=req.url,
        final_url=final_url,
        status=status,
        engine=engine,
        title=title,
        description=desc,
        html=out_html,
        text=text,
        html_truncated=bool(html_trunc),
        text_truncated=bool(text_trunc),
        challenge=is_challenge(status, html),
        blocked_requests=blocked,
        elapsed_ms=deadline.elapsed_ms(),
    )


def _cap(deps: Deps, requested: int) -> int:
    # html and text are capped separately; together they stay under the response cap
    return max(1024, min(requested, deps.settings.max_response_bytes // 2 - 8192))


async def render(deps: Deps, req: RenderRequest, deadline: Deadline) -> RenderResult:
    if req.engine == "impersonate":
        return await render_impersonate(deps, req, deadline)
    if req.engine == "flaresolverr":
        return await render_flaresolverr(deps, req, deadline)
    return await render_browser(deps, req, deadline)


async def render_browser(deps: Deps, req: RenderRequest, deadline: Deadline) -> RenderResult:
    _check(deps, req.url)
    max_bytes = _cap(deps, req.max_bytes)
    session = deps.proxy.open_session(deps.settings.max_download_bytes, upstream=req.upstream())
    try:
        async with deps.pool.page(
            session, user_agent=req.user_agent, locale=req.locale, cookies=_pw_cookies(req.cookies)
        ) as page:
            resp = await _goto(page, session, req.url, req.wait_until, deadline)
            if req.wait_ms:
                await page.wait_for_timeout(min(req.wait_ms, deadline.left() * 1000))
            if req.scroll and req.max_scrolls:
                await _autoscroll(page, req.max_scrolls, deadline)
            html = await page.content()
            final_url = page.url
        status = resp.status if resp is not None else 0
        return await _build_result(req, "browser", final_url, status, html, len(session.blocked), deadline, max_bytes)
    except BrowserUnavailable as e:
        raise ServiceError("browser_unavailable") from e
    finally:
        deps.proxy.close_session(session)


async def screenshot(deps: Deps, req: ScreenshotRequest, deadline: Deadline) -> ScreenshotResult:
    _check(deps, req.url)
    session = deps.proxy.open_session(deps.settings.max_download_bytes)
    try:
        async with deps.pool.page(session, viewport=(req.viewport_width, req.viewport_height)) as page:
            resp = await _goto(page, session, req.url, req.wait_until, deadline)
            if req.wait_ms:
                await page.wait_for_timeout(min(req.wait_ms, deadline.left() * 1000))
            kw: dict[str, Any] = {"type": req.format, "full_page": req.full_page, "timeout": deadline.ms()}
            if req.full_page:
                try:
                    height = int(await page.evaluate(_SCROLL_HEIGHT_JS))
                except Exception:
                    height = MAX_SCREENSHOT_HEIGHT
                if height > MAX_SCREENSHOT_HEIGHT:
                    kw["clip"] = {"x": 0, "y": 0, "width": req.viewport_width, "height": MAX_SCREENSHOT_HEIGHT}
            if req.format == "jpeg":
                kw["quality"] = req.quality
            try:
                img: bytes = await page.screenshot(**kw)
            except Exception as e:
                raise _nav_error(session, e) from e
            final_url = page.url
    except BrowserUnavailable as e:
        raise ServiceError("browser_unavailable") from e
    finally:
        deps.proxy.close_session(session)
    if len(img) * 4 // 3 + 4096 > deps.settings.max_response_bytes:
        raise ServiceError("too_large")
    return ScreenshotResult(
        url=req.url,
        final_url=final_url,
        status=resp.status if resp is not None else 0,
        content_type=f"image/{req.format}",
        image_base64=base64.b64encode(img).decode("ascii"),
        bytes=len(img),
        elapsed_ms=deadline.elapsed_ms(),
    )


# Full-page captures are clipped to this many CSS pixels of height; the encoded
# image is also bounded by max_response_bytes.
MAX_SCREENSHOT_HEIGHT = 16384
MAX_REDIRECTS = 6
_SCROLL_HEIGHT_JS = "Math.max(document.documentElement.scrollHeight, document.body ? document.body.scrollHeight : 0)"
_CROSS_HOST_STRIP = ("cookie", "authorization", "proxy-authorization")


def _hop_headers(prev: dict[str, str], cookies: list[Cookie], prev_host: str, host: str) -> dict[str, str]:
    """Headers for one redirect hop. Credentials never survive a host change;
    cookies are recomputed from the per-domain jar for the new host."""
    out = dict(prev)
    if host != prev_host:
        out = {k: v for k, v in out.items() if k.lower() not in _CROSS_HOST_STRIP}
    if cookies:
        out.pop("Cookie", None)
        cookie = _cookie_header(cookies, host)
        if cookie:
            out["Cookie"] = cookie
    return out


def _cookie_header(cookies: list[Cookie], host: str) -> str:
    out = []
    for c in cookies:
        d = c.domain.lstrip(".").lower()
        if host == d or host.endswith("." + d):
            out.append(f"{c.name}={c.value}")
    return "; ".join(out)


async def render_impersonate(deps: Deps, req: RenderRequest, deadline: Deadline) -> RenderResult:
    from curl_cffi.requests import AsyncSession

    host, _ = _check(deps, req.url)
    max_bytes = _cap(deps, req.max_bytes)
    raw_cap = max_bytes * 2
    session = deps.proxy.open_session(deps.settings.max_download_bytes, upstream=req.upstream())
    headers = {"Accept-Language": f"{req.locale},en;q=0.8"}
    if req.user_agent:
        headers["User-Agent"] = req.user_agent
    cookie = _cookie_header(req.cookies, host)
    if cookie:
        headers["Cookie"] = cookie
    try:
        async with AsyncSession(impersonate="chrome", trust_env=False) as s:
            cur_url, cur_host = req.url, host
            for hop in range(MAX_REDIRECTS + 1):
                async with s.stream(
                    "GET",
                    cur_url,
                    headers=headers,
                    proxy=deps.proxy.proxy_url(session),
                    allow_redirects=False,
                    timeout=max(1.0, deadline.left()),
                ) as r:
                    marker = r.headers.get(EGRESS_HEADER) or ""
                    if marker == "blocked":
                        raise ServiceError("egress_blocked")
                    if marker in UPSTREAM_MARKERS:
                        raise ServiceError("upstream_error")
                    location = r.headers.get("location")
                    if r.status_code in (301, 302, 303, 307, 308) and location:
                        if hop == MAX_REDIRECTS:
                            raise ServiceError("navigation_failed")
                        nxt = urljoin(cur_url, location)
                        nxt_host, _ = _check(deps, nxt)
                        headers = _hop_headers(headers, req.cookies, cur_host, nxt_host)
                        cur_url, cur_host = nxt, nxt_host
                        continue
                    buf = bytearray()
                    async for chunk in r.aiter_content():
                        buf += chunk
                        if len(buf) > raw_cap:
                            break
                    status, final_url = r.status_code, str(r.url) or cur_url
                    enc = r.charset or r.encoding or "utf-8"
                    break
        try:
            html = bytes(buf).decode(enc, errors="replace")
        except LookupError:
            html = bytes(buf).decode("utf-8", errors="replace")
    except ServiceError:
        raise
    except Exception as e:
        logger.info("impersonate fetch failed: %s", type(e).__name__)
        if session.upstream_failed and not session.blocked:
            raise ServiceError("upstream_error") from e
        if session.blocked:
            raise ServiceError("egress_blocked") from e
        if "Timeout" in type(e).__name__ or deadline.left() <= 0:
            raise ServiceError("timeout") from e
        raise ServiceError("navigation_failed") from e
    finally:
        deps.proxy.close_session(session)
    return await _build_result(req, "impersonate", final_url, status, html, len(session.blocked), deadline, max_bytes)


async def _vet_resolved(deps: Deps, url: str) -> None:
    host, port = _check(deps, url)
    try:
        await deps.policy.resolve(host, port)
    except BlockedError as e:
        raise ServiceError("egress_blocked") from e
    except ResolveError as e:
        raise ServiceError("navigation_failed") from e


async def render_flaresolverr(deps: Deps, req: RenderRequest, deadline: Deadline) -> RenderResult:
    import httpx

    base = deps.settings.flaresolverr_url
    if not base:
        raise ServiceError("engine_unavailable")
    await _vet_resolved(deps, req.url)
    max_bytes = _cap(deps, req.max_bytes)
    budget = deadline.left()
    session = deps.proxy.open_session(deps.settings.max_download_bytes, upstream=req.upstream())
    proxy_base = deps.settings.egress_proxy_url or deps.proxy.server_url
    payload = {
        "cmd": "request.get",
        "url": req.url,
        "maxTimeout": int(max(1.0, budget - 2) * 1000),
        "proxy": {"url": proxy_base, "username": session.sid, "password": session.secret},
    }
    try:
        async with httpx.AsyncClient(timeout=budget, trust_env=False, follow_redirects=False) as client:
            async with client.stream("POST", f"{base}/v1", json=payload) as resp:
                buf = bytearray()
                async for chunk in resp.aiter_bytes():
                    buf += chunk
                    if len(buf) > deps.settings.max_response_bytes * 3:
                        raise ServiceError("too_large")
        data = json.loads(bytes(buf))
    except ServiceError:
        raise
    except httpx.TimeoutException as e:
        raise ServiceError("timeout") from e
    except Exception as e:
        logger.info("flaresolverr call failed: %s", type(e).__name__)
        raise ServiceError("upstream_error") from e
    finally:
        deps.proxy.close_session(session)
    if not isinstance(data, dict) or data.get("status") != "ok":
        raise ServiceError("egress_blocked" if session.blocked else "upstream_error")
    # Fail closed (review M2): the proxy is only advisory to FlareSolverr. A build
    # that ignores it fetches straight from its own network (no traffic on our
    # session); a page that made it try a refused destination is not returned.
    if session.blocked:
        raise ServiceError("egress_blocked")
    if session.bytes_in == 0:
        logger.warning("flaresolverr answered without using the egress proxy; refusing the result")
        raise ServiceError("upstream_error")
    sol = data.get("solution") or {}
    final_url = str(sol.get("url") or req.url)
    if urlsplit(final_url).scheme not in ("http", "https"):
        raise ServiceError("upstream_error")
    await _vet_resolved(deps, final_url)
    html = str(sol.get("response") or "")
    try:
        status = int(sol.get("status") or 0)
    except (TypeError, ValueError):
        status = 0
    return await _build_result(req, "flaresolverr", final_url, status, html, len(session.blocked), deadline, max_bytes)
