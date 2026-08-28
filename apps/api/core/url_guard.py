"""
SSRF url-guard — validate any user-supplied URL before the server fetches it.

Ported from eliasstravik/rowbound (core/url-guard.ts). Yupcha is adding
user-defined HTTP columns + source-engine endpoints; every such URL MUST pass
this guard first. See docs/research/clay-alternatives-ingestion-catalog.md (top-10 #7).

Blocks: non-http(s) schemes; private / loopback / link-local / unique-local
IPv4 + IPv6 (incl. IPv4-mapped); the AWS/GCP metadata IP (169.254.169.254); and
numeric / octal / hex / dotted-decimal IP encodings that smuggle a private host
past a naive string check.

Caveat (documented honestly, as in the source): this validates the URL's host
at call time. It does NOT close the DNS-rebinding hole (a hostname that resolves
to a public IP now and a private one at fetch time). For defense in depth,
resolve + re-check at connect time, or fetch through an egress proxy.
"""

import ipaddress
import re
import socket
from typing import Optional
from urllib.parse import urljoin, urlparse

# Hostnames we never allow regardless of resolution.
_BLOCKED_HOSTNAMES = {
    "localhost", "ip6-localhost", "ip6-loopback",
    "metadata.google.internal", "metadata",
}

# AWS/GCP/Azure link-local metadata endpoint.
_METADATA_IPS = {"169.254.169.254", "fd00:ec2::254"}


class BlockedUrlError(ValueError):
    """Raised when a URL is not safe for server-side fetching."""


async def guarded_get(client, url: str, *, max_redirects: int = 6, **kwargs):
    """GET a URL while validating the initial target and every redirect hop.

    The caller must create ``client`` with ``follow_redirects=False``.  Keeping
    redirect handling here makes the security invariant reusable by workbook
    actions and enrichment providers instead of relying on each HTTP library's
    automatic redirect implementation.
    """
    current = url
    for _hop in range(max_redirects + 1):
        check_url(current, allow_http=True, resolve=True)
        response = await client.get(current, **kwargs)
        if response.status_code not in (301, 302, 303, 307, 308):
            return response
        location = response.headers.get("location")
        if not location:
            return response
        current = urljoin(current, location)
    raise BlockedUrlError("too many redirects")


def _ip_is_blocked(ip: ipaddress._BaseAddress) -> bool:
    if isinstance(ip, ipaddress.IPv6Address) and ip.ipv4_mapped is not None:
        ip = ip.ipv4_mapped  # unwrap ::ffff:10.0.0.1
    return (
        ip.is_private or ip.is_loopback or ip.is_link_local
        or ip.is_reserved or ip.is_multicast or ip.is_unspecified
        or str(ip) in _METADATA_IPS
    )


def _parse_ip_any_encoding(host: str) -> Optional[ipaddress._BaseAddress]:
    """Parse a host that is an IP in ANY encoding (decimal/octal/hex/dotted).

    `int(host, 0)` handles `2130706433` (decimal), `0x7f000001` (hex), and
    `0177.0.0.1`-style octal via the dotted path below. Returns None if the host
    is a real domain name (let DNS handle it).
    """
    h = host.strip().strip("[]")  # strip IPv6 brackets
    # straight IPv4/IPv6 literal
    try:
        return ipaddress.ip_address(h)
    except ValueError:
        pass

    def _int_any(tok: str) -> int:
        """Parse a numeric token as hex (0x..), classic octal (leading 0), or
        decimal. Python's int(x, 0) rejects classic-octal leading zeros, so we
        handle them explicitly — that's exactly the smuggling vector."""
        t = tok.lower()
        if t.startswith("0x"):
            return int(t, 16)
        if t.startswith("0") and len(t) > 1:
            return int(t, 8)
        return int(t, 10)

    # single integer (decimal/hex/octal) → IPv4
    if re.fullmatch(r"0[xX][0-9a-fA-F]+|0[0-7]+|[0-9]+", h):
        try:
            return ipaddress.ip_address(_int_any(h) & 0xFFFFFFFF)
        except ValueError:
            return None
    # dotted with octal/hex/decimal octets, e.g. 0177.0.0.1 or 0x7f.0.0.1
    parts = h.split(".")
    if len(parts) == 4 and all(re.fullmatch(r"0[xX][0-9a-fA-F]+|[0-9]+", p) for p in parts):
        try:
            packed = bytes(_int_any(p) & 0xFF for p in parts)
            return ipaddress.ip_address(packed)
        except ValueError:
            return None
    return None


def is_safe_url(url: str, *, allow_http: bool = True) -> bool:
    try:
        check_url(url, allow_http=allow_http)
        return True
    except BlockedUrlError:
        return False


def check_url(url: str, *, allow_http: bool = True, resolve: bool = False) -> str:
    """Validate `url` for server-side fetching. Returns it if safe, else raises.

    resolve=True additionally resolves the hostname and blocks if ANY resolved
    address is private (closes most of the rebinding gap at validation time).
    """
    if not url or not isinstance(url, str):
        raise BlockedUrlError("empty url")

    parsed = urlparse(url.strip())
    scheme = (parsed.scheme or "").lower()
    allowed_schemes = {"https"} | ({"http"} if allow_http else set())
    if scheme not in allowed_schemes:
        raise BlockedUrlError(f"scheme '{scheme}' not allowed")

    host = (parsed.hostname or "").lower()
    if not host:
        raise BlockedUrlError("no host")
    if host in _BLOCKED_HOSTNAMES:
        raise BlockedUrlError(f"host '{host}' is blocked")

    # If the host is an IP in any encoding, validate it directly.
    ip = _parse_ip_any_encoding(host)
    if ip is not None:
        if _ip_is_blocked(ip):
            raise BlockedUrlError(f"ip '{ip}' is private/blocked")
        return url

    if resolve:
        try:
            infos = socket.getaddrinfo(host, None)
        except socket.gaierror as e:
            raise BlockedUrlError(f"cannot resolve host: {e}")
        for info in infos:
            addr = info[4][0]
            # NB: BlockedUrlError subclasses ValueError, so we must NOT wrap the
            # block-check in `except ValueError` — that would swallow our own
            # raise and silently defeat the rebinding guard. Only the
            # ip_address() parse can raise a (non-blocking) ValueError.
            try:
                parsed_ip = ipaddress.ip_address(addr)
            except ValueError:
                continue
            if _ip_is_blocked(parsed_ip):
                raise BlockedUrlError(f"host resolves to private ip {addr}")
    return url
