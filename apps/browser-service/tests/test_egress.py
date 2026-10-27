import asyncio
import base64
import ipaddress

import pytest

from browser_service.egress import (
    EGRESS_HEADER,
    BlockedError,
    EgressPolicy,
    EgressProxy,
    ResolveError,
    parse_ip_any_encoding,
)

LOOPBACK = (ipaddress.ip_network("127.0.0.0/8"),)


@pytest.mark.parametrize(
    "addr",
    [
        "127.0.0.1",
        "10.1.2.3",
        "172.16.0.1",
        "192.168.1.1",
        "169.254.169.254",
        "169.254.1.1",
        "100.64.0.1",
        "100.100.100.200",
        "0.0.0.0",
        "224.0.0.1",
        "255.255.255.255",
        "198.18.0.1",
        "::1",
        "::",
        "fe80::1",
        "fc00::1",
        "fd00:ec2::254",
        "::ffff:127.0.0.1",
        "::ffff:10.0.0.1",
        "64:ff9b::a9fe:a9fe",
        "2002:7f00:1::1",
        "2001:0:4136:e378::1",
        "ff02::1",
        # review L1: "global" per Python's ipaddress but not internet destinations
        "fec0::1",
        "192.88.99.1",
        "2001:20::1",
        "5f00::1",
    ],
)
def test_blocked_addresses(addr):
    assert EgressPolicy().ip_blocked(ipaddress.ip_address(addr))


@pytest.mark.parametrize("addr", ["8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"])
def test_public_addresses_allowed(addr):
    assert not EgressPolicy().ip_blocked(ipaddress.ip_address(addr))


def test_allow_cidrs_never_unblock_metadata():
    p = EgressPolicy(allow_cidrs=[ipaddress.ip_network("169.254.0.0/16"), *LOOPBACK])
    assert not p.ip_blocked(ipaddress.ip_address("127.0.0.1"))
    assert p.ip_blocked(ipaddress.ip_address("169.254.169.254"))
    assert not p.ip_blocked(ipaddress.ip_address("169.254.1.1"))


@pytest.mark.parametrize(
    "host,expected",
    [
        ("2130706433", "127.0.0.1"),
        ("0x7f000001", "127.0.0.1"),
        ("0177.0.0.1", "127.0.0.1"),
        ("127.1", "127.0.0.1"),
        ("0x7f.1", "127.0.0.1"),
        ("10.1", "10.0.0.1"),
        ("[::1]", "::1"),
        ("example.com", None),
    ],
)
def test_parse_ip_any_encoding(host, expected):
    got = parse_ip_any_encoding(host)
    assert (str(got) if got else None) == expected


@pytest.mark.parametrize(
    "url",
    [
        "file:///etc/passwd",
        "ftp://example.com/",
        "javascript:alert(1)",
        "http://localhost/",
        "http://127.0.0.1:8000/",
        "http://2130706433/",
        "http://postgres:5432/",
        "http://redis/",
        "http://metadata.google.internal/computeMetadata/v1/",
        "http://169.254.169.254/latest/",
        "http://user:pw@example.com/",
        "http://foo.internal/",
        "http://printer.local/",
        "http://[::1]/",
        "http://[fe80::1%25eth0]/",
        "",
        "http://exa mple.com/",
    ],
)
def test_check_url_rejects(url):
    with pytest.raises(BlockedError):
        EgressPolicy().check_url(url)


def test_check_url_accepts_public_names():
    assert EgressPolicy().check_url("https://Example.com/a?b=1") == ("example.com", 443)
    assert EgressPolicy(ports=None).check_url("http://example.com:8080/") == ("example.com", 8080)


# review L2: only web ports unless configured


@pytest.mark.parametrize("url", ["http://example.com:8080/", "https://example.com:22/", "http://example.com:6379/"])
def test_check_url_refuses_non_web_ports_by_default(url):
    with pytest.raises(BlockedError):
        EgressPolicy().check_url(url)
    assert EgressPolicy(ports=frozenset({8080, 22, 6379})).check_url(url)[1] in (8080, 22, 6379)


async def test_proxy_refuses_non_web_ports():
    p = EgressProxy(EgressPolicy(allow_cidrs=LOOPBACK, resolver=_resolver({"site.test": ["127.0.0.5"]})))
    await p.start()
    try:
        s = p.open_session(1 << 20)
        for target in ("site.test:22", "site.test:6379", "site.test:8443"):
            out = await _send(p, f"CONNECT {target} HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\n".encode())
            assert out.startswith(b"HTTP/1.1 403"), target
        out = await _send(p, f"GET http://site.test:25/ HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\n".encode())
        assert out.startswith(b"HTTP/1.1 403")
    finally:
        await p.close()


def test_egress_ports_setting():
    from browser_service.config import ConfigError, Settings

    tok = {"BROWSER_SERVICE_TOKEN": "t" * 40}
    assert Settings.from_env(tok).egress_ports == frozenset({80, 443})
    assert Settings.from_env({**tok, "BROWSER_EGRESS_PORTS": "80, 443,8443"}).egress_ports == frozenset({80, 443, 8443})
    for bad in ("0", "x", "70000", ""):
        if bad == "":
            continue
        with pytest.raises(ConfigError):
            Settings.from_env({**tok, "BROWSER_EGRESS_PORTS": bad})


def _resolver(table, calls=None):
    async def resolve(host, port):
        if calls is not None:
            calls.append(host)
        v = table[host]
        if isinstance(v, Exception):
            raise v
        return v() if callable(v) else v

    return resolve


async def test_resolve_blocks_when_any_address_private():
    p = EgressPolicy(
        resolver=_resolver(
            {
                "mixed.test": ["93.184.216.34", "10.0.0.5"],
                "ok.test": ["93.184.216.34"],
                "gone.test": OSError("nxdomain"),
            }
        )
    )
    with pytest.raises(BlockedError):
        await p.resolve("mixed.test", 443)
    assert await p.resolve("ok.test", 443) == ["93.184.216.34"]
    with pytest.raises(ResolveError):
        await p.resolve("gone.test", 443)
    with pytest.raises(BlockedError):
        await p.resolve("ok.test", 0)


# ── proxy ────────────────────────────────────────────────────────────


class Upstream:
    """Tiny TCP server: records the first bytes it receives and replies."""

    def __init__(self, reply: bytes):
        self.reply = reply
        self.received = b""
        self.connections = 0

    async def start(self):
        self.server = await asyncio.start_server(self._handle, "127.0.0.1", 0)
        self.port = self.server.sockets[0].getsockname()[1]
        return self

    async def _handle(self, r, w):
        self.connections += 1
        try:
            self.received += await asyncio.wait_for(r.read(65536), 2)
        except TimeoutError:
            pass
        w.write(self.reply)
        await w.drain()
        w.close()

    async def close(self):
        self.server.close()
        await self.server.wait_closed()


def _auth(session):
    return "Basic " + base64.b64encode(f"{session.sid}:{session.secret}".encode()).decode()


async def _proxy(table, calls=None):
    p = EgressProxy(
        EgressPolicy(allow_cidrs=LOOPBACK, resolver=_resolver(table, calls), ports=None),
        header_timeout=2,
        idle_timeout=2,
    )
    await p.start()
    return p


async def _send(proxy, data: bytes, read_all=True):
    r, w = await asyncio.open_connection(proxy.host, proxy.port)
    w.write(data)
    await w.drain()
    out = await asyncio.wait_for(r.read(65536) if not read_all else r.read(-1), 5)
    w.close()
    return out


async def test_proxy_requires_session_credentials():
    proxy = await _proxy({})
    try:
        out = await _send(proxy, b"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
        assert out.startswith(b"HTTP/1.1 407") and b"Proxy-Authenticate: Basic" in out
        s = proxy.open_session(1 << 20)
        bad = "Basic " + base64.b64encode(f"{s.sid}:wrong".encode()).decode()
        out = await _send(proxy, f"CONNECT example.com:443 HTTP/1.1\r\nProxy-Authorization: {bad}\r\n\r\n".encode())
        assert out.startswith(b"HTTP/1.1 407")
        proxy.close_session(s)
        out = await _send(proxy, f"CONNECT a.test:443 HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\n".encode())
        assert out.startswith(b"HTTP/1.1 407"), "closed sessions must not authenticate"
    finally:
        await proxy.close()


async def test_connect_to_private_destination_is_refused_and_attributed():
    proxy = await _proxy({"evil.test": ["10.0.0.1"]})
    try:
        s = proxy.open_session(1 << 20)
        for target in ("evil.test:443", "127.0.0.1:22", "169.254.169.254:80", "postgres:5432", "[::1]:443"):
            out = await _send(proxy, f"CONNECT {target} HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\n".encode())
            # 127.0.0.1 is in the test allow list, so it is the only one that may pass the policy
            if target.startswith("127."):
                assert not out.startswith(b"HTTP/1.1 403")
                continue
            assert out.startswith(b"HTTP/1.1 403"), target
            assert f"{EGRESS_HEADER}: blocked".encode() in out
        assert "evil.test" in s.blocked and "169.254.169.254" in s.blocked
    finally:
        await proxy.close()


async def test_connect_tunnel_pins_the_single_resolution():
    up = await (Upstream(b"pong")).start()
    answers = iter([["127.0.0.1"], ["10.9.9.9"]])  # rebinding: public, then private
    calls: list[str] = []
    proxy = await _proxy({"rebind.test": lambda: next(answers)}, calls)
    try:
        s = proxy.open_session(1 << 20)
        out = await _send(
            proxy, f"CONNECT rebind.test:{up.port} HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\nping".encode()
        )
        assert out.startswith(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        assert out.endswith(b"pong") and up.received == b"ping"
        assert calls == ["rebind.test"], "resolved exactly once per tunnel"
        out = await _send(
            proxy, f"CONNECT rebind.test:{up.port} HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\n".encode()
        )
        assert out.startswith(b"HTTP/1.1 403") and up.connections == 1
    finally:
        await proxy.close()
        await up.close()


async def test_plain_http_is_rewritten_to_origin_form():
    up = await (Upstream(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")).start()
    proxy = await _proxy({"site.test": ["127.0.0.1"]})
    try:
        s = proxy.open_session(1 << 20)
        req = (
            f"GET http://site.test:{up.port}/a/b?c=1 HTTP/1.1\r\nHost: site.test\r\n"
            f"Proxy-Authorization: {_auth(s)}\r\nProxy-Connection: keep-alive\r\nConnection: keep-alive\r\n\r\n"
        )
        out = await _send(proxy, req.encode())
        assert out.endswith(b"ok")
        sent = up.received.decode()
        assert sent.startswith("GET /a/b?c=1 HTTP/1.1\r\n")
        assert "Proxy-Authorization" not in sent and "keep-alive" not in sent and "Connection: close" in sent
    finally:
        await proxy.close()
        await up.close()


async def test_plain_http_rejects_non_http_schemes_and_garbage():
    proxy = await _proxy({})
    try:
        s = proxy.open_session(1 << 20)
        out = await _send(proxy, f"GET https://site.test/ HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\n".encode())
        assert out.startswith(b"HTTP/1.1 400")
        out = await _send(proxy, b"NONSENSE\r\n\r\n")
        assert out.startswith(b"HTTP/1.1 400")
    finally:
        await proxy.close()


async def test_session_byte_cap_cuts_the_download():
    up = await (Upstream(b"x" * 200_000)).start()
    proxy = await _proxy({"big.test": ["127.0.0.1"]})
    try:
        s = proxy.open_session(1000)
        out = await _send(
            proxy, f"CONNECT big.test:{up.port} HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\nhi".encode()
        )
        assert len(out) < 200_000 and s.over_limit
    finally:
        await proxy.close()
        await up.close()


async def test_unresolvable_host_is_502_not_blocked():
    proxy = await _proxy({"gone.test": OSError("nx")})
    try:
        s = proxy.open_session(1 << 20)
        out = await _send(proxy, f"CONNECT gone.test:443 HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\n".encode())
        assert out.startswith(b"HTTP/1.1 502") and not s.blocked
    finally:
        await proxy.close()


def test_session_repr_hides_secret():
    s = EgressProxy(EgressPolicy()).open_session(10)
    assert s.secret not in repr(s)
