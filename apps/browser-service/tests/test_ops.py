"""Ops added for the Go web layer (plan 15 WP-WEB): generic search, extract,
holehe, staffspy, stats, per-request upstream proxies and the stealth Maps
browser."""

import asyncio
import sys
import types

import pytest
from conftest import AUTH, TOKEN, FakePool

from browser_service.config import ConfigError, Settings
from browser_service.egress import UpstreamProxy
from browser_service.scrapers import holehe_check, search, staffspy_roster

ARTICLE = (
    "<html><head><title>Acme Robotics raises Series B</title>"
    "<meta name='description' content='Acme news'><meta name='author' content='Jane Doe'></head><body>"
    "<nav><a href='/'>Home</a> <a href='/about'>About</a></nav>"
    "<article><h1>Acme Robotics raises Series B</h1>"
    + "".join(
        f"<p>Paragraph {i}: Acme Robotics builds warehouse automation for logistics companies across India "
        f"and Europe, and today announced a new round of funding led by Example Ventures.</p>"
        for i in range(8)
    )
    + "</article><footer>Contact: hello@acme.example</footer></body></html>"
)


class SearchRec:
    def __init__(self, results=None, error=None):
        self.calls = []
        self.results = results if results is not None else [{"title": "T", "href": "https://a.example/", "body": "B"}]
        self.error = error

    async def __call__(self, deps, method, search, max_results, deadline, **opts):
        self.calls.append((method, search, max_results, opts))
        if self.error is not None:
            raise self.error
        return self.results


# ── /v1/search ───────────────────────────────────────────────────────


async def test_search_passes_options_and_normalises_hits(make_client):
    rec = SearchRec(
        [
            {"title": "A", "href": "https://a.example/", "body": "x"},
            {"title": "N", "url": "https://n.example/", "body": "y", "date": "2026-01-01", "source": "Wire"},
            "garbage",
        ]
    )
    c = await make_client(search_fn=rec)
    r = await c.post(
        "/v1/search",
        headers=AUTH,
        json={
            "query": "acme robotics",
            "method": "news",
            "max_results": 5,
            "backends": ["bing", "brave"],
            "region": "in-en",
            "safesearch": "off",
            "timelimit": "w",
            "upstream_proxy": "socks5://u:p@9.9.9.9:1080",
        },
    )
    assert r.status_code == 200, r.text
    b = r.json()
    assert b["error"] is None and b["query"] == "acme robotics" and b["method"] == "news"
    assert b["hits"][0]["url"] == "https://a.example/" and b["hits"][1]["source"] == "Wire"
    assert len(b["hits"]) == 2
    method, q, n, opts = rec.calls[0]
    assert (method, q, n) == ("news", "acme robotics", 5)
    assert opts["backend"] == "bing, brave" and opts["region"] == "in-en" and opts["safesearch"] == "off"
    assert opts["timelimit"] == "w"
    assert isinstance(opts["upstream"], UpstreamProxy) and opts["upstream"].host == "9.9.9.9"
    assert "u:p" not in r.text


async def test_search_defaults_use_service_backends(make_client):
    rec = SearchRec()
    c = await make_client(search_fn=rec, search_backends="duckduckgo, google")
    r = await c.post("/v1/search", headers=AUTH, json={"query": "q"})
    assert r.status_code == 200
    _m, _q, n, opts = rec.calls[0]
    assert n == 10 and opts["backend"] == "duckduckgo, google" and opts["upstream"] is None
    assert opts["region"] == "us-en" and opts["safesearch"] == "moderate" and opts["timelimit"] is None
    rec2 = SearchRec()
    c2 = await make_client(search_fn=rec2)
    await c2.post("/v1/search", headers=AUTH, json={"query": "q", "backends": []})
    assert rec2.calls[0][3]["backend"] is None  # ddgs default


@pytest.mark.parametrize(
    "exc,code",
    [
        (RuntimeError("RatelimitException: 202 Ratelimit"), "rate_limited"),
        (RuntimeError("https://x 403 Forbidden"), "rate_limited"),
        (RuntimeError("captcha required"), "rate_limited"),
        (TimeoutError(), "timeout"),
        (RuntimeError("Read timed out"), "timeout"),
        (RuntimeError("No results found."), "no_results"),
        (RuntimeError("boom secret-text"), "failed"),
    ],
)
async def test_search_classifies_failures_without_upstream_text(make_client, exc, code):
    c = await make_client(search_fn=SearchRec(error=exc))
    r = await c.post("/v1/search", headers=AUTH, json={"query": "q"})
    assert r.status_code == 200 and r.json()["error"] == code and r.json()["hits"] == []
    assert "secret-text" not in r.text


async def test_search_empty_is_no_results(make_client):
    c = await make_client(search_fn=SearchRec([]))
    r = await c.post("/v1/search", headers=AUTH, json={"query": "q"})
    assert r.json()["error"] == "no_results"


@pytest.mark.parametrize(
    "body",
    [
        {"query": ""},
        {"query": "q", "method": "images"},
        {"query": "q", "max_results": 0},
        {"query": "q", "backends": ["bad name!"]},
        {"query": "q", "region": "../x"},
        {"query": "q", "upstream_proxy": "https://1.2.3.4:443"},
        {"query": "q", "upstream_proxy": "not a url"},
    ],
)
async def test_search_validation(make_client, body):
    c = await make_client(search_fn=SearchRec())
    r = await c.post("/v1/search", headers=AUTH, json=body)
    assert r.status_code == 400, r.text


def test_block_classification_matches_legacy_patterns():
    # legacy search_cache._RL_PATTERNS
    for text in ("ratelimit", "Rate limit", "HTTP 429", "403", "202", "Too Many", "blocked", "captcha"):
        assert search.classify_search_error(RuntimeError(text)) == "rate_limited", text
    assert search.classify_search_error(RuntimeError("connection reset")) == "failed"


async def test_scrape_sources_forward_the_upstream_proxy(make_client):
    rec = SearchRec()
    c = await make_client(search_fn=rec)
    r = await c.post(
        "/v1/scrape/crunchbase",
        headers=AUTH,
        json={"queries": ["q"], "cities": ["c"], "delay_s": 0, "upstream_proxy": "http://1.2.3.4:8080"},
    )
    assert r.status_code == 200
    assert rec.calls[0][3]["upstream"].port == 8080


# ── upstream proxy on page ops ───────────────────────────────────────


async def test_render_session_carries_the_upstream_proxy(make_client, pool):
    c = await make_client()
    r = await c.post(
        "/v1/render", headers=AUTH, json={"url": "https://example.com/", "upstream_proxy": "http://u:pw@1.2.3.4:3128"}
    )
    assert r.status_code == 200
    up = pool.sessions[0].upstream
    assert (up.scheme, up.host, up.port, up.username, up.password) == ("http", "1.2.3.4", 3128, "u", "pw")


async def test_render_rejects_a_bad_upstream_proxy(make_client, pool):
    c = await make_client()
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://example.com/", "upstream_proxy": "ftp://x:1"})
    assert r.status_code == 400 and r.json()["error"]["fields"] == ["upstream_proxy"]
    assert not pool.calls


# ── /v1/extract ──────────────────────────────────────────────────────


async def test_extract_from_html(make_client):
    c = await make_client()
    r = await c.post("/v1/extract", headers=AUTH, json={"html": ARTICLE, "url": "https://news.example/acme"})
    assert r.status_code == 200, r.text
    b = r.json()
    assert b["found"] is True
    assert "warehouse automation" in b["text"] and "Home" not in b["text"]
    assert b["word_count"] == len(b["text"].split()) and b["word_count"] > 100
    assert b["title"] == "Acme Robotics raises Series B" and b["author"] == "Jane Doe"
    assert "# Acme Robotics raises Series B" in b["markdown"]
    assert b["status"] == 0 and b["final_url"] == "https://news.example/acme"


async def test_extract_nothing_found(make_client):
    c = await make_client()
    r = await c.post("/v1/extract", headers=AUTH, json={"html": "<html><body></body></html>"})
    b = r.json()
    assert r.status_code == 200 and b["found"] is False and b["text"] == "" and b["word_count"] == 0


async def test_extract_from_url_fetches_through_the_browser_engine(make_client, pool):
    pool.html = ARTICLE
    c = await make_client()
    r = await c.post(
        "/v1/extract",
        headers=AUTH,
        json={"url": "https://news.example/acme", "engine": "browser", "upstream_proxy": "socks5://1.2.3.4:1080"},
    )
    assert r.status_code == 200, r.text
    assert r.json()["found"] and r.json()["status"] == 200
    assert pool.sessions[0].upstream.scheme == "socks5"


@pytest.mark.parametrize(
    "body",
    [
        {},
        {"url": "http://10.0.0.1/"},
        {"html": "<p>x</p>", "engine": "nope"},
        {"url": "https://a.example/x", "html": ""},
    ],
)
async def test_extract_validation(make_client, body):
    c = await make_client()
    r = await c.post("/v1/extract", headers=AUTH, json=body)
    assert r.status_code in (400, 403), r.text


async def test_extract_accepts_bodies_above_the_generic_cap(make_client):
    c = await make_client(max_request_bytes=64 * 1024)
    big = "<html><body><article>" + "<p>word " * 30000 + "</article></body></html>"
    r = await c.post("/v1/extract", headers=AUTH, json={"html": big})
    assert r.status_code == 200
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://a.example/", "user_agent": "x" * 70000})
    assert r.status_code == 413


# ── /v1/holehe ───────────────────────────────────────────────────────


def _fake_holehe(monkeypatch, behaviours):
    """Install fake holehe modules: name -> exists | 'rate' | Exception."""
    funcs = []
    for name, b in behaviours.items():

        async def fn(email, client, out, _name=name, _b=b):
            if isinstance(_b, Exception):
                raise _b
            out.append(
                {
                    "name": _name,
                    "domain": f"{_name}.com",
                    "method": "register",
                    "frequent_rate_limit": False,
                    "rateLimit": _b == "rate",
                    "exists": _b is True,
                    "emailrecovery": "secret-recovery",
                    "phoneNumber": None,
                    "others": None,
                }
            )

        fn.__name__ = name
        funcs.append(fn)
    monkeypatch.setattr(holehe_check, "load_modules", lambda: funcs)
    return funcs


async def test_holehe_reports_per_site_results(make_client, monkeypatch):
    _fake_holehe(monkeypatch, {"twitter": True, "spotify": False, "instagram": "rate", "adobe": RuntimeError("x")})
    seen = {}

    def fake_client(proxy_url, timeout):
        seen["proxy"] = proxy_url
        seen["timeout"] = timeout

        class C:
            async def aclose(self):
                seen["closed"] = True

        return C()

    monkeypatch.setattr(holehe_check, "make_client", fake_client)
    c = await make_client()
    r = await c.post("/v1/holehe", headers=AUTH, json={"email": "jane@acme.example"})
    assert r.status_code == 200, r.text
    b = r.json()
    assert b["exists_on"] == ["twitter"] and b["total_checked"] == 3 and b["errors"] == 1
    assert [s["name"] for s in b["sites"]] == ["twitter", "spotify", "instagram"]
    assert b["sites"][2]["rate_limited"] is True and b["sites"][0]["domain"] == "twitter.com"
    assert "secret-recovery" not in r.text
    assert seen["proxy"].startswith("http://s") and seen["closed"] and seen["timeout"] == 10


async def test_holehe_caps_modules(make_client, monkeypatch):
    _fake_holehe(monkeypatch, {f"m{i}": False for i in range(60)})
    monkeypatch.setattr(holehe_check, "make_client", lambda p, t: types.SimpleNamespace(aclose=_anoop))
    c = await make_client()
    r = await c.post("/v1/holehe", headers=AUTH, json={"email": "a@b.example"})
    assert r.json()["total_checked"] == 50
    r = await c.post("/v1/holehe", headers=AUTH, json={"email": "a@b.example", "max_modules": 5})
    assert r.json()["total_checked"] == 5


async def _anoop():
    return None


@pytest.mark.parametrize("email", ["", "no-at-sign", "a@b", "x" * 300 + "@a.com"])
async def test_holehe_validation(make_client, email):
    c = await make_client()
    r = await c.post("/v1/holehe", headers=AUTH, json={"email": email})
    assert r.status_code == 400


def test_holehe_real_modules_load():
    mods = holehe_check.load_modules()
    assert len(mods) > 100 and all(asyncio.iscoroutinefunction(m) for m in mods[:5])


# ── /v1/staffspy ─────────────────────────────────────────────────────

LI = [
    {"name": "li_at", "value": "AQ-secret", "domain": ".linkedin.com"},
    {"name": "JSESSIONID", "value": '"ajax:1"', "domain": ".linkedin.com"},
]


async def test_staffspy_is_off_by_default(make_client):
    c = await make_client()
    r = await c.post("/v1/staffspy", headers=AUTH, json={"company_name": "Acme", "cookies": LI})
    assert r.status_code == 503 and r.json()["error"]["code"] == "engine_unavailable"


async def test_staffspy_maps_rows_and_uses_the_egress_proxy(make_client, monkeypatch):
    seen = {}

    def fake_scrape(session, company, search_term, location, max_results):
        seen["proxies"] = dict(session.proxies)
        seen["trust_env"] = session.trust_env
        seen["cookies"] = {c.name: c.value for c in session.cookies}
        seen["args"] = (company, search_term, location, max_results)
        return [
            {
                "name": "Jane Doe",
                "current_position": "CEO",
                "profile_link": "https://linkedin.com/in/jane",
                "location": "Pune",
            },
            {"name": "", "headline": "x"},
            {"name": "Raj", "headline": "Engineer", "profile_id": "raj-1", "location": None},
        ]

    monkeypatch.setattr(staffspy_roster, "scrape_staff", fake_scrape)
    monkeypatch.setattr(staffspy_roster, "check_logged_in", lambda s: True)
    c = await make_client(staffspy_enabled=True)
    r = await c.post(
        "/v1/staffspy",
        headers=AUTH,
        json={"company_name": "Acme", "search_term": "founder OR ceo", "location": "Pune", "cookies": LI},
    )
    assert r.status_code == 200, r.text
    b = r.json()
    assert b["error"] is None
    assert b["staff"] == [
        {"name": "Jane Doe", "title": "CEO", "linkedin": "https://linkedin.com/in/jane", "location": "Pune"},
        {"name": "Raj", "title": "Engineer", "linkedin": "raj-1", "location": ""},
    ]
    assert seen["proxies"]["https"].startswith("http://s") and seen["trust_env"] is False
    assert seen["cookies"]["li_at"] == "AQ-secret" and seen["args"] == ("Acme", "founder OR ceo", "Pune", 25)
    assert "AQ-secret" not in r.text


async def test_staffspy_login_failure_and_empty(make_client, monkeypatch):
    monkeypatch.setattr(staffspy_roster, "check_logged_in", lambda s: False)
    c = await make_client(staffspy_enabled=True)
    r = await c.post("/v1/staffspy", headers=AUTH, json={"company_name": "Acme", "cookies": LI})
    assert r.json()["error"] == "login_failed" and r.json()["staff"] == []
    monkeypatch.setattr(staffspy_roster, "check_logged_in", lambda s: True)
    monkeypatch.setattr(staffspy_roster, "scrape_staff", lambda *a: [])
    r = await c.post("/v1/staffspy", headers=AUTH, json={"company_name": "Acme", "cookies": LI})
    assert r.json()["error"] == "no_staff_found"


async def test_staffspy_needs_li_at(make_client):
    c = await make_client(staffspy_enabled=True)
    r = await c.post("/v1/staffspy", headers=AUTH, json={"company_name": "Acme", "cookies": LI[1:]})
    assert r.status_code == 400


def test_staffspy_flag_from_env():
    assert not Settings.from_env({"BROWSER_SERVICE_TOKEN": TOKEN}).staffspy_enabled
    assert Settings.from_env({"BROWSER_SERVICE_TOKEN": TOKEN, "BROWSER_STAFFSPY_ENABLED": "1"}).staffspy_enabled


# ── stealth Maps browser ─────────────────────────────────────────────


def test_stealth_flag_from_env():
    s = Settings.from_env({"BROWSER_SERVICE_TOKEN": TOKEN})
    assert s.maps_stealth is False
    assert Settings.from_env({"BROWSER_SERVICE_TOKEN": TOKEN, "BROWSER_MAPS_STEALTH": "true"}).maps_stealth
    with pytest.raises(ConfigError):
        Settings.from_env({"BROWSER_SERVICE_TOKEN": TOKEN, "BROWSER_MAPS_STEALTH": "maybe"})


async def test_maps_uses_the_stealth_pool_when_enabled(make_client, pool):
    stealth = FakePool()
    stealth.html = "<html></html>"
    c = await make_client(maps_stealth=True, deps_kw={"stealth_pool": stealth})
    r = await c.post(
        "/v1/scrape/google_maps",
        headers=AUTH,
        json={"query": "cafes", "city": "Pune", "upstream_proxy": "http://1.2.3.4:8080"},
    )
    assert r.status_code == 200, r.text
    assert stealth.calls and not pool.calls
    assert stealth.sessions[0].upstream.port == 8080
    h = (await c.get("/v1/health", headers=AUTH)).json()
    assert "stealth" in h["engines"]


async def test_maps_uses_the_plain_pool_by_default(make_client, pool):
    c = await make_client()
    r = await c.post("/v1/scrape/google_maps", headers=AUTH, json={"query": "cafes", "city": "Pune"})
    assert r.status_code == 200 and pool.calls


async def test_stealth_pool_launches_patchright_through_the_egress_proxy(monkeypatch):
    from browser_service.browser import CHROMIUM_ARGS, BrowserPool

    launched = {}

    class FakeBrowser:
        def is_connected(self):
            return True

        async def close(self):
            pass

    class FakeChromium:
        async def launch(self, **kw):
            launched.update(kw)
            return FakeBrowser()

    class FakePW:
        chromium = FakeChromium()

        async def stop(self):
            pass

    class Starter:
        async def start(self):
            return FakePW()

    mod = types.ModuleType("patchright.async_api")
    mod.async_playwright = lambda: Starter()
    monkeypatch.setitem(sys.modules, "patchright", types.ModuleType("patchright"))
    monkeypatch.setitem(sys.modules, "patchright.async_api", mod)
    proxy = types.SimpleNamespace(server_url="http://127.0.0.1:9")
    p = BrowserPool(proxy, driver="patchright")
    await p.start()
    assert p.ready and launched["proxy"] == {"server": "http://127.0.0.1:9"}
    assert "--proxy-bypass-list=<-loopback>" in launched["args"]
    assert "--disable-quic" in launched["args"]
    assert set(launched["args"]) <= set(CHROMIUM_ARGS)
    await p.stop()


# ── /v1/stats ────────────────────────────────────────────────────────


async def test_stats_counts_calls_and_error_codes(make_client, pool):
    c = await make_client(search_fn=SearchRec(error=RuntimeError("429")))
    await c.post("/v1/render", headers=AUTH, json={"url": "https://example.com/"})
    await c.post("/v1/render", headers=AUTH, json={"url": "http://127.0.0.1/"})
    await c.post("/v1/search", headers=AUTH, json={"query": "q"})
    r = await c.get("/v1/stats", headers=AUTH)
    assert r.status_code == 200, r.text
    b = r.json()
    assert b["ops"]["render"] == {"calls": 2, "ok": 1, "errors": {"egress_blocked": 1}}
    assert b["ops"]["search"]["calls"] == 1 and b["searches"] == {"rate_limited": 1}
    assert b["capacity"] == 4 and b["active"] == 0 and b["uptime_s"] >= 0
    assert set(b["egress"]) == {"sessions", "blocked", "upstream_failed"}
    assert (await c.get("/v1/stats")).status_code == 401


def test_settings_defaults_unchanged():
    s = Settings(token="t" * 40)
    assert s.max_extract_bytes == 8 << 20 and s.staffspy_enabled is False and s.maps_stealth is False
