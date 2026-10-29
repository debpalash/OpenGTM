"""Real Chromium through the egress proxy against local servers.

Skipped when no Chromium is installed (``uv run playwright install --only-shell
chromium``). 127.0.0.1 is allow-listed for the test site; 127.0.0.2 plays the
internal network and must never receive a request.
"""

import base64
import http.server
import ipaddress
import threading

import httpx
import pytest
from conftest import AUTH, make_settings

from browser_service.app import create_app
from browser_service.egress import EgressPolicy

pytestmark = pytest.mark.browser


def _chromium_available() -> bool:
    try:
        from playwright.sync_api import sync_playwright

        with sync_playwright() as p:
            p.chromium.launch(headless=True).close()
        return True
    except Exception:
        return False


if not _chromium_available():
    pytest.skip("chromium not installed", allow_module_level=True)


class Servers:
    def __init__(self):
        self.secret_hits = []
        outer = self

        class Site(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                b = outer.secret_port
                routes = {
                    "/page": (
                        200,
                        f"""<html><head><title>T</title></head><body><p id=s>static</p>
<img src="http://evil.test:{b}/pixel.png"><script>
document.body.insertAdjacentHTML('beforeend', '<p>rendered-by-js</p>');
fetch('http://127.0.0.2:{b}/x').catch(() => {{}});
</script></body></html>""",
                    ),
                    "/redirect-evil": (302, f"http://evil.test:{b}/secret"),
                    "/redirect-literal": (302, f"http://127.0.0.2:{b}/secret"),
                    "/redirect-tls": (302, f"https://evil.test:{b}/secret"),
                    "/redirect-ok": (302, "/page"),
                }
                code, payload = routes.get(self.path, (404, "nope"))
                self.send_response(code)
                if code == 302:
                    self.send_header("Location", payload)
                    payload = ""
                data = payload.encode()
                self.send_header("Content-Type", "text/html")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        class Secret(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                outer.secret_hits.append(self.path)
                self.send_response(200)
                self.send_header("Content-Length", "6")
                self.end_headers()
                self.wfile.write(b"secret")

            do_CONNECT = do_GET

        self.site = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Site)
        self.secret = http.server.ThreadingHTTPServer(("127.0.0.2", 0), Secret)
        self.site_port = self.site.server_address[1]
        self.secret_port = self.secret.server_address[1]
        for s in (self.site, self.secret):
            threading.Thread(target=s.serve_forever, daemon=True).start()

    def close(self):
        for s in (self.site, self.secret):
            s.shutdown()
            s.server_close()


@pytest.fixture
def servers():
    s = Servers()
    yield s
    s.close()


@pytest.fixture
async def client(servers):
    table = {"site.test": ["127.0.0.1"], "evil.test": ["127.0.0.2"]}

    async def resolve(host, port):
        return table[host]

    policy = EgressPolicy(allow_cidrs=[ipaddress.ip_network("127.0.0.1/32")], resolver=resolve, ports=None)
    app = create_app(make_settings(max_timeout_s=60.0), policy=policy)
    cm = app.router.lifespan_context(app)
    await cm.__aenter__()
    c = httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://svc", timeout=60)
    yield c
    await c.aclose()
    await cm.__aexit__(None, None, None)


async def test_render_executes_js_and_blocks_internal_subrequests(client, servers):
    r = await client.post(
        "/v1/render",
        headers=AUTH,
        json={"url": f"http://site.test:{servers.site_port}/page", "include_text": True, "wait_ms": 500},
    )
    assert r.status_code == 200, r.text
    b = r.json()
    assert "rendered-by-js" in b["text"] and b["title"] == "T" and b["status"] == 200
    assert b["blocked_requests"] >= 2
    assert servers.secret_hits == []


@pytest.mark.parametrize("path", ["/redirect-evil", "/redirect-literal", "/redirect-tls"])
@pytest.mark.parametrize("engine", ["browser", "impersonate"])
async def test_redirects_to_internal_hosts_are_refused(client, servers, path, engine):
    r = await client.post(
        "/v1/render", headers=AUTH, json={"url": f"http://site.test:{servers.site_port}{path}", "engine": engine}
    )
    assert r.status_code == 403, r.text
    assert r.json()["error"]["code"] == "egress_blocked"
    assert servers.secret_hits == []


async def test_redirect_to_allowed_page_follows(client, servers):
    r = await client.post("/v1/render", headers=AUTH, json={"url": f"http://site.test:{servers.site_port}/redirect-ok"})
    assert r.status_code == 200 and r.json()["final_url"].endswith("/page")


async def test_direct_internal_name_is_refused(client, servers):
    r = await client.post("/v1/render", headers=AUTH, json={"url": f"http://evil.test:{servers.secret_port}/secret"})
    assert r.status_code == 403 and servers.secret_hits == []


async def test_impersonate_engine_fetches_without_js(client, servers):
    r = await client.post(
        "/v1/render",
        headers=AUTH,
        json={"url": f"http://site.test:{servers.site_port}/page", "engine": "impersonate", "include_text": True},
    )
    assert r.status_code == 200, r.text
    b = r.json()
    assert "static" in b["text"] and "rendered-by-js" not in b["text"] and b["engine"] == "impersonate"


async def test_screenshot_png(client, servers):
    r = await client.post(
        "/v1/screenshot", headers=AUTH, json={"url": f"http://site.test:{servers.site_port}/page", "format": "png"}
    )
    assert r.status_code == 200, r.text
    assert base64.b64decode(r.json()["image_base64"]).startswith(b"\x89PNG")
    assert servers.secret_hits == []


async def test_health_reports_ready_browser(client):
    r = await client.get("/v1/health", headers=AUTH)
    assert r.json()["browser"] == "ready"
