"""Upstream proxy chaining: Go picks a proxy (PROXY_LIST / proxy manager) per
request; the egress proxy still resolves and vets the target, then tunnels to
the vetted IP through that upstream. Credentials live only in the session."""

import asyncio
import base64
import ipaddress
import struct

import pytest

from browser_service.egress import EGRESS_HEADER, EgressPolicy, EgressProxy, UpstreamProxy, parse_upstream

LOOPBACK = (ipaddress.ip_network("127.0.0.0/8"),)


def _resolver(table):
    async def resolve(host, port):
        v = table[host]
        if isinstance(v, Exception):
            raise v
        return list(v)

    return resolve


def _auth(session):
    return "Basic " + base64.b64encode(f"{session.sid}:{session.secret}".encode()).decode()


async def _proxy(table, allow=LOOPBACK):
    p = EgressProxy(EgressPolicy(allow_cidrs=allow, resolver=_resolver(table)), header_timeout=2, idle_timeout=2)
    await p.start()
    return p


async def _send(proxy, data: bytes):
    r, w = await asyncio.open_connection(proxy.host, proxy.port)
    w.write(data)
    await w.drain()
    out = await asyncio.wait_for(r.read(-1), 5)
    w.close()
    return out


class FakeUpstream:
    """A fake upstream proxy (http CONNECT, socks5 or socks4). It records the
    handshake and then plays the origin: answers 'pong' to whatever it gets."""

    def __init__(self, kind: str, *, refuse: bool = False):
        self.kind = kind
        self.refuse = refuse
        self.connections = 0
        self.target = None
        self.auth = None
        self.payload = b""

    async def start(self):
        self.server = await asyncio.start_server(self._handle, "127.0.0.1", 0)
        self.port = self.server.sockets[0].getsockname()[1]
        return self

    async def close(self):
        self.server.close()
        await self.server.wait_closed()

    async def _handle(self, r, w):
        self.connections += 1
        try:
            ok = await getattr(self, "_" + self.kind)(r, w)
            if ok:
                try:
                    self.payload += await asyncio.wait_for(r.read(65536), 2)
                except TimeoutError:
                    pass
                w.write(b"pong")
                await w.drain()
        finally:
            w.close()

    async def _http(self, r, w):
        head = (await r.readuntil(b"\r\n\r\n")).decode()
        line, *hdrs = head.strip().split("\r\n")
        self.target = line
        for h in hdrs:
            n, _, v = h.partition(":")
            if n.lower() == "proxy-authorization":
                self.auth = base64.b64decode(v.strip().split(" ", 1)[1]).decode()
        if self.refuse:
            w.write(b"HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
            return False
        w.write(b"HTTP/1.1 200 Connection established\r\n\r\n")
        return True

    async def _socks5(self, r, w):
        ver, n = await r.readexactly(2)
        methods = await r.readexactly(n)
        if 2 in methods:
            w.write(b"\x05\x02")
            await w.drain()
            await r.readexactly(1)
            ulen = (await r.readexactly(1))[0]
            user = (await r.readexactly(ulen)).decode()
            plen = (await r.readexactly(1))[0]
            pw = (await r.readexactly(plen)).decode()
            self.auth = f"{user}:{pw}"
            w.write(b"\x01\x00")
        else:
            w.write(b"\x05\x00")
        await w.drain()
        _ver, _cmd, _rsv, atyp = await r.readexactly(4)
        if atyp == 1:
            addr = str(ipaddress.IPv4Address(await r.readexactly(4)))
        elif atyp == 4:
            addr = str(ipaddress.IPv6Address(await r.readexactly(16)))
        else:
            ln = (await r.readexactly(1))[0]
            addr = "name:" + (await r.readexactly(ln)).decode()
        (port,) = struct.unpack(">H", await r.readexactly(2))
        self.target = f"{addr}:{port}"
        if self.refuse:
            w.write(b"\x05\x05\x00\x01" + b"\x00" * 6)
            return False
        w.write(b"\x05\x00\x00\x01" + b"\x00" * 6)
        return True

    async def _socks4(self, r, w):
        _vn, _cd = await r.readexactly(2)
        (port,) = struct.unpack(">H", await r.readexactly(2))
        addr = str(ipaddress.IPv4Address(await r.readexactly(4)))
        user = (await r.readuntil(b"\x00"))[:-1].decode()
        self.target = f"{addr}:{port}"
        self.auth = user
        w.write(b"\x00\x5a" + b"\x00" * 6)
        return True


# ── parsing ──────────────────────────────────────────────────────────


@pytest.mark.parametrize(
    "url,scheme,host,port,user,pw",
    [
        ("http://u:p%40ss@1.2.3.4:8080", "http", "1.2.3.4", 8080, "u", "p@ss"),
        ("socks5://9.9.9.9:1080", "socks5", "9.9.9.9", 1080, None, None),
        ("socks5h://a:b@proxy.example.com:1080", "socks5", "proxy.example.com", 1080, "a", "b"),
        ("socks4://5.5.5.5:1080", "socks4", "5.5.5.5", 1080, None, None),
        ("HTTP://[2606:4700::1]:3128", "http", "2606:4700::1", 3128, None, None),
    ],
)
def test_parse_upstream(url, scheme, host, port, user, pw):
    up = parse_upstream(url)
    assert (up.scheme, up.host, up.port, up.username, up.password) == (scheme, host, port, user, pw)


@pytest.mark.parametrize(
    "url",
    ["https://1.2.3.4:443", "ftp://1.2.3.4:21", "http://1.2.3.4", "socks5://:1080", "1.2.3.4:8080", "http://h:0", ""],
)
def test_parse_upstream_rejects(url):
    with pytest.raises(ValueError):
        parse_upstream(url)


def test_upstream_repr_hides_credentials():
    up = parse_upstream("http://user:hunter2@1.2.3.4:8080")
    assert "hunter2" not in repr(up) and "user" not in repr(up) and "hunter2" not in str(up)


# ── chaining ─────────────────────────────────────────────────────────


@pytest.mark.parametrize("kind", ["http", "socks5", "socks4"])
async def test_connect_chains_through_upstream_to_the_vetted_ip(kind):
    up = await FakeUpstream(kind).start()
    proxy = await _proxy({"site.test": ["127.0.0.7"]})
    try:
        s = proxy.open_session(1 << 20, upstream=parse_upstream(f"{kind}://us:pw@127.0.0.1:{up.port}"))
        out = await _send(proxy, f"CONNECT site.test:443 HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\nping".encode())
        assert out.startswith(b"HTTP/1.1 200 Connection Established\r\n\r\n") and out.endswith(b"pong")
        assert up.payload == b"ping"
        # the upstream is asked for the pinned address, never the name
        assert "127.0.0.7" in up.target and "443" in up.target and "site.test" not in up.target
        if kind == "socks4":
            assert up.auth == "us"
        else:
            assert up.auth == "us:pw"
    finally:
        await proxy.close()
        await up.close()


async def test_plain_http_chains_through_upstream_in_origin_form():
    up = await FakeUpstream("http").start()
    proxy = await _proxy({"site.test": ["127.0.0.7"]})
    try:
        s = proxy.open_session(1 << 20, upstream=parse_upstream(f"http://127.0.0.1:{up.port}"))
        out = await _send(
            proxy, f"GET http://site.test/x?y=1 HTTP/1.1\r\nHost: site.test\r\nProxy-Authorization: {_auth(s)}\r\n\r\n".encode()
        )
        assert out.endswith(b"pong")
        assert up.target == "CONNECT 127.0.0.7:80 HTTP/1.1" and up.auth is None
        assert up.payload.startswith(b"GET /x?y=1 HTTP/1.1\r\n") and b"Proxy-Authorization" not in up.payload
    finally:
        await proxy.close()
        await up.close()


async def test_blocked_target_never_reaches_the_upstream():
    up = await FakeUpstream("http").start()
    proxy = await _proxy({"evil.test": ["10.0.0.1"]})
    try:
        s = proxy.open_session(1 << 20, upstream=parse_upstream(f"http://127.0.0.1:{up.port}"))
        for target in ("evil.test:443", "169.254.169.254:80", "postgres:5432"):
            out = await _send(proxy, f"CONNECT {target} HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\n".encode())
            assert out.startswith(b"HTTP/1.1 403") and f"{EGRESS_HEADER}: blocked".encode() in out
        assert up.connections == 0
    finally:
        await proxy.close()
        await up.close()


async def test_private_upstream_is_refused():
    proxy = await _proxy({"site.test": ["127.0.0.7"], "gw.test": ["10.1.1.1"]})
    try:
        for url in ("http://10.0.0.9:3128", "socks5://gw.test:1080", "http://169.254.169.254:80"):
            s = proxy.open_session(1 << 20, upstream=parse_upstream(url))
            out = await _send(proxy, f"CONNECT site.test:443 HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\n".encode())
            assert out.startswith(b"HTTP/1.1 502") and f"{EGRESS_HEADER}: upstream_blocked".encode() in out
            assert s.upstream_failed
    finally:
        await proxy.close()


@pytest.mark.parametrize("kind", ["http", "socks5"])
async def test_upstream_refusal_is_502_and_flagged(kind):
    up = await FakeUpstream(kind, refuse=True).start()
    proxy = await _proxy({"site.test": ["127.0.0.7"]})
    try:
        s = proxy.open_session(1 << 20, upstream=parse_upstream(f"{kind}://127.0.0.1:{up.port}"))
        out = await _send(proxy, f"CONNECT site.test:443 HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\n".encode())
        assert out.startswith(b"HTTP/1.1 502") and f"{EGRESS_HEADER}: upstream_failed".encode() in out
        assert s.upstream_failed and not s.blocked
    finally:
        await proxy.close()
        await up.close()


async def test_socks4_cannot_carry_ipv6():
    up = await FakeUpstream("socks4").start()
    proxy = await _proxy({"v6.test": ["2606:4700::1111"]})
    try:
        s = proxy.open_session(1 << 20, upstream=parse_upstream(f"socks4://127.0.0.1:{up.port}"))
        out = await _send(proxy, f"CONNECT v6.test:443 HTTP/1.1\r\nProxy-Authorization: {_auth(s)}\r\n\r\n".encode())
        assert out.startswith(b"HTTP/1.1 502")
    finally:
        await proxy.close()
        await up.close()


def test_session_repr_hides_upstream():
    s = EgressProxy(EgressPolicy()).open_session(10, upstream=parse_upstream("http://u:secretpw@1.2.3.4:8080"))
    assert "secretpw" not in repr(s)
    assert isinstance(s.upstream, UpstreamProxy)
