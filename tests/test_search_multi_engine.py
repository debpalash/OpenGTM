"""Offline tests for the multi-engine search fallback.

All network is mocked. We cover:
  * DDG returns results  -> no fallback to keyed engines
  * DDG returns empty     -> falls back to a configured keyed engine
  * DDG raises            -> falls back to a configured keyed engine
  * engine ORDER is respected
  * no keys configured    -> DDG-only, no crash
  * result-shape parity across every keyed adapter
"""
import importlib

import pytest

import apps.api.services.leadgen.search_engines as se
from apps.api.services.leadgen import proxy_client


# ── Helpers ───────────────────────────────────────────────────────────────
SHAPE_KEYS = {"title", "href", "body"}


def _assert_shape(results):
    assert isinstance(results, list)
    for r in results:
        assert isinstance(r, dict)
        assert set(r.keys()) == SHAPE_KEYS
        assert isinstance(r["title"], str)
        assert isinstance(r["href"], str)
        assert isinstance(r["body"], str)


class _FakeDDGS:
    """Stand-in for ddgs.DDGS used to control what the keyless layer returns."""

    behavior = "empty"  # "empty" | "results" | "raise"

    def __init__(self, *a, **k):
        pass

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def text(self, query, **kwargs):
        if _FakeDDGS.behavior == "raise":
            raise RuntimeError("rate limited")
        if _FakeDDGS.behavior == "results":
            return [{"title": "ddg", "href": "https://ddg.example/x", "body": "from ddg"}]
        return []


@pytest.fixture(autouse=True)
def _clear_env(monkeypatch):
    """Start every test from a clean, keyless slate."""
    for var in (
        "SERPAPI_KEY", "BING_SEARCH_KEY", "GOOGLE_CSE_ID",
        "GOOGLE_CSE_KEY", "BRAVE_SEARCH_KEY", "SEARCH_FALLBACK_ORDER",
        "SEARCH_ATTEMPT_ORDER", "SEARCH_BACKENDS",
    ):
        monkeypatch.delenv(var, raising=False)
    _FakeDDGS.behavior = "empty"
    yield


@pytest.fixture
def patch_ddgs(monkeypatch):
    """Make `from ddgs import DDGS` inside proxy_client resolve to _FakeDDGS."""
    import sys
    import types

    fake_mod = types.ModuleType("ddgs")
    fake_mod.DDGS = _FakeDDGS
    monkeypatch.setitem(sys.modules, "ddgs", fake_mod)
    # proxy_client picks the proxy via get_proxy(); force direct-only, no proxy.
    monkeypatch.setattr(proxy_client, "get_proxy", lambda *a, **k: None)
    monkeypatch.setattr(proxy_client, "SEARCH_ATTEMPT_ORDER", ["direct"])
    yield


# ── search_engines unit-level ──────────────────────────────────────────────
def test_no_keys_means_no_configured_engines(monkeypatch):
    assert se.configured_engines() == []
    assert se.search_fallback("anything", max_results=5) == []


def test_shape_parity_across_all_adapters(monkeypatch):
    """Each adapter, given its provider's JSON, yields the same {title,href,body}."""
    payloads = {
        "serpapi": (
            {"SERPAPI_KEY": "k"},
            {"organic_results": [{"title": "A", "link": "https://a.example", "snippet": "sa"}]},
        ),
        "bing": (
            {"BING_SEARCH_KEY": "k"},
            {"webPages": {"value": [{"name": "B", "url": "https://b.example", "snippet": "sb"}]}},
        ),
        "google_cse": (
            {"GOOGLE_CSE_ID": "id", "GOOGLE_CSE_KEY": "k"},
            {"items": [{"title": "C", "link": "https://c.example", "snippet": "sc"}]},
        ),
        "brave": (
            {"BRAVE_SEARCH_KEY": "k"},
            {"web": {"results": [{"title": "D", "url": "https://d.example", "description": "sd"}]}},
        ),
    }
    for engine, (env, json_body) in payloads.items():
        for var in ("SERPAPI_KEY", "BING_SEARCH_KEY", "GOOGLE_CSE_ID",
                    "GOOGLE_CSE_KEY", "BRAVE_SEARCH_KEY"):
            monkeypatch.delenv(var, raising=False)
        for k, v in env.items():
            monkeypatch.setenv(k, v)
        monkeypatch.setattr(se, "_http_get_json", lambda *a, **k: json_body)
        assert se.configured_engines() == [engine]
        results = se.search_fallback("q", max_results=5)
        assert len(results) == 1, engine
        _assert_shape(results)


def test_engine_order_respected(monkeypatch):
    monkeypatch.setenv("SERPAPI_KEY", "k")
    monkeypatch.setenv("BRAVE_SEARCH_KEY", "k")
    # Brave listed first -> Brave should be queried first.
    monkeypatch.setenv("SEARCH_FALLBACK_ORDER", "brave,serpapi")
    assert se.configured_engines() == ["brave", "serpapi"]

    calls = []

    def fake_get(url, params, headers=None):
        calls.append(url)
        if "brave" in url:
            return {"web": {"results": [{"title": "B", "url": "https://b.example", "description": "x"}]}}
        return {"organic_results": [{"title": "S", "link": "https://s.example", "snippet": "y"}]}

    monkeypatch.setattr(se, "_http_get_json", fake_get)
    results = se.search_fallback("q", max_results=5)
    assert results[0]["href"] == "https://b.example"
    assert "brave" in calls[0]  # brave tried first


def test_order_skips_empty_engine(monkeypatch):
    """First configured engine returns empty -> chain continues to the next."""
    monkeypatch.setenv("SERPAPI_KEY", "k")
    monkeypatch.setenv("BRAVE_SEARCH_KEY", "k")
    monkeypatch.setenv("SEARCH_FALLBACK_ORDER", "serpapi,brave")

    def fake_get(url, params, headers=None):
        if "serpapi" in url:
            return {"organic_results": []}  # empty
        return {"web": {"results": [{"title": "B", "url": "https://b.example", "description": "x"}]}}

    monkeypatch.setattr(se, "_http_get_json", fake_get)
    results = se.search_fallback("q", max_results=5)
    assert len(results) == 1
    assert results[0]["href"] == "https://b.example"


# ── End-to-end through get_ddgs() (the layer all callers use) ───────────────
def test_no_fallback_when_ddg_has_results(patch_ddgs, monkeypatch):
    _FakeDDGS.behavior = "results"
    monkeypatch.setenv("SERPAPI_KEY", "k")

    def boom(*a, **k):
        raise AssertionError("keyed fallback must not run when DDG has results")

    monkeypatch.setattr(se, "search_fallback", boom)

    with proxy_client.get_ddgs() as ddgs:
        results = ddgs.text("q", max_results=5)
    assert results == [{"title": "ddg", "href": "https://ddg.example/x", "body": "from ddg"}]


def test_fallback_when_ddg_empty(patch_ddgs, monkeypatch):
    _FakeDDGS.behavior = "empty"
    monkeypatch.setenv("SERPAPI_KEY", "k")
    monkeypatch.setattr(
        se, "_http_get_json",
        lambda *a, **k: {"organic_results": [{"title": "A", "link": "https://a.example", "snippet": "s"}]},
    )
    with proxy_client.get_ddgs() as ddgs:
        results = ddgs.text("q", max_results=5)
    assert len(results) == 1
    _assert_shape(results)
    assert results[0]["href"] == "https://a.example"


def test_fallback_when_ddg_raises(patch_ddgs, monkeypatch):
    _FakeDDGS.behavior = "raise"
    monkeypatch.setenv("BRAVE_SEARCH_KEY", "k")
    monkeypatch.setattr(
        se, "_http_get_json",
        lambda *a, **k: {"web": {"results": [{"title": "D", "url": "https://d.example", "description": "s"}]}},
    )
    with proxy_client.get_ddgs() as ddgs:
        results = ddgs.text("q", max_results=5)
    assert len(results) == 1
    _assert_shape(results)
    assert results[0]["href"] == "https://d.example"


def test_no_keys_ddg_only_no_crash(patch_ddgs, monkeypatch):
    """DDG empty + no keys -> [] with no exception (== today's behavior)."""
    _FakeDDGS.behavior = "empty"
    with proxy_client.get_ddgs() as ddgs:
        results = ddgs.text("q", max_results=5)
    assert results == []


def test_news_has_no_keyed_fallback(patch_ddgs, monkeypatch):
    """Only text() falls back to keyed engines; news() stays keyless."""
    _FakeDDGS.behavior = "empty"
    monkeypatch.setenv("SERPAPI_KEY", "k")

    def boom(*a, **k):
        raise AssertionError("news() must not use keyed fallback")

    monkeypatch.setattr(se, "search_fallback", boom)
    with proxy_client.get_ddgs() as ddgs:
        results = ddgs.news("q", max_results=5)
    assert results == []
