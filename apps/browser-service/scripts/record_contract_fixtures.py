"""Record real service responses for the Go client's replay test.

uv run python scripts/record_contract_fixtures.py ../backend-go/internal/browserclient/testdata/recorded

Runs the FastAPI app in-process with fake browser / ddgs / holehe / staffspy
backends (no network) and saves request + response pairs; extract runs the
real trafilatura and markdownify.
"""

from __future__ import annotations

import asyncio
import json
import sys
from contextlib import asynccontextmanager
from pathlib import Path

import httpx

from browser_service.app import create_app
from browser_service.config import Settings
from browser_service.scrapers import holehe_check, staffspy_roster

TOKEN = "r" * 40
ARTICLE = (
    "<html><head><title>Acme Robotics raises Series B</title><meta name='author' content='Jane Doe'>"
    "<meta name='description' content='Acme news'></head><body><nav>Home About</nav><article>"
    "<h1>Acme Robotics raises Series B</h1>"
    + "".join(f"<p>Paragraph {i}: Acme Robotics builds warehouse automation for logistics firms.</p>" for i in range(6))
    + "</article></body></html>"
)


class Page:
    url = "https://acme.example/final"

    async def goto(self, url, wait_until=None, timeout=None):
        class R:
            status = 200
            headers: dict = {}

        return R()

    async def wait_for_timeout(self, ms):
        pass

    async def content(self):
        return ARTICLE


class Pool:
    ready = True

    async def start(self):
        pass

    async def stop(self):
        pass

    @asynccontextmanager
    async def page(self, session, **kw):
        yield Page()


async def fake_search(deps, method, search, max_results, deadline, **opts):
    if "fail" in search:
        raise RuntimeError("202 Ratelimit")
    return [
        {"title": "Acme Robotics", "href": "https://acme.example/", "body": "Warehouse automation"},
        {
            "title": "Acme funding",
            "url": "https://news.example/acme",
            "body": "Series B",
            "date": "2026-10-01",
            "source": "Wire",
        },
    ][:max_results]


async def main(out: Path) -> None:
    out.mkdir(parents=True, exist_ok=True)

    async def fn(email, client, o, _n="twitter"):
        o.append({"name": _n, "domain": "twitter.com", "method": "register", "rateLimit": False, "exists": True})

    async def fn2(email, client, o):
        o.append({"name": "spotify", "domain": "spotify.com", "method": "register", "rateLimit": True, "exists": False})

    holehe_check.load_modules = lambda: [fn, fn2]

    class C:
        async def aclose(self):
            pass

    holehe_check.make_client = lambda p, t: C()
    staffspy_roster.check_logged_in = lambda s: True
    staffspy_roster.scrape_staff = lambda s, c, t, loc, n: [
        {
            "name": "Jane Doe",
            "current_position": "CEO",
            "profile_link": "https://linkedin.com/in/jane",
            "location": "Pune",
        }
    ]
    app = create_app(Settings(token=TOKEN, staffspy_enabled=True), pool=Pool(), search_fn=fake_search)
    cm = app.router.lifespan_context(app)
    await cm.__aenter__()
    calls = {
        "search": ("POST", "/v1/search", {"query": "acme robotics", "method": "news", "max_results": 2}),
        "search_error": ("POST", "/v1/search", {"query": "fail", "max_results": 2}),
        "extract": ("POST", "/v1/extract", {"html": ARTICLE, "url": "https://acme.example/a"}),
        "extract_url": ("POST", "/v1/extract", {"url": "https://acme.example/a", "engine": "browser"}),
        "holehe": ("POST", "/v1/holehe", {"email": "jane@acme.example"}),
        "staffspy": (
            "POST",
            "/v1/staffspy",
            {"company_name": "Acme", "cookies": [{"name": "li_at", "value": "x", "domain": ".linkedin.com"}]},
        ),
        "stats": ("GET", "/v1/stats", None),
        "render_bad_proxy": ("POST", "/v1/render", {"url": "https://acme.example/", "upstream_proxy": "ftp://x:1"}),
    }
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://svc") as c:
        for name, (method, path, body) in calls.items():
            r = await c.request(method, path, json=body, headers={"Authorization": f"Bearer {TOKEN}"})
            data = r.json()
            if isinstance(data, dict):
                data.pop("elapsed_ms", None)
                data.pop("uptime_s", None)
            (out / f"{name}.json").write_text(
                json.dumps(
                    {"method": method, "path": path, "request": body, "status": r.status_code, "response": data},
                    indent=1,
                    sort_keys=True,
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )
    await cm.__aexit__(None, None, None)
    print("recorded", len(calls))


if __name__ == "__main__":
    asyncio.run(main(Path(sys.argv[1])))
