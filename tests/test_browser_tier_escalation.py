"""Offline tests for browser-tier escalation in the lead-sourcing fetch path.

No real network calls — every tier is mocked. Covers:
  * challenge / JS-empty detection markers + tiny-body heuristic
  * Tier2 (curl_cffi) used when available, with graceful fallbacks
  * auto-escalation Tier1/2 -> Tier3 on a challenge marker
  * graceful degrade when the browser libs/binary are absent (no crash; the
    lower-tier result is preserved)
  * the successful tier is reported on the result (tier_used) and logged

Run:
    PYTHONPATH=. uv run --group dev python -m pytest tests/test_browser_tier_escalation.py -q
"""

import asyncio
import logging

import pytest

from apps.api.services.leadgen import http as H
from apps.api.services.leadgen.http import (
    FetchResult,
    StealthClient,
    TINY_BODY_THRESHOLD,
    _is_challenge,
)


# ── test isolation ───────────────────────────────────────────────────

@pytest.fixture(autouse=True)
def _reset_logging_state():
    """Keep the caplog-based logging assertions order-independent.

    In the full suite, importing the app elsewhere can mutate global logging
    state (logfire config / instrumentation can leave a global ``logging.disable``
    in effect or flip the ``leadgen.http`` logger's ``propagate``/``disabled``),
    which would silently drop records from caplog. Reset that around each test and
    clear the module-level warn-once cache so state can't leak in from prior tests.
    """
    logging.disable(logging.NOTSET)
    lg = logging.getLogger("leadgen.http")
    prev = (lg.disabled, lg.propagate, lg.level)
    lg.disabled = False
    lg.propagate = True
    H._warned_missing.clear()
    try:
        yield
    finally:
        lg.disabled, lg.propagate, lg.level = prev


# ── helpers ──────────────────────────────────────────────────────────

def _client():
    """A client with no proxies / inert rate limiter (works on dicts, no DB)."""
    c = StealthClient()
    # Defensive: force "no proxy" so we don't depend on a proxy dir.
    c.proxy_pool.__len__ = lambda: 0  # type: ignore[method-assign]
    return c


# ── challenge detection ──────────────────────────────────────────────

def test_blocked_status_is_challenge():
    assert _is_challenge(403, "anything")
    assert _is_challenge(429, "")
    assert _is_challenge(503, "")


def test_cloudflare_marker_is_challenge():
    assert _is_challenge(200, "<html>Just a moment...</html>")
    assert _is_challenge(200, "...challenge-platform...")


def test_enable_javascript_marker_is_challenge():
    assert _is_challenge(200, "<html><body>Please enable JavaScript to continue</body></html>")
    assert _is_challenge(200, "<noscript>turn on js</noscript>")


def test_tiny_body_is_challenge():
    assert _is_challenge(200, "<html></html>")  # well under threshold


def test_real_page_is_not_challenge():
    body = "<html><body>" + ("x" * (TINY_BODY_THRESHOLD + 100)) + "</body></html>"
    assert not _is_challenge(200, body)


def test_empty_body_is_not_flagged_as_tiny():
    # A truly empty body shouldn't trip the tiny-body branch (len==0 guard).
    assert not _is_challenge(200, "")


# ── Tier 2 (curl_cffi) selection + fallbacks ─────────────────────────

def test_curl_cffi_tier2_used_when_available(monkeypatch):
    c = _client()

    async def fake_cffi(self, url, proxy, timeout):
        r = FetchResult(url=url, tier_used=2)
        r.status_code = 200
        r.text = "<html>" + "ok " * 300 + "</html>"
        return r

    # If curl_cffi path returns a good result, the lower fallbacks must not run.
    async def boom(self, url, proxy, timeout):  # pragma: no cover - must not run
        raise AssertionError("fallback should not be reached")

    monkeypatch.setattr(StealthClient, "_fetch_curl_cffi", fake_cffi)
    monkeypatch.setattr(StealthClient, "_fetch_plain", boom)

    res = asyncio.run(c.fetch("https://example.com", use_proxy=False))
    assert res.ok
    assert res.tier_used == 2


def test_falls_back_to_plain_when_curl_cffi_absent(monkeypatch):
    c = _client()

    async def cffi_absent(self, url, proxy, timeout):
        return None  # simulates curl_cffi ImportError -> None

    async def no_stealth(self, url, proxy, timeout):
        return None  # stealth_requests absent

    async def fake_plain(self, url, proxy, timeout):
        r = FetchResult(url=url, tier_used=1)
        r.status_code = 200
        r.text = "<html>" + "ok " * 300 + "</html>"
        return r

    monkeypatch.setattr(StealthClient, "_fetch_curl_cffi", cffi_absent)
    monkeypatch.setattr(StealthClient, "_fetch_stealth_requests", no_stealth)
    monkeypatch.setattr(StealthClient, "_fetch_plain", fake_plain)

    res = asyncio.run(c.fetch("https://example.com", use_proxy=False))
    assert res.ok
    assert res.tier_used == 1  # plain HTTP tier


# ── auto-escalation Tier1/2 -> Tier3 ─────────────────────────────────

def test_escalates_to_browser_on_challenge(monkeypatch):
    c = _client()

    async def fake_http(self, url, proxy, timeout):
        r = FetchResult(url=url, tier_used=2)
        r.status_code = 403  # blocked -> challenge
        r.text = "Just a moment..."
        return r

    async def fake_browser(self, url, proxy, timeout):
        r = FetchResult(url=url, tier_used=3)
        r.status_code = 200
        r.text = "<html>" + "real content " * 100 + "</html>"
        return r

    monkeypatch.setattr(StealthClient, "_fetch_http", fake_http)
    monkeypatch.setattr(StealthClient, "_fetch_tier3", fake_browser)

    res = asyncio.run(c.fetch("https://blocked.example", use_proxy=False))
    assert res.ok
    assert res.tier_used == 3  # escalated to browser


def test_no_escalation_when_http_ok(monkeypatch):
    c = _client()
    calls = {"browser": 0}

    async def fake_http(self, url, proxy, timeout):
        r = FetchResult(url=url, tier_used=2)
        r.status_code = 200
        r.text = "<html>" + "good " * 300 + "</html>"
        return r

    async def fake_browser(self, url, proxy, timeout):  # pragma: no cover
        calls["browser"] += 1
        return FetchResult(url=url, tier_used=3)

    monkeypatch.setattr(StealthClient, "_fetch_http", fake_http)
    monkeypatch.setattr(StealthClient, "_fetch_tier3", fake_browser)

    res = asyncio.run(c.fetch("https://ok.example", use_proxy=False))
    assert res.ok
    assert res.tier_used == 2
    assert calls["browser"] == 0  # browser never invoked


def test_escalates_on_tiny_body(monkeypatch):
    c = _client()

    async def fake_http(self, url, proxy, timeout):
        r = FetchResult(url=url, tier_used=2)
        r.status_code = 200
        r.text = "<html></html>"  # JS shell, tiny
        return r

    async def fake_browser(self, url, proxy, timeout):
        r = FetchResult(url=url, tier_used=3)
        r.status_code = 200
        r.text = "<html>" + "rendered " * 200 + "</html>"
        return r

    monkeypatch.setattr(StealthClient, "_fetch_http", fake_http)
    monkeypatch.setattr(StealthClient, "_fetch_tier3", fake_browser)

    res = asyncio.run(c.fetch("https://js-shell.example", use_proxy=False))
    assert res.tier_used == 3
    assert "rendered" in res.text


# ── graceful degrade when browser absent ─────────────────────────────

def test_graceful_degrade_when_browser_missing(monkeypatch):
    """Challenge detected, but no browser installed -> keep the HTTP result, no crash."""
    c = _client()

    async def fake_http(self, url, proxy, timeout):
        r = FetchResult(url=url, tier_used=2)
        r.status_code = 200
        # Tiny body triggers escalation, but it's still *some* content.
        r.text = "<html><p>tiny</p></html>"
        return r

    # Simulate "no browser library available".
    monkeypatch.setattr(StealthClient, "_load_async_playwright", staticmethod(lambda: None))
    monkeypatch.setattr(StealthClient, "_fetch_http", fake_http)

    res = asyncio.run(c.fetch("https://nojs.example", use_proxy=False))
    # No crash; we keep the lower-tier (HTTP) body since browser produced nothing.
    assert res.text == "<html><p>tiny</p></html>"
    assert res.tier_used == 2


def test_tier3_returns_error_when_no_browser(monkeypatch):
    c = _client()
    H._warned_missing.clear()
    monkeypatch.setattr(StealthClient, "_load_async_playwright", staticmethod(lambda: None))
    res = asyncio.run(c._fetch_tier3("https://x.example", None, 10))
    assert not res.ok
    assert res.error == "browser not installed"
    assert res.tier_used == 3


def test_warn_once_only_logs_once(caplog):
    H._warned_missing.clear()
    with caplog.at_level(logging.WARNING, logger="leadgen.http"):
        H._warn_once("k1", "first message")
        H._warn_once("k1", "should be suppressed")
        H._warn_once("k2", "second key")
    msgs = [r.message for r in caplog.records]
    assert "first message" in msgs
    assert "should be suppressed" not in msgs
    assert "second key" in msgs


# ── successful-tier logging ──────────────────────────────────────────

def test_successful_tier_is_logged(monkeypatch, caplog):
    c = _client()

    async def fake_http(self, url, proxy, timeout):
        r = FetchResult(url=url, tier_used=2)
        r.status_code = 200
        r.text = "<html>" + "ok " * 300 + "</html>"
        return r

    monkeypatch.setattr(StealthClient, "_fetch_http", fake_http)

    with caplog.at_level(logging.DEBUG, logger="leadgen.http"):
        res = asyncio.run(c.fetch("https://ok.example", use_proxy=False))
    assert res.tier_used == 2
    assert any("succeeded at tier 2" in r.message for r in caplog.records)


def test_escalation_is_logged(monkeypatch, caplog):
    c = _client()

    async def fake_http(self, url, proxy, timeout):
        r = FetchResult(url=url, tier_used=2)
        r.status_code = 403
        r.text = "Just a moment..."
        return r

    async def fake_browser(self, url, proxy, timeout):
        r = FetchResult(url=url, tier_used=3)
        r.status_code = 200
        r.text = "<html>" + "content " * 200 + "</html>"
        return r

    monkeypatch.setattr(StealthClient, "_fetch_http", fake_http)
    monkeypatch.setattr(StealthClient, "_fetch_tier3", fake_browser)

    with caplog.at_level(logging.INFO, logger="leadgen.http"):
        asyncio.run(c.fetch("https://blocked.example", use_proxy=False))
    assert any("escalating to browser tier 3" in r.message for r in caplog.records)


# ── deprecated alias still works ─────────────────────────────────────

def test_fetch_tier2_alias_delegates_to_http(monkeypatch):
    c = _client()

    async def fake_http(self, url, proxy, timeout):
        return FetchResult(url=url, tier_used=2, status_code=200, text="aliased")

    monkeypatch.setattr(StealthClient, "_fetch_http", fake_http)
    res = asyncio.run(c._fetch_tier2("https://x.example", None, 5))
    assert res.text == "aliased"
