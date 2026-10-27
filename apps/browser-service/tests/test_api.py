import asyncio
import base64
import json
from pathlib import Path

import pytest
from conftest import AUTH, TOKEN

from browser_service import app as app_module
from browser_service.config import ConfigError, Settings
from browser_service.openapi_export import render as render_openapi

# ── config ───────────────────────────────────────────────────────────


def test_refuses_to_start_without_a_token():
    with pytest.raises(ConfigError):
        Settings.from_env({})
    with pytest.raises(ConfigError):
        Settings.from_env({"BROWSER_SERVICE_TOKEN": "short"})
    s = Settings.from_env({"BROWSER_SERVICE_TOKEN": TOKEN})
    assert s.host == "127.0.0.1" and s.max_concurrency == 4
    assert TOKEN not in repr(s)


def test_allow_cidrs_only_under_app_env_test():
    env = {"BROWSER_SERVICE_TOKEN": TOKEN, "BROWSER_EGRESS_ALLOW_CIDRS": "127.0.0.1/32"}
    with pytest.raises(ConfigError):
        Settings.from_env(env)
    s = Settings.from_env({**env, "APP_ENV": "test"})
    assert [str(c) for c in s.egress_allow_cidrs] == ["127.0.0.1/32"]


def test_numeric_settings_are_bounded():
    with pytest.raises(ConfigError):
        Settings.from_env({"BROWSER_SERVICE_TOKEN": TOKEN, "BROWSER_MAX_CONCURRENCY": "0"})
    with pytest.raises(ConfigError):
        Settings.from_env({"BROWSER_SERVICE_TOKEN": TOKEN, "BROWSER_MAX_TIMEOUT_S": "nope"})


# ── auth ─────────────────────────────────────────────────────────────


async def test_healthz_is_open_and_v1_needs_the_token(make_client):
    c = await make_client()
    assert (await c.get("/healthz")).json() == {"status": "ok"}
    for headers in (
        {},
        {"Authorization": "Bearer wrong"},
        {"Authorization": f"Basic {TOKEN}"},
        {"Authorization": f"Bearer {TOKEN}x"},
    ):
        r = await c.get("/v1/health", headers=headers)
        assert r.status_code == 401 and r.json()["error"]["code"] == "unauthorized"
        r = await c.post("/v1/render", headers=headers, json={"url": "https://example.com"})
        assert r.status_code == 401
    r = await c.get("/v1/health", headers=AUTH)
    assert r.status_code == 200
    body = r.json()
    assert body["browser"] == "ready" and body["capacity"] == 4 and body["engines"] == ["browser", "impersonate"]


async def test_unknown_routes_use_the_envelope(make_client):
    c = await make_client()
    r = await c.post("/v1/scrape/nope", headers=AUTH, json={})
    assert r.status_code == 404 and r.json()["error"]["code"] == "not_found"
    r = await c.get("/v1/render", headers=AUTH)
    assert r.status_code == 405 and r.json()["error"]["code"] == "method_not_allowed"


# ── render ───────────────────────────────────────────────────────────


async def test_render_returns_html_text_and_meta(make_client, pool):
    c = await make_client()
    pool.final_url = "https://example.com/final"
    r = await c.post(
        "/v1/render",
        headers=AUTH,
        json={
            "url": "https://example.com/",
            "include_text": True,
            "wait_ms": 1500,
            "scroll": True,
            "max_scrolls": 2,
            "cookies": [{"name": "li_at", "value": "c", "domain": ".example.com"}],
        },
    )
    assert r.status_code == 200, r.text
    b = r.json()
    assert b["final_url"] == "https://example.com/final" and b["status"] == 200 and b["engine"] == "browser"
    assert b["title"] == "Hello" and b["description"] == "Desc"
    assert "# Head" in b["text"] and "x()" not in b["text"]
    assert "<h1>Head</h1>" in b["html"] and not b["html_truncated"]
    assert ("goto", "https://example.com/", "domcontentloaded") in pool.calls
    assert ("wait", 1500) in pool.calls
    assert pool.page_kwargs[0]["cookies"][0]["httpOnly"] is False


async def test_render_caps_html_by_bytes(make_client, pool):
    c = await make_client()
    pool.html = "<html><body>" + "é" * 5000 + "</body></html>"
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://example.com/", "max_bytes": 1024})
    b = r.json()
    assert b["html_truncated"] and len(b["html"].encode()) <= 1024


async def test_render_clamps_max_bytes_to_service_cap(make_client, pool):
    c = await make_client(max_response_bytes=64 * 1024)
    pool.html = "<html><body>" + "a" * 200_000 + "</body></html>"
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://example.com/", "max_bytes": 10 << 20})
    assert r.status_code == 200 and len(r.content) <= 64 * 1024


@pytest.mark.parametrize(
    "url",
    [
        "file:///etc/passwd",
        "http://127.0.0.1:8000/",
        "http://postgres:5432/",
        "http://169.254.169.254/latest/meta-data",
        "http://[::1]/",
        "http://0x7f000001/",
    ],
)
async def test_render_refuses_internal_targets(make_client, pool, url):
    c = await make_client()
    r = await c.post("/v1/render", headers=AUTH, json={"url": url})
    assert r.status_code == 403 and r.json()["error"]["code"] == "egress_blocked"
    assert not pool.calls


async def test_blocked_navigation_maps_to_egress_blocked_without_upstream_text(make_client, pool):
    c = await make_client()
    pool.goto_error = RuntimeError("net::ERR_TUNNEL_CONNECTION_FAILED at http://evil.test secret-upstream-text")
    pool.block_on_error = True
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://example.com/"})
    assert r.status_code == 403 and r.json()["error"]["code"] == "egress_blocked"
    assert "secret-upstream-text" not in r.text and "evil.test" not in r.text


async def test_proxy_refusal_page_is_not_returned_as_content(make_client, pool):
    c = await make_client()
    pool.status, pool.headers = 403, {"x-opengtm-egress": "blocked"}
    r = await c.post("/v1/render", headers=AUTH, json={"url": "http://example.com/"})
    assert r.status_code == 403 and r.json()["error"]["code"] == "egress_blocked"


async def test_navigation_failure_is_generic(make_client, pool):
    c = await make_client()
    pool.goto_error = RuntimeError("net::ERR_NAME_NOT_RESOLVED upstream-detail")
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://example.com/"})
    assert r.status_code == 502 and r.json()["error"]["code"] == "navigation_failed"
    assert "upstream-detail" not in r.text and "ERR_NAME" not in r.text


async def test_render_deadline(make_client, pool, monkeypatch):
    monkeypatch.setattr(app_module, "GRACE_S", 0.05)
    c = await make_client()
    pool.goto_delay = 3
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://example.com/", "timeout_s": 1})
    assert r.status_code == 504 and r.json()["error"]["code"] == "timeout"


async def test_timeout_is_clamped_to_service_max(make_client, pool, monkeypatch):
    monkeypatch.setattr(app_module, "GRACE_S", 0.05)
    c = await make_client(max_timeout_s=1.0)
    pool.goto_delay = 3
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://example.com/", "timeout_s": 500})
    assert r.status_code == 504


async def test_concurrency_cap_answers_busy(make_client, pool):
    c = await make_client(max_concurrency=1, queue_wait_s=0.1)
    pool.goto_delay = 0.6
    first = asyncio.create_task(c.post("/v1/render", headers=AUTH, json={"url": "https://example.com/"}))
    await asyncio.sleep(0.1)
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://example.com/"})
    assert r.status_code == 429 and r.json()["error"]["code"] == "busy"
    assert (await first).status_code == 200


@pytest.mark.parametrize(
    "body,field",
    [
        ({"url": "https://example.com", "bogus": 1}, "bogus"),
        ({"url": "https://example.com", "timeout_s": 601}, "timeout_s"),
        ({"url": "https://example.com", "engine": "lynx"}, "engine"),
        ({}, "url"),
    ],
)
async def test_validation_errors_are_400_with_field_names(make_client, body, field):
    c = await make_client()
    r = await c.post("/v1/render", headers=AUTH, json=body)
    assert r.status_code == 400
    assert r.json()["error"]["code"] == "bad_request" and any(field in f for f in r.json()["error"]["fields"])


async def test_request_body_cap(make_client):
    c = await make_client(max_request_bytes=1024)
    r = await c.post("/v1/render", headers=AUTH, content=json.dumps({"url": "https://example.com/" + "a" * 2000}))
    assert r.status_code == 413 and r.json()["error"]["code"] == "too_large"


async def test_flaresolverr_engine_unconfigured(make_client):
    c = await make_client()
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://example.com/", "engine": "flaresolverr"})
    assert r.status_code == 503 and r.json()["error"]["code"] == "engine_unavailable"


async def test_browser_unavailable(make_client, pool):
    from browser_service.browser import BrowserUnavailable

    c = await make_client()
    pool.goto_error = None

    from contextlib import asynccontextmanager

    @asynccontextmanager
    async def broken(session, **kw):
        raise BrowserUnavailable()
        yield

    pool.page = broken
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://example.com/"})
    assert r.status_code == 503 and r.json()["error"]["code"] == "browser_unavailable"


# ── screenshot ───────────────────────────────────────────────────────


async def test_screenshot(make_client, pool):
    c = await make_client()
    r = await c.post("/v1/screenshot", headers=AUTH, json={"url": "https://example.com/", "quality": 40})
    b = r.json()
    assert r.status_code == 200 and base64.b64decode(b["image_base64"]) == pool.image
    assert b["content_type"] == "image/jpeg" and b["bytes"] == len(pool.image)
    shot = [k for k in pool.calls if k[0] == "screenshot"][0][1]
    assert shot["quality"] == 40 and shot["type"] == "jpeg"


async def test_screenshot_over_cap(make_client, pool):
    c = await make_client(max_response_bytes=64 * 1024)
    pool.image = b"x" * 100_000
    r = await c.post("/v1/screenshot", headers=AUTH, json={"url": "https://example.com/", "format": "png"})
    assert r.status_code == 413 and r.json()["error"]["code"] == "too_large"


# ── openapi contract ─────────────────────────────────────────────────


def test_checked_in_openapi_matches_the_service():
    path = Path(__file__).resolve().parents[2] / "backend-go/internal/browserclient/testdata/openapi.json"
    assert path.exists(), "regenerate: uv run python -m browser_service.openapi_export > " + str(path)
    assert json.loads(path.read_text()) == json.loads(render_openapi()), (
        "OpenAPI drift: regenerate the Go client's testdata/openapi.json and update the client"
    )


# review L3: header-bound fields cannot smuggle CR/LF or other control characters


@pytest.mark.parametrize(
    "extra,field",
    [
        ({"locale": "en-US\r\nX-Evil: 1"}, "locale"),
        ({"locale": "e"}, "locale"),
        ({"locale": "en_US"}, "locale"),
        ({"user_agent": "Mozilla/5.0\r\nX-Evil: 1"}, "user_agent"),
        ({"user_agent": "Mozilla\x00"}, "user_agent"),
        ({"user_agent": "Mozilla  "}, "user_agent"),
        ({"cookies": [{"name": "a b", "value": "v", "domain": ".e.com"}]}, "cookies.0.name"),
        ({"cookies": [{"name": "a", "value": "v\r\nX: 1", "domain": ".e.com"}]}, "cookies.0.value"),
        ({"cookies": [{"name": "a", "value": "v; other=1", "domain": ".e.com"}]}, "cookies.0.value"),
    ],
)
async def test_header_fields_are_validated(make_client, pool, extra, field):
    c = await make_client()
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://example.com/", **extra})
    assert r.status_code == 400 and field in r.json()["error"]["fields"], r.text
    assert not pool.calls


async def test_header_fields_accept_normal_values(make_client, pool):
    c = await make_client()
    r = await c.post(
        "/v1/render",
        headers=AUTH,
        json={
            "url": "https://example.com/",
            "locale": "zh-Hant-TW",
            "user_agent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0",
            "cookies": [{"name": "li_at", "value": 'AQE"x/y=z', "domain": ".example.com"}],
        },
    )
    assert r.status_code == 200, r.text
