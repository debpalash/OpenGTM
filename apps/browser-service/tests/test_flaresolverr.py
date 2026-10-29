"""FlareSolverr fetches with its own browser, so it must be routed through the
egress proxy with per-request credentials (resolve-and-pin for every connection
it makes), not just pre-vetted by DNS."""

import asyncio
import base64
import http.server
import ipaddress
import json
import socket
import threading
from urllib.parse import urlsplit

import httpx
import pytest
from conftest import AUTH, FakePool, make_settings

from browser_service.app import create_app
from browser_service.config import ConfigError, Settings
from browser_service.egress import EgressPolicy


class Origin:
    """A loopback 'website' FlareSolverr's browser can reach through the proxy."""

    def __init__(self):
        self.sock = socket.socket()
        self.sock.bind(("127.0.0.1", 0))
        self.sock.listen()
        self.port = self.sock.getsockname()[1]
        threading.Thread(target=self._serve, daemon=True).start()

    def _serve(self):
        while True:
            try:
                conn, _ = self.sock.accept()
            except OSError:
                return
            with conn:
                conn.recv(1024)
                conn.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")

    def close(self):
        self.sock.close()


class FakeFlareSolverr:
    """Records the payload, then behaves like a browser told to use the given
    proxy. mode "good": fetches the origin through it; "evil": also tries an
    internal host through it; "ignore": never touches the proxy (a FlareSolverr
    build that drops the proxy object)."""

    def __init__(self, origin_port=0, mode="good"):
        self.payloads = []
        self.tunnel_status = []
        self.mode = mode
        outer = self

        def tunnel(proxy, target):
            u = urlsplit(proxy["url"])
            cred = base64.b64encode(f"{proxy.get('username', '')}:{proxy.get('password', '')}".encode())
            with socket.create_connection((u.hostname, u.port), timeout=5) as s:
                s.sendall(f"CONNECT {target} HTTP/1.1\r\nProxy-Authorization: Basic ".encode() + cred + b"\r\n\r\n")
                head = s.recv(64)
                outer.tunnel_status.append(head.split(b"\r\n")[0].decode())
                if head.startswith(b"HTTP/1.1 200"):
                    s.sendall(b"GET / HTTP/1.1\r\nHost: origin.test\r\n\r\n")
                    s.recv(1024)

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                outer.payloads.append(body)
                proxy = body.get("proxy") or {}
                if proxy.get("url") and outer.mode != "ignore":
                    tunnel(proxy, f"origin.test:{origin_port}")
                    if outer.mode == "evil":
                        tunnel(proxy, "evil.test:443")
                out = json.dumps(
                    {
                        "status": "ok",
                        "solution": {"url": body["url"], "status": 200, "response": "<html><title>ok</title></html>"},
                    }
                ).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(out)))
                self.end_headers()
                self.wfile.write(out)

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
async def setup(request):
    origin = Origin()
    fs = FakeFlareSolverr(origin.port, getattr(request, "param", "good"))
    table = {"site.test": ["93.184.216.34"], "evil.test": ["10.0.0.7"], "origin.test": ["127.0.0.1"]}

    async def resolve(host, port):
        return table[host]

    policy = EgressPolicy(allow_cidrs=[ipaddress.ip_network("127.0.0.1/32")], resolver=resolve, ports=None)
    app = create_app(make_settings(flaresolverr_url=fs.url, launch_browser=False), pool=FakePool(), policy=policy)
    cm = app.router.lifespan_context(app)
    await cm.__aenter__()
    c = httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://svc", timeout=30)
    yield c, fs
    await c.aclose()
    await cm.__aexit__(None, None, None)
    fs.close()
    origin.close()


async def test_flaresolverr_is_routed_through_the_egress_proxy(setup):
    c, fs = setup
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://site.test/", "engine": "flaresolverr"})
    assert r.status_code == 200, r.text
    proxy = fs.payloads[0]["proxy"]
    assert proxy["url"].startswith("http://") and proxy["username"] and proxy["password"]
    assert proxy["password"] not in r.text
    assert fs.tunnel_status == ["HTTP/1.1 200 Connection Established"]
    assert r.json()["blocked_requests"] == 0


# review M2: fail closed


@pytest.mark.parametrize("setup", ["evil"], indirect=True)
async def test_flaresolverr_result_is_refused_when_the_proxy_refused_a_destination(setup):
    c, fs = setup
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://site.test/", "engine": "flaresolverr"})
    assert fs.tunnel_status == ["HTTP/1.1 200 Connection Established", "HTTP/1.1 403 Forbidden"]
    assert r.status_code == 403 and r.json()["error"]["code"] == "egress_blocked"
    assert "ok" not in r.json()["error"]["message"]


@pytest.mark.parametrize("setup", ["ignore"], indirect=True)
async def test_flaresolverr_result_is_refused_when_the_proxy_saw_no_traffic(setup):
    c, fs = setup
    r = await c.post("/v1/render", headers=AUTH, json={"url": "https://site.test/", "engine": "flaresolverr"})
    assert fs.payloads and not fs.tunnel_status
    assert r.status_code == 502 and r.json()["error"]["code"] == "upstream_error"


async def test_flaresolverr_session_is_closed_after_the_call(setup):
    c, fs = setup
    await c.post("/v1/render", headers=AUTH, json={"url": "https://site.test/", "engine": "flaresolverr"})
    proxy = fs.payloads[0]["proxy"]
    u = urlsplit(proxy["url"])
    cred = base64.b64encode(f"{proxy['username']}:{proxy['password']}".encode()).decode()
    r, w = await asyncio.open_connection(u.hostname, u.port)
    w.write(f"CONNECT site.test:443 HTTP/1.1\r\nProxy-Authorization: Basic {cred}\r\n\r\n".encode())
    await w.drain()
    assert (await r.read(64)).startswith(b"HTTP/1.1 407")
    w.close()


def test_flaresolverr_requires_a_reachable_proxy_address():
    env = {"BROWSER_SERVICE_TOKEN": "t" * 40, "FLARESOLVERR_URL": "http://flaresolverr:8191"}
    with pytest.raises(ConfigError):
        Settings.from_env(env)
    s = Settings.from_env(
        {**env, "BROWSER_EGRESS_PROXY_LISTEN": "0.0.0.0:8091", "BROWSER_EGRESS_PROXY_URL": "http://browser:8091"}
    )
    assert s.egress_proxy_host == "0.0.0.0" and s.egress_proxy_port == 8091
    assert s.egress_proxy_url == "http://browser:8091"
