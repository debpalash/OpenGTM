"""Offline tests for the DDG result cache + per-host adaptive backoff.

All network is mocked — we never hit real DuckDuckGo. Covers the spec test
plan: cache hit dedup, key isolation, TTL (incl. shorter empty-TTL), LRU bound,
backoff trip/curve/cap, backoff routing to keyed fallback, success reset,
empty != block, flags-off legacy behaviour, shape invariance, thread safety.
"""
import sys
import threading
import types

import pytest

import apps.api.services.leadgen.search_cache as sc
import apps.api.services.leadgen.search_engines as se
from apps.api.services.leadgen import proxy_client


SHAPE_KEYS = {"title", "href", "body"}
_DDG_ROW = {"title": "ddg", "href": "https://ddg.example/x", "body": "from ddg"}


# ── Fakes / fixtures ────────────────────────────────────────────────────────
class _CountingDDGS:
    """ddgs.DDGS stand-in that counts upstream calls and is configurable."""

    calls = 0
    behavior = "results"  # "results" | "empty" | "raise"
    exc = RuntimeError("boom")

    def __init__(self, *a, **k):
        pass

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def text(self, query, **kwargs):
        _CountingDDGS.calls += 1
        if _CountingDDGS.behavior == "raise":
            raise _CountingDDGS.exc
        if _CountingDDGS.behavior == "empty":
            return []
        return [dict(_DDG_ROW)]

    news = text


@pytest.fixture(autouse=True)
def _clean(monkeypatch):
    """Fresh cache/backoff + keyless, flag-default slate for every test."""
    for var in (
        "SERPAPI_KEY", "BING_SEARCH_KEY", "GOOGLE_CSE_ID", "GOOGLE_CSE_KEY",
        "BRAVE_SEARCH_KEY", "SEARCH_FALLBACK_ORDER", "SEARCH_ATTEMPT_ORDER",
        "SEARCH_BACKENDS", "SEARCH_CACHE_ENABLED", "SEARCH_BACKOFF_ENABLED",
        "SEARCH_CACHE_TTL", "SEARCH_CACHE_EMPTY_TTL", "SEARCH_CACHE_MAX_ENTRIES",
        "SEARCH_BACKOFF_BASE", "SEARCH_BACKOFF_CAP",
    ):
        monkeypatch.delenv(var, raising=False)
    _CountingDDGS.calls = 0
    _CountingDDGS.behavior = "results"
    _CountingDDGS.exc = RuntimeError("boom")
    sc.reset_all()
    yield
    sc.reset_all()


@pytest.fixture
def patch_ddgs(monkeypatch):
    """Resolve `from ddgs import DDGS` to _CountingDDGS; direct-only, no proxy."""
    fake_mod = types.ModuleType("ddgs")
    fake_mod.DDGS = _CountingDDGS
    monkeypatch.setitem(sys.modules, "ddgs", fake_mod)
    monkeypatch.setattr(proxy_client, "get_proxy", lambda *a, **k: None)
    monkeypatch.setattr(proxy_client, "SEARCH_ATTEMPT_ORDER", ["direct"])
    yield


@pytest.fixture
def fake_clock(monkeypatch):
    """Controllable monotonic clock for deterministic TTL/backoff tests."""
    state = {"t": 1000.0}
    monkeypatch.setattr(sc.time, "monotonic", lambda: state["t"])

    def advance(dt):
        state["t"] += dt

    return advance


# ── 1. Cache hit dedup ──────────────────────────────────────────────────────
def test_cache_hit_one_upstream_call(patch_ddgs):
    with proxy_client.get_ddgs() as ddgs:
        r1 = ddgs.text("acme corp", max_results=5)
        r2 = ddgs.text("acme corp", max_results=5)
    assert r1 == r2 == [_DDG_ROW]
    assert _CountingDDGS.calls == 1  # second served from cache, zero network


# ── 2. Key isolation ────────────────────────────────────────────────────────
def test_key_isolation_max_results_and_query(patch_ddgs):
    with proxy_client.get_ddgs() as ddgs:
        ddgs.text("acme", max_results=5)
        ddgs.text("acme", max_results=15)   # different max_results
        ddgs.text("other", max_results=5)   # different query
    assert _CountingDDGS.calls == 3


def test_normalized_query_collapses_to_same_key(patch_ddgs):
    with proxy_client.get_ddgs() as ddgs:
        ddgs.text("Acme   Corp", max_results=5)
        ddgs.text("  acme corp ", max_results=5)  # case/space-normalized -> hit
    assert _CountingDDGS.calls == 1


def test_tenant_salt_isolates_workspaces():
    from apps.api.core import tenancy

    var = tenancy.current_workspace_var
    k_a = sc.make_cache_key("text", "secret q", 5, "")   # no tenant -> shared
    tok = var.set("ws-1")
    try:
        k_b = sc.make_cache_key("text", "secret q", 5, "")
        var.set("ws-2")
        k_c = sc.make_cache_key("text", "secret q", 5, "")
    finally:
        var.reset(tok)
    assert k_a != k_b and k_b != k_c and k_a != k_c  # each tenant distinct


# ── 3. TTL (non-empty long, empty short) ────────────────────────────────────
def test_non_empty_ttl_expiry(fake_clock):
    sc.put_cached("k", [_DDG_ROW])
    assert sc.get_cached("k") == [_DDG_ROW]
    fake_clock(599)
    assert sc.get_cached("k") == [_DDG_ROW]   # within 600s TTL
    fake_clock(2)
    assert sc.get_cached("k") is None         # past 600s


def test_empty_uses_shorter_ttl(fake_clock):
    sc.put_cached("empty", [], empty=True)
    sc.put_cached("full", [_DDG_ROW])
    fake_clock(61)                            # past 60s empty-TTL, within 600s
    assert sc.get_cached("empty") is None     # empty expired fast
    assert sc.get_cached("full") == [_DDG_ROW]  # non-empty still alive


def test_empty_result_cached_with_empty_ttl_via_run(patch_ddgs):
    _CountingDDGS.behavior = "empty"
    with proxy_client.get_ddgs() as ddgs:
        assert ddgs.text("nada", max_results=5) == []
        assert ddgs.text("nada", max_results=5) == []
    assert _CountingDDGS.calls == 1  # empty result is cached (short TTL)


# ── 4. LRU bound ────────────────────────────────────────────────────────────
def test_lru_bound_evicts_oldest(monkeypatch):
    monkeypatch.setenv("SEARCH_CACHE_MAX_ENTRIES", "5")
    for i in range(20):
        sc.put_cached(f"k{i}", [_DDG_ROW])
    assert sc.cache_size() == 5
    assert sc.get_cached("k0") is None    # oldest evicted
    assert sc.get_cached("k19") == [_DDG_ROW]  # newest retained


# ── 5. Backoff trip + curve + cap ───────────────────────────────────────────
def test_backoff_curve_and_cap(monkeypatch, fake_clock):
    monkeypatch.setenv("SEARCH_BACKOFF_BASE", "10")
    monkeypatch.setenv("SEARCH_BACKOFF_CAP", "100")
    monkeypatch.setattr(sc.random, "uniform", lambda a, b: 0.0)  # no jitter

    exc = RuntimeError("429 ratelimit")
    # block 1 -> 10, block 2 -> 20, block 3 -> 40, block 4 -> 80, block 5 -> 100(cap)
    expected = [10, 20, 40, 80, 100]
    for n, want in enumerate(expected, start=1):
        assert sc.record_block("ddgs", exc) is True
        assert sc.host_blocked("ddgs") is True
        assert sc._backoff.consecutive("ddgs") == n
        assert sc._backoff.time_remaining("ddgs") == pytest.approx(want)
        fake_clock(want + 1)  # let it expire so the next block starts fresh-timed
    assert sc.host_blocked("ddgs") is False  # last one expired after advance


def test_backoff_jitter_never_exceeds_cap(monkeypatch):
    monkeypatch.setenv("SEARCH_BACKOFF_BASE", "900")
    monkeypatch.setenv("SEARCH_BACKOFF_CAP", "900")
    for _ in range(5):
        sc.record_block("ddgs", RuntimeError("blocked"))
        assert sc._backoff.time_remaining("ddgs") <= 900.0 + 1e-6


# ── 6. Backoff routing -> keyed fallback ────────────────────────────────────
def test_backed_off_host_routes_to_keyed(patch_ddgs, monkeypatch):
    # Put ddgs in backoff, then ensure _run skips DDG entirely and uses keyed.
    sc.record_block("ddgs", RuntimeError("429"))
    assert sc.host_blocked("ddgs")

    def boom_text(self, *a, **k):
        raise AssertionError("DDG must be skipped while backed off")

    monkeypatch.setattr(_CountingDDGS, "text", boom_text)
    monkeypatch.setenv("SERPAPI_KEY", "k")
    monkeypatch.setattr(
        se, "_http_get_json",
        lambda *a, **k: {"organic_results": [
            {"title": "A", "link": "https://a.example", "snippet": "s"}]},
    )
    with proxy_client.get_ddgs() as ddgs:
        results = ddgs.text("q", max_results=5)
    assert results == [{"title": "A", "href": "https://a.example", "body": "s"}]
    assert _CountingDDGS.calls == 0


def test_search_fallback_skips_backed_off_engine(monkeypatch):
    monkeypatch.setenv("SERPAPI_KEY", "k")
    monkeypatch.setenv("BRAVE_SEARCH_KEY", "k")
    monkeypatch.setenv("SEARCH_FALLBACK_ORDER", "serpapi,brave")
    sc.record_block("serpapi", RuntimeError("429"))  # serpapi cooling down

    seen = []

    def fake_get(url, params, headers=None):
        seen.append(url)
        return {"web": {"results": [
            {"title": "B", "url": "https://b.example", "description": "x"}]}}

    monkeypatch.setattr(se, "_http_get_json", fake_get)
    results = se.search_fallback("q", max_results=5)
    assert results[0]["href"] == "https://b.example"     # served by brave
    assert all("serpapi" not in u for u in seen)          # serpapi skipped


# ── 7. Success reset ────────────────────────────────────────────────────────
def test_success_resets_backoff():
    sc.record_block("ddgs", RuntimeError("ratelimit"))
    sc.record_block("ddgs", RuntimeError("ratelimit"))
    assert sc._backoff.consecutive("ddgs") == 2
    sc.record_success("ddgs")
    assert sc._backoff.consecutive("ddgs") == 0
    assert sc.host_blocked("ddgs") is False


def test_success_after_blocks_via_run(patch_ddgs):
    # First call rate-limited -> backoff recorded; keyed fallback empty -> [].
    _CountingDDGS.behavior = "raise"
    _CountingDDGS.exc = RuntimeError("429 Too Many Requests")
    with proxy_client.get_ddgs() as ddgs:
        ddgs.text("q1", max_results=5)
    assert sc._backoff.consecutive("ddgs") >= 1


# ── 8. Empty != block ───────────────────────────────────────────────────────
def test_empty_result_does_not_trip_backoff(patch_ddgs):
    _CountingDDGS.behavior = "empty"
    with proxy_client.get_ddgs() as ddgs:
        ddgs.text("q", max_results=5)
    assert sc.host_blocked("ddgs") is False
    assert sc._backoff.consecutive("ddgs") == 0


def test_non_block_exception_does_not_trip_backoff():
    assert sc.record_block("ddgs", ValueError("bad argument")) is False
    assert sc.host_blocked("ddgs") is False


def test_block_signal_classification():
    for msg in ("429", "rate limit hit", "RatelimitException", "captcha", "403 blocked"):
        assert sc.is_block_signal(RuntimeError(msg)) is True
    assert sc.is_block_signal(ValueError("parse error")) is False
    assert sc.is_block_signal(None) is False


# ── 9. Flags off == legacy ──────────────────────────────────────────────────
def test_flags_off_no_cache_no_backoff(patch_ddgs, monkeypatch):
    monkeypatch.setenv("SEARCH_CACHE_ENABLED", "false")
    monkeypatch.setenv("SEARCH_BACKOFF_ENABLED", "false")
    with proxy_client.get_ddgs() as ddgs:
        ddgs.text("q", max_results=5)
        ddgs.text("q", max_results=5)
    assert _CountingDDGS.calls == 2          # no caching -> two upstream calls
    # Even after a block signal, host is never reported blocked.
    sc.record_block("ddgs", RuntimeError("429"))
    assert sc.host_blocked("ddgs") is False


# ── 10. Shape invariance ────────────────────────────────────────────────────
def test_cached_value_shape_identical(patch_ddgs):
    with proxy_client.get_ddgs() as ddgs:
        fresh = ddgs.text("q", max_results=5)
        cached = ddgs.text("q", max_results=5)
    assert fresh == cached
    for r in cached:
        assert set(r.keys()) == SHAPE_KEYS


def test_cache_returns_copy_not_aliased(patch_ddgs):
    with proxy_client.get_ddgs() as ddgs:
        first = ddgs.text("q", max_results=5)
        first.append({"title": "x", "href": "y", "body": "z"})  # mutate caller's list
        second = ddgs.text("q", max_results=5)
    assert second == [_DDG_ROW]  # cache not corrupted by caller mutation


# ── 11. Thread-safety smoke ─────────────────────────────────────────────────
def test_thread_safety_smoke(monkeypatch):
    monkeypatch.setenv("SEARCH_CACHE_MAX_ENTRIES", "50")
    errors = []

    def worker(base):
        try:
            for i in range(500):
                sc.put_cached(f"k{base}-{i}", [_DDG_ROW])
                sc.get_cached(f"k{base}-{i}")
                sc.record_block("ddgs", RuntimeError("429"))
                sc.host_blocked("ddgs")
        except Exception as e:  # noqa: BLE001
            errors.append(e)

    threads = [threading.Thread(target=worker, args=(b,)) for b in range(8)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert not errors
    assert sc.cache_size() <= 50  # bound held under concurrency
