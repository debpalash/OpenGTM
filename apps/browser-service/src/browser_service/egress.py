"""Egress guard: every byte the browser (or a scraping library) sends to the
internet goes through an in-process forward proxy that resolves the target once,
refuses non-public addresses, and dials the vetted IP itself (resolve-and-pin).

Why a proxy and not a request hook: Chromium resolves DNS itself, so checking a
URL in a route handler and then letting Chromium connect leaves a DNS-rebinding
window (the name can resolve to a public IP for the check and to 127.0.0.1 for
the connection). With a proxy configured, Chromium never resolves names: it
sends ``CONNECT host:port`` (https/wss) or an absolute-form request (http) to
the proxy, and the proxy is the only component that resolves and connects.
Redirect hops, subresources, iframes, WebSockets and fetch() calls all open new
proxy requests, so each one is vetted at connect time.

Each API request gets its own proxy credentials (an ``EgressSession``); the
proxy rejects unauthenticated clients (407), attributes blocked destinations to
the session, caps the bytes a session may download and tears its tunnels down
when the request ends.

Blocked: every address that is not globally routable (RFC 1918, loopback,
link-local incl. 169.254.169.254, CGNAT 100.64/10, ULA, multicast, reserved,
documentation ranges), IPv4-mapped/NAT64/6to4/Teredo forms, and the known
cloud metadata addresses. Hostnames without a dot (compose service names such
as ``postgres``) and ``localhost``/``*.internal``-style names are refused before
DNS. A hostname is refused when ANY of its addresses is blocked.
"""

from __future__ import annotations

import asyncio
import base64
import binascii
import hmac
import ipaddress
import logging
import re
import secrets
import socket
from collections.abc import Awaitable, Callable, Iterable
from dataclasses import dataclass, field
from urllib.parse import urlsplit

logger = logging.getLogger("browser_service.egress")

IPAddress = ipaddress.IPv4Address | ipaddress.IPv6Address
IPNetwork = ipaddress.IPv4Network | ipaddress.IPv6Network

# Header the proxy puts on its own refusal responses, so a plain-http navigation
# that the proxy answered can be told apart from a real 403 from the site.
EGRESS_HEADER = "x-opengtm-egress"

_METADATA_IPS = frozenset(ipaddress.ip_address(a) for a in ("169.254.169.254", "fd00:ec2::254", "100.100.100.200"))
# Ranges blocked on top of ``not is_global`` (some are "global" per IANA but can
# be translated by a gateway into an internal address).
_EXTRA_BLOCKED = tuple(
    ipaddress.ip_network(n) for n in ("100.64.0.0/10", "64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "2001::/32")
)
_BLOCKED_HOSTNAMES = frozenset(
    {"localhost", "ip6-localhost", "ip6-loopback", "metadata", "metadata.google.internal", "metadata.goog"}
)
_BLOCKED_SUFFIXES = (".localhost", ".internal", ".local", ".localdomain", ".home.arpa", ".lan", ".intranet")
_HOST_RE = re.compile(r"^[a-z0-9_]([a-z0-9_-]{0,62})(\.[a-z0-9_]([a-z0-9_-]{0,62}))*\.?$")


class BlockedError(Exception):
    """The destination is not allowed (non-public address or forbidden name)."""

    def __init__(self, reason: str) -> None:
        super().__init__(reason)
        self.reason = reason


class ResolveError(Exception):
    """The hostname did not resolve."""


def _int_any(tok: str) -> int:
    t = tok.lower()
    if t.startswith("0x"):
        return int(t, 16)
    if t.startswith("0") and len(t) > 1:
        return int(t, 8)
    return int(t, 10)


def parse_ip_any_encoding(host: str) -> IPAddress | None:
    """Parse ``host`` as an IP literal in any encoding browsers accept
    (dotted, single integer, hex, octal). None for a real hostname."""
    h = host.strip().strip("[]")
    if "%" in h:  # IPv6 zone id
        h = h.split("%", 1)[0]
    try:
        return ipaddress.ip_address(h)
    except ValueError:
        pass
    try:
        if re.fullmatch(r"0[xX][0-9a-fA-F]+|0[0-7]+|[0-9]+", h):
            return ipaddress.ip_address(_int_any(h) & 0xFFFFFFFF)
        parts = h.split(".")
        if 2 <= len(parts) <= 4 and all(re.fullmatch(r"0[xX][0-9a-fA-F]+|[0-9]+", p) for p in parts):
            nums = [_int_any(p) for p in parts]
            # inet_aton forms: a.b.c.d, a.b.c (c = 16 bits), a.b (b = 24 bits)
            value = 0
            for n in nums[:-1]:
                value = (value << 8) | (n & 0xFF)
            last_bits = 8 * (5 - len(parts))
            value = (value << last_bits) | (nums[-1] & ((1 << last_bits) - 1))
            return ipaddress.ip_address(value & 0xFFFFFFFF)
    except ValueError:
        return None
    return None


class EgressPolicy:
    """Decides which destinations are reachable.

    ``allow_cidrs`` exempts ranges from the block list (test-only: the service
    refuses to start with it outside APP_ENV=test). Metadata IPs stay blocked.
    """

    def __init__(
        self,
        allow_cidrs: Iterable[IPNetwork] = (),
        resolver: Callable[[str, int], Awaitable[list[str]]] | None = None,
    ) -> None:
        self.allow_cidrs = tuple(allow_cidrs)
        self._resolver = resolver or _system_resolve

    def ip_blocked(self, ip: IPAddress) -> bool:
        if isinstance(ip, ipaddress.IPv6Address):
            if ip.scope_id:
                ip = ipaddress.IPv6Address(str(ip).split("%", 1)[0])
            if ip.ipv4_mapped is not None:
                ip = ip.ipv4_mapped
        if ip in _METADATA_IPS:
            return True
        if any(ip in n for n in self.allow_cidrs if n.version == ip.version):
            return False
        if not ip.is_global or ip.is_multicast or ip.is_reserved or ip.is_unspecified:
            return True
        return any(ip in n for n in _EXTRA_BLOCKED if n.version == ip.version)

    def check_host(self, host: str) -> None:
        """Literal checks, no DNS. Raises BlockedError."""
        h = (host or "").strip().lower().rstrip(".")
        if not h:
            raise BlockedError("empty host")
        ip = parse_ip_any_encoding(h)
        if ip is not None:
            if self.ip_blocked(ip):
                raise BlockedError("non-public address")
            return
        if h in _BLOCKED_HOSTNAMES or h.endswith(_BLOCKED_SUFFIXES):
            raise BlockedError("internal hostname")
        if "." not in h:
            raise BlockedError("single-label hostname")
        if not _HOST_RE.match(h):
            raise BlockedError("invalid hostname")

    def check_url(self, url: str) -> tuple[str, int]:
        """Validate a URL a caller asked us to open. Returns (host, port)."""
        if not url or len(url) > 4096:
            raise BlockedError("invalid url")
        try:
            parts = urlsplit(url.strip())
            port = parts.port
        except ValueError as e:
            raise BlockedError("invalid url") from e
        scheme = (parts.scheme or "").lower()
        if scheme not in ("http", "https"):
            raise BlockedError("scheme not allowed")
        if parts.username is not None or parts.password is not None:
            raise BlockedError("credentials in url")
        host = parts.hostname or ""
        self.check_host(host)
        return host, port or (443 if scheme == "https" else 80)

    async def resolve(self, host: str, port: int) -> list[str]:
        """Resolve ``host`` once and return its addresses, all of them vetted.
        Raises BlockedError when any address is blocked, ResolveError when none."""
        if not 0 < port < 65536:
            raise BlockedError("invalid port")
        self.check_host(host)
        literal = parse_ip_any_encoding(host)
        if literal is not None:
            return [str(literal)]
        try:
            addrs = await self._resolver(host.rstrip("."), port)
        except (OSError, UnicodeError) as e:
            raise ResolveError("resolve failed") from e
        out: list[str] = []
        for a in addrs:
            try:
                ip = ipaddress.ip_address(a.split("%", 1)[0])
            except ValueError as e:
                raise BlockedError("unparseable address") from e
            if self.ip_blocked(ip):
                raise BlockedError("resolves to non-public address")
            if str(ip) not in out:
                out.append(str(ip))
        if not out:
            raise ResolveError("no addresses")
        return out


async def _system_resolve(host: str, port: int) -> list[str]:
    loop = asyncio.get_running_loop()
    infos = await loop.getaddrinfo(host, port, type=socket.SOCK_STREAM)
    return [str(info[4][0]) for info in infos]


@dataclass(eq=False)
class EgressSession:
    """Proxy credentials and accounting for one API request."""

    sid: str
    secret: str
    max_bytes: int
    blocked: list[str] = field(default_factory=list)
    bytes_in: int = 0
    over_limit: bool = False
    closed: bool = False
    _tasks: set[asyncio.Task] = field(default_factory=set, repr=False)

    def record_blocked(self, host: str) -> None:
        if len(self.blocked) < 50:
            self.blocked.append(host)

    def __repr__(self) -> str:  # never print the secret
        return f"EgressSession(sid={self.sid!r}, blocked={len(self.blocked)}, bytes_in={self.bytes_in})"


_HOP_HEADERS = frozenset(
    {"proxy-authorization", "proxy-connection", "connection", "keep-alive", "te", "trailer", "upgrade"}
)


class EgressProxy:
    """Authenticated HTTP forward proxy (CONNECT + absolute-form GET/POST/...)
    bound to loopback. See the module docstring."""

    def __init__(
        self,
        policy: EgressPolicy,
        *,
        connect_timeout: float = 10.0,
        header_timeout: float = 10.0,
        idle_timeout: float = 60.0,
    ) -> None:
        self.policy = policy
        self.connect_timeout = connect_timeout
        self.header_timeout = header_timeout
        self.idle_timeout = idle_timeout
        self._sessions: dict[str, EgressSession] = {}
        self._server: asyncio.Server | None = None
        self.host = "127.0.0.1"
        self.port = 0

    async def start(self, host: str = "127.0.0.1", port: int = 0) -> None:
        self._server = await asyncio.start_server(self._handle, host, port, limit=64 * 1024)
        sock = self._server.sockets[0]
        self.host, self.port = host, sock.getsockname()[1]

    async def close(self) -> None:
        for s in list(self._sessions.values()):
            self.close_session(s)
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()
            self._server = None

    @property
    def server_url(self) -> str:
        return f"http://{self.host}:{self.port}"

    def proxy_url(self, session: EgressSession) -> str:
        return f"http://{session.sid}:{session.secret}@{self.host}:{self.port}"

    def open_session(self, max_bytes: int) -> EgressSession:
        s = EgressSession(sid="s" + secrets.token_hex(8), secret=secrets.token_urlsafe(24), max_bytes=max_bytes)
        self._sessions[s.sid] = s
        return s

    def close_session(self, session: EgressSession) -> None:
        session.closed = True
        self._sessions.pop(session.sid, None)
        for t in list(session._tasks):
            t.cancel()

    def _authenticate(self, headers: list[tuple[str, str]]) -> EgressSession | None:
        for name, value in headers:
            if name.lower() != "proxy-authorization":
                continue
            scheme, _, cred = value.strip().partition(" ")
            if scheme.lower() != "basic":
                return None
            try:
                raw = base64.b64decode(cred.strip(), validate=True).decode("utf-8")
            except (binascii.Error, UnicodeDecodeError):
                return None
            sid, _, secret = raw.partition(":")
            s = self._sessions.get(sid)
            if s is None or s.closed:
                return None
            if hmac.compare_digest(s.secret.encode(), secret.encode()):
                return s
            return None
        return None

    async def _handle(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        task = asyncio.current_task()
        session: EgressSession | None = None
        try:
            try:
                head = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), self.header_timeout)
            except (asyncio.IncompleteReadError, asyncio.LimitOverrunError, TimeoutError, ConnectionError):
                return
            lines = head[:-4].decode("latin-1").split("\r\n")
            parts = lines[0].split(" ")
            if len(parts) != 3 or not parts[2].startswith("HTTP/1."):
                await _reply(writer, 400, "Bad Request")
                return
            method, target, _version = parts
            headers: list[tuple[str, str]] = []
            for line in lines[1:]:
                name, sep, value = line.partition(":")
                if not sep:
                    await _reply(writer, 400, "Bad Request")
                    return
                headers.append((name.strip(), value.strip()))
            session = self._authenticate(headers)
            if session is None:
                await _reply(
                    writer, 407, "Proxy Authentication Required", extra=[("Proxy-Authenticate", 'Basic realm="egress"')]
                )
                return
            if task is not None:
                session._tasks.add(task)

            if method.upper() == "CONNECT":
                host, port = _split_hostport(target)
                if host is None:
                    await _reply(writer, 400, "Bad Request")
                    return
                upstream = await self._dial(session, writer, host, port)
                if upstream is None:
                    return
                up_reader, up_writer = upstream
                writer.write(b"HTTP/1.1 200 Connection Established\r\n\r\n")
                await writer.drain()
            else:
                try:
                    u = urlsplit(target)
                    port = u.port or 80
                except ValueError:
                    await _reply(writer, 400, "Bad Request")
                    return
                if u.scheme.lower() != "http" or not u.hostname:
                    await _reply(writer, 400, "Bad Request")
                    return
                upstream = await self._dial(session, writer, u.hostname, port)
                if upstream is None:
                    return
                up_reader, up_writer = upstream
                path = u.path or "/"
                if u.query:
                    path += "?" + u.query
                out = [f"{method} {path} HTTP/1.1"]
                out += [f"{n}: {v}" for n, v in headers if n.lower() not in _HOP_HEADERS]
                out.append("Connection: close")
                up_writer.write(("\r\n".join(out) + "\r\n\r\n").encode("latin-1"))
                await up_writer.drain()
            await self._splice(session, reader, writer, up_reader, up_writer)
        except asyncio.CancelledError:
            pass
        except Exception:  # pragma: no cover - defensive; never crash the server
            logger.exception("egress proxy connection failed")
        finally:
            if session is not None and task is not None:
                session._tasks.discard(task)
            _close(writer)

    async def _dial(self, session: EgressSession, writer: asyncio.StreamWriter, host: str, port: int):
        try:
            ips = await self.policy.resolve(host, port)
        except BlockedError as e:
            session.record_blocked(host)
            logger.info("egress blocked host=%s reason=%s", host, e.reason)
            await _reply(writer, 403, "Forbidden", extra=[(EGRESS_HEADER, "blocked")])
            return None
        except ResolveError:
            await _reply(writer, 502, "Bad Gateway", extra=[(EGRESS_HEADER, "unresolved")])
            return None
        for ip in ips:  # pinned: connect to the vetted addresses only
            try:
                return await asyncio.wait_for(asyncio.open_connection(ip, port), self.connect_timeout)
            except (OSError, TimeoutError):
                continue
        await _reply(writer, 502, "Bad Gateway", extra=[(EGRESS_HEADER, "unreachable")])
        return None

    async def _splice(self, session, c_reader, c_writer, u_reader, u_writer) -> None:
        async def pipe(src: asyncio.StreamReader, dst: asyncio.StreamWriter, count: bool) -> None:
            try:
                while not session.closed:
                    data = await asyncio.wait_for(src.read(65536), self.idle_timeout)
                    if not data:
                        return
                    if count:
                        session.bytes_in += len(data)
                        if session.bytes_in > session.max_bytes:
                            session.over_limit = True
                            return
                    dst.write(data)
                    await dst.drain()
            except (OSError, TimeoutError, asyncio.IncompleteReadError):
                return

        up = asyncio.ensure_future(pipe(c_reader, u_writer, False))
        down = asyncio.ensure_future(pipe(u_reader, c_writer, True))
        try:
            await asyncio.wait({up, down}, return_when=asyncio.FIRST_COMPLETED)
        finally:
            for t in (up, down):
                t.cancel()
            await asyncio.gather(up, down, return_exceptions=True)
            _close(u_writer)


def _split_hostport(target: str) -> tuple[str | None, int]:
    m = re.fullmatch(r"\[([0-9a-fA-F:.%a-zA-Z]+)\]:(\d{1,5})|([^:\[\]]+):(\d{1,5})", target)
    if not m:
        return None, 0
    host = m.group(1) or m.group(3)
    port = int(m.group(2) or m.group(4))
    if not 0 < port < 65536:
        return None, 0
    return host, port


async def _reply(writer: asyncio.StreamWriter, code: int, reason: str, extra: list[tuple[str, str]] = ()) -> None:
    lines = [f"HTTP/1.1 {code} {reason}", "Content-Length: 0", "Connection: close"]
    lines += [f"{n}: {v}" for n, v in extra]
    try:
        writer.write(("\r\n".join(lines) + "\r\n\r\n").encode("latin-1"))
        await writer.drain()
    except (OSError, ConnectionError):
        pass


def _close(writer: asyncio.StreamWriter) -> None:
    try:
        writer.close()
    except Exception:  # noqa: S110  # pragma: no cover - already closed
        pass
