"""Final security round: screenshot cap, redirect credential scoping, ddgs bound."""

from __future__ import annotations

import asyncio
import threading
import time
from types import SimpleNamespace

from conftest import AUTH

from browser_service import engines
from browser_service.errors import ServiceError
from browser_service.scrapers import search as search_mod


async def test_full_page_screenshot_height_is_capped(make_client, pool):
    pool.scroll_height = 5_000_000
    c = await make_client()
    r = await c.post("/v1/screenshot", headers=AUTH, json={"url": "https://example.com/", "full_page": True})
    assert r.status_code == 200
    shot = [k for k in pool.calls if k[0] == "screenshot"][0][1]
    assert shot["full_page"] is True
    assert shot["clip"]["height"] <= engines.MAX_SCREENSHOT_HEIGHT
    assert shot["clip"]["height"] == engines.MAX_SCREENSHOT_HEIGHT


async def test_short_full_page_screenshot_is_unclipped(make_client, pool):
    pool.scroll_height = 2000
    c = await make_client()
    await c.post("/v1/screenshot", headers=AUTH, json={"url": "https://example.com/", "full_page": True})
    shot = [k for k in pool.calls if k[0] == "screenshot"][0][1]
    assert "clip" not in shot


class _Resp:
    def __init__(self, status, location=None, url=""):
        self.status_code = status
        self.headers = {"location": location} if location else {}
        self.url = url
        self.charset = "utf-8"
        self.encoding = "utf-8"

    async def aiter_content(self):
        yield b"<html><body>ok</body></html>"


def test_cookie_and_auth_not_forwarded_across_hosts():
    cookies = [SimpleNamespace(name="li_at", value="S", domain=".a.test")]
    first = engines._hop_headers({"Authorization": "Bearer x", "Cookie": "z=1"}, cookies, "a.test", "a.test")
    assert first["Cookie"] == "li_at=S" and first["Authorization"] == "Bearer x"
    cross = engines._hop_headers(first, cookies, "a.test", "b.test")
    assert "Cookie" not in cross and "Authorization" not in cross
    same_sub = engines._hop_headers(first, cookies, "a.test", "www.a.test")
    assert same_sub["Cookie"] == "li_at=S"


async def test_impersonate_redirect_is_followed_manually_and_scoped(monkeypatch, make_client, pool):
    seen = []

    class FakeSession:
        def __init__(self, **kw):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *a):
            return False

        def stream(self, method, url, **kw):
            seen.append((url, dict(kw["headers"]), kw["allow_redirects"]))
            resp = _Resp(302, "https://b.test/next", url) if len(seen) == 1 else _Resp(200, None, url)

            class CM:
                async def __aenter__(_):
                    return resp

                async def __aexit__(_, *a):
                    return False

            return CM()

    import curl_cffi.requests as cr

    monkeypatch.setattr(cr, "AsyncSession", FakeSession)
    c = await make_client()
    r = await c.post(
        "/v1/render",
        headers=AUTH,
        json={
            "url": "https://a.test/start",
            "engine": "impersonate",
            "cookies": [{"name": "li_at", "value": "S", "domain": ".a.test"}],
        },
    )
    assert r.status_code == 200, r.text
    assert len(seen) == 2
    assert all(s[2] is False for s in seen)
    assert seen[0][1].get("Cookie") == "li_at=S"
    assert "Cookie" not in seen[1][1] and "Authorization" not in seen[1][1]


async def test_ddgs_work_is_bounded_even_after_timeouts(monkeypatch):
    release = threading.Event()
    started = []

    class FakeDDGS:
        def __init__(self, **kw):
            pass

        def text(self, q, **kw):
            started.append(q)
            release.wait(5)
            return []

    import ddgs

    monkeypatch.setattr(ddgs, "DDGS", FakeDDGS)
    cap = search_mod.MAX_DDGS_WORKERS
    settings = SimpleNamespace(max_download_bytes=1, search_timeout_s=1, search_backends="")
    proxy = SimpleNamespace(
        open_session=lambda *a, **k: object(), proxy_url=lambda s: "http://x", close_session=lambda s: None
    )
    deps = SimpleNamespace(settings=settings, proxy=proxy)
    deadline = SimpleNamespace(left=lambda: 0.3)

    async def one(i):
        try:
            return await search_mod.ddgs_search(deps, "text", f"q{i}", 3, deadline)
        except ServiceError as e:
            return e.code
        except Exception as e:  # timeout of an abandoned worker
            return type(e).__name__

    results = await asyncio.gather(*(one(i) for i in range(cap + 3)))
    await asyncio.sleep(0.1)
    assert len(started) <= cap
    assert results.count("busy") >= 3
    release.set()
    time.sleep(0.2)
