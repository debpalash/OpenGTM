"""
Reacher verifier — status mapping, graceful fallback, circuit breaker, SSRF
allowlist, and the global-only URL guarantee. NEVER hits a real Reacher/SMTP
server: the httpx client is mocked entirely.
"""
import asyncio

import pytest

from apps.api.services.leadgen.enrichment import email_verify_cascade as cascade
from apps.api.services.leadgen.enrichment.providers import reacher_verify as rv
from apps.api.services.leadgen.enrichment.providers.reacher_verify import (
    ReacherVerifier, map_reacher_status, validate_reacher_url, _CircuitBreaker,
)


# ── status mapping (every is_reachable × catch_all/disposable/role) ──────────

def _payload(reachable, *, catch_all=False, disposable=False, role=False):
    return {
        "input": "x@y.com",
        "is_reachable": reachable,
        "smtp": {"is_catch_all": catch_all},
        "misc": {"is_disposable": disposable, "is_role_account": role},
    }


def test_map_safe_to_valid():
    r = map_reacher_status(_payload("safe"))
    assert r.status == cascade.VALID and r.source == "reacher"


def test_map_invalid():
    assert map_reacher_status(_payload("invalid")).status == cascade.INVALID


def test_map_risky_catch_all_to_catch_all():
    r = map_reacher_status(_payload("risky", catch_all=True))
    assert r.status == cascade.CATCH_ALL and "catch_all" in r.detail


def test_map_risky_non_catch_all_to_unknown():
    # risky-but-not-catch-all is non-definitive → cascade should corroborate
    assert map_reacher_status(_payload("risky")).status == cascade.UNKNOWN


def test_map_unknown_to_unknown():
    # Gmail/Microsoft block probing → unknown → cascade falls through
    assert map_reacher_status(_payload("unknown")).status == cascade.UNKNOWN


def test_map_disposable_to_invalid_regardless():
    r = map_reacher_status(_payload("safe", disposable=True))
    assert r.status == cascade.INVALID and r.detail == "disposable"


def test_map_role_surfaced_in_detail():
    r = map_reacher_status(_payload("safe", role=True))
    assert r.status == cascade.VALID and "role" in r.detail


# ── graceful fallback: errors/timeouts → UNKNOWN, never raise ────────────────

def _verifier(monkeypatch, *, enabled=True, url="http://reacher:8080"):
    monkeypatch.setattr(ReacherVerifier, "_enabled", lambda self: enabled)
    monkeypatch.setattr(ReacherVerifier, "_url", lambda self: url)
    monkeypatch.setattr(ReacherVerifier, "_api_key", lambda self: "")
    monkeypatch.setattr(ReacherVerifier, "_timeout", lambda self: 1.0)
    return ReacherVerifier()


def test_verify_success(monkeypatch):
    rv._breaker.reset()
    v = _verifier(monkeypatch)

    async def fake_check(self, email):
        return _payload("safe")
    monkeypatch.setattr(rv.ReacherClient, "check", fake_check)

    r = asyncio.run(v.verify("a@b.com"))
    assert r.status == cascade.VALID and r.email == "a@b.com"


def test_verify_error_degrades_to_unknown(monkeypatch):
    rv._breaker.reset()
    v = _verifier(monkeypatch)

    async def boom(self, email):
        raise RuntimeError("connection refused")
    monkeypatch.setattr(rv.ReacherClient, "check", boom)

    r = asyncio.run(v.verify("a@b.com"))
    assert r.status == cascade.UNKNOWN and r.detail == "error"


def test_verify_timeout_marked_timeout(monkeypatch):
    rv._breaker.reset()
    v = _verifier(monkeypatch)

    class FakeTimeout(Exception):
        pass
    FakeTimeout.__name__ = "ReadTimeout"

    async def slow(self, email):
        raise FakeTimeout()
    monkeypatch.setattr(rv.ReacherClient, "check", slow)

    r = asyncio.run(v.verify("a@b.com"))
    assert r.status == cascade.UNKNOWN and r.detail == "timeout"


# ── circuit breaker ──────────────────────────────────────────────────────────

def test_breaker_opens_after_threshold():
    b = _CircuitBreaker(threshold=3, cooldown=60)
    assert not b.is_open()
    b.record_failure(); b.record_failure()
    assert not b.is_open()            # below threshold
    b.record_failure()
    assert b.is_open()               # tripped


def test_breaker_resets_on_success():
    b = _CircuitBreaker(threshold=2, cooldown=60)
    b.record_failure(); b.record_failure()
    assert b.is_open()
    b.reset()
    assert not b.is_open()


def test_breaker_open_makes_verifier_unavailable(monkeypatch):
    rv._breaker.reset()
    v = _verifier(monkeypatch)
    assert v.is_available() is True
    for _ in range(rv._breaker.threshold):
        rv._breaker.record_failure()
    assert v.is_available() is False  # breaker open → unavailable
    # ...and verify() short-circuits without calling the client.
    r = asyncio.run(v.verify("a@b.com"))
    assert r.status == cascade.UNKNOWN and r.detail == "breaker_open"
    rv._breaker.reset()


# ── SSRF allowlist ───────────────────────────────────────────────────────────

@pytest.mark.parametrize("url", [
    "http://reacher:8080", "https://api.reacher.email", "http://localhost:8080",
])
def test_allowlist_accepts_valid(url):
    assert validate_reacher_url(url) is True


@pytest.mark.parametrize("url", [
    "", "file:///etc/passwd", "gopher://x", "http://169.254.169.254/latest/meta-data",
    "http://metadata.google.internal/x", "ftp://host/x", "not a url",
])
def test_allowlist_rejects_bad(url):
    assert validate_reacher_url(url) is False


def test_offallowlist_url_disables_verifier(monkeypatch):
    rv._breaker.reset()
    v = _verifier(monkeypatch, url="http://169.254.169.254/latest")
    assert v.is_available() is False


# ── flag gating + URL is global-only ─────────────────────────────────────────

def test_disabled_flag_makes_unavailable(monkeypatch):
    rv._breaker.reset()
    v = _verifier(monkeypatch, enabled=False)
    assert v.is_available() is False


def test_url_is_global_only_not_workspace(monkeypatch):
    """A per-workspace REACHER_URL must be ignored; the API key may be ws-scoped."""
    seen = {}

    def fake_get_secret(workspace_id, key, default=""):
        seen[key] = workspace_id
        return "http://evil.attacker.internal" if key == rv.ENV_URL else "ws-key"
    monkeypatch.setattr("apps.api.services.workspace.secrets.get_secret", fake_get_secret)
    monkeypatch.setattr(rv, "_resolve_global",
                        lambda key, default="": "http://reacher:8080" if key == rv.ENV_URL else default)

    v = ReacherVerifier(workspace_id="ws-123")
    # URL resolves from GLOBAL, never the workspace secret.
    assert v._url() == "http://reacher:8080"
    assert rv.ENV_URL not in seen  # get_secret was never consulted for the URL
    # The API key DOES resolve per-workspace.
    assert v._api_key() == "ws-key"
    assert seen.get(rv.ENV_API_KEY) == "ws-123"
