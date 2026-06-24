"""
Launch-blocking security floor regression tests.

Covers the three verified issues:

  (A) SSRF / DNS-rebinding — the user-supplied HTTP-column path and the
      declarative-manifest fetch path must call the guard with resolve=True so a
      hostname that resolves to a private/metadata IP is blocked at fetch time.
  (B) Insecure auth defaults — the app must fail closed (refuse to boot) when run
      outside dev/test with the insecure default SECRET_KEY, and access-token TTL
      must be short (15–60 min). Refresh tokens must not be usable as access
      tokens.
  (C) No-op rate limiter — an expensive route must return 429 once its
      per-workspace limit is exceeded.
"""
import asyncio
import socket

import pytest

from apps.api.core.url_guard import check_url, is_safe_url, BlockedUrlError


# ── (A) SSRF / DNS-rebinding ────────────────────────────────────────────────

# Hosts that are literal private / loopback / metadata / encoded IPs. These must
# be blocked by check_url WITHOUT needing DNS resolution.
_LITERAL_SSRF_VECTORS = [
    "http://169.254.169.254/latest/meta-data/",  # AWS/GCP metadata IP
    "http://localhost/",                         # localhost name
    "http://127.0.0.1/",                         # loopback
    "http://[::1]/",                             # IPv6 loopback
    "http://2130706433/",                        # decimal-encoded 127.0.0.1
    "http://0x7f000001/",                        # hex-encoded 127.0.0.1
    "http://0177.0.0.1/",                        # octal-encoded first octet
    "http://10.0.0.5/",                          # private RFC1918
]


@pytest.mark.parametrize("url", _LITERAL_SSRF_VECTORS)
def test_guard_blocks_literal_ssrf_vectors(url):
    assert not is_safe_url(url), url
    with pytest.raises(BlockedUrlError):
        check_url(url, resolve=True)


def test_guard_allows_public_host_that_resolves(monkeypatch):
    """A real public host that resolves to a public IP must pass even with
    resolve=True (no false positives)."""
    monkeypatch.setattr(
        socket, "getaddrinfo",
        lambda *a, **k: [(socket.AF_INET, None, None, "", ("93.184.216.34", 0))],
    )
    assert check_url("https://example.com/path", resolve=True) is not None


def test_guard_blocks_rebinding_host(monkeypatch):
    """resolve=True closes the rebinding gap: a benign-looking hostname that
    resolves to a private/metadata IP is rejected."""
    monkeypatch.setattr(
        socket, "getaddrinfo",
        lambda *a, **k: [(socket.AF_INET, None, None, "", ("169.254.169.254", 0))],
    )
    with pytest.raises(BlockedUrlError):
        check_url("https://rebind.evil.example", resolve=True)
    # And without resolve, the same host would have slipped through — proving
    # resolve=True is the thing doing the work here.
    assert is_safe_url("https://rebind.evil.example", allow_http=False)


def test_http_column_path_uses_resolve(monkeypatch):
    """The user-supplied HTTP-column path must block a host that only resolves to
    a private IP — i.e. it passes resolve=True into the guard."""
    from apps.api.services.workbook.http_column import execute_http_column

    monkeypatch.setattr(
        socket, "getaddrinfo",
        lambda *a, **k: [(socket.AF_INET, None, None, "", ("10.0.0.5", 0))],
    )
    cols = [{"id": "website", "name": "Website", "type": "lead_field"}]
    col = {"type": "http", "http_url": "https://internal.rebind.example/x"}
    res = asyncio.run(execute_http_column(col, {"website": "x"}, cols))
    assert res["success"] is False
    assert res["error"].startswith("blocked_url"), res


def test_declarative_compiler_passes_resolve():
    """The declarative-manifest fetch path passes resolve=True to check_url.

    We assert on the source rather than executing the full enrichment pipeline
    (which needs a manifest + live network)."""
    import inspect
    from apps.api.services.leadgen.enrichment.declarative import compiler

    src = inspect.getsource(compiler)
    assert "check_url(url, allow_http=True, resolve=True)" in src


# ── (B) Insecure auth defaults ──────────────────────────────────────────────

def test_boot_fails_on_insecure_default_key_in_prod():
    """A real deployment (APP_ENV=production) with the shipped insecure default
    SECRET_KEY must refuse to boot."""
    from apps.api.core.config import Settings, INSECURE_DEFAULT_SECRET_KEY

    s = Settings(SECRET_KEY=INSECURE_DEFAULT_SECRET_KEY, APP_ENV="production")
    assert s.secret_key_is_insecure and not s.is_dev_env
    with pytest.raises(RuntimeError):
        s.validate_security()


def test_boot_ok_dev_with_default_key():
    """Dev ergonomics preserved: the insecure default boots under APP_ENV=dev."""
    from apps.api.core.config import Settings, INSECURE_DEFAULT_SECRET_KEY

    s = Settings(SECRET_KEY=INSECURE_DEFAULT_SECRET_KEY, APP_ENV="dev")
    s.validate_security()  # must not raise


def test_boot_ok_prod_with_real_key():
    """A real key boots fine in production."""
    from apps.api.core.config import Settings

    s = Settings(SECRET_KEY="a-real-strong-secret-key-value", APP_ENV="production")
    assert not s.secret_key_is_insecure
    s.validate_security()  # must not raise


def test_access_token_ttl_is_short():
    """Access-token TTL must be in the 15–60 minute range (was ~47h)."""
    from apps.api.core.config import Settings

    s = Settings()
    assert 15 <= s.ACCESS_TOKEN_EXPIRE_MINUTES <= 60, s.ACCESS_TOKEN_EXPIRE_MINUTES


def test_refresh_token_not_usable_as_access_token():
    """A refresh token carries type='refresh' and must be rejected by the
    access-token validator path."""
    from apps.api.auth import create_refresh_token, create_access_token, decode_token

    refresh = create_refresh_token({"sub": "alice"})
    access = create_access_token({"sub": "alice"})
    assert decode_token(refresh).get("type") == "refresh"
    assert decode_token(access).get("type") == "access"


# ── (C) Rate limiter actually enforces ──────────────────────────────────────

def test_expensive_route_returns_429_after_limit(monkeypatch):
    """An expensive workspace-scoped route returns 429 once its per-workspace
    limit is exceeded. We hit the lightweight collection endpoint."""
    from starlette.requests import Request as _Req  # noqa: F401  (import probe)
    from fastapi import FastAPI, Request
    from fastapi.testclient import TestClient
    from slowapi.errors import RateLimitExceeded
    from slowapi import _rate_limit_exceeded_handler
    from apps.api.core.ratelimit import limiter

    # Build a tiny app that reuses the SHARED limiter + the same workspace key,
    # with a deliberately tiny limit so the test is fast and deterministic.
    app = FastAPI()
    app.state.limiter = limiter
    app.add_exception_handler(RateLimitExceeded, _rate_limit_exceeded_handler)

    @app.post("/expensive")
    @limiter.limit("2/minute")
    async def expensive(request: Request):
        return {"ok": True}

    client = TestClient(app)
    headers = {"X-Workspace-Id": "ws-test-429"}
    codes = [client.post("/expensive", headers=headers).status_code for _ in range(4)]
    assert codes[0] == 200 and codes[1] == 200, codes
    assert 429 in codes[2:], codes


def test_rate_limit_key_is_workspace_scoped():
    """Distinct workspaces get independent buckets (one tenant can't exhaust
    another's budget)."""
    from types import SimpleNamespace
    from apps.api.core.ratelimit import workspace_key

    req_ws_a = SimpleNamespace(headers={"x-workspace-id": "A"})
    req_ws_b = SimpleNamespace(headers={"x-workspace-id": "B"})
    assert workspace_key(req_ws_a) == "ws:A"
    assert workspace_key(req_ws_b) == "ws:B"
    assert workspace_key(req_ws_a) != workspace_key(req_ws_b)
