"""A keyless search backend that just said 429/403/202 or timed out is left alone while it cools down."""

from __future__ import annotations

import pytest

from browser_service.scrapers import backend_health
from browser_service.scrapers.backend_health import BackendHealth


class Clock:
    def __init__(self):
        self.t = 1000.0

    def __call__(self):
        return self.t


def test_a_429_cools_the_backend_and_it_comes_back_later():
    clock = Clock()
    h = BackendHealth(clock)
    h.record_status("brave", 429)
    assert h.blocked("brave") and not h.blocked("mojeek")
    clock.t += backend_health.BLOCK_COOLDOWN_S + 1
    assert not h.blocked("brave")


@pytest.mark.parametrize("status", [202, 403, 429, 503])
def test_block_statuses_use_the_long_cooldown(status):
    clock = Clock()
    h = BackendHealth(clock)
    h.record_status("x", status)
    assert h.seconds_left("x") == backend_health.BLOCK_COOLDOWN_S


def test_a_timeout_uses_the_short_cooldown_and_backs_off_while_it_repeats():
    clock = Clock()
    h = BackendHealth(clock)
    h.record_failure("duckduckgo")
    assert h.seconds_left("duckduckgo") == backend_health.FAIL_COOLDOWN_S
    clock.t += 61
    h.record_failure("duckduckgo")
    assert h.seconds_left("duckduckgo") == 2 * backend_health.FAIL_COOLDOWN_S
    for _ in range(10):
        clock.t += 10_000
        h.record_failure("duckduckgo")
    assert h.seconds_left("duckduckgo") == backend_health.FAIL_COOLDOWN_MAX_S


def test_a_200_clears_the_streak():
    clock = Clock()
    h = BackendHealth(clock)
    h.record_status("brave", 429)
    h.record_status("brave", 200)
    assert not h.blocked("brave")
    h.record_status("brave", 429)
    assert h.seconds_left("brave") == backend_health.BLOCK_COOLDOWN_S  # the streak restarted


def test_allowed_and_all_blocked():
    h = BackendHealth(Clock())
    h.record_status("brave", 429)
    assert h.allowed(["brave", "bing"]) == ["bing"]
    assert not h.all_blocked(["brave", "bing"])
    h.record_status("bing", 403)
    assert h.all_blocked(["brave", "bing"]) and not h.all_blocked([])


class Resp:
    def __init__(self, status):
        self.status_code = status
        self.text = "<html></html>"


class Client:
    def __init__(self, status=None, exc=None):
        self.status, self.exc, self.calls = status, exc, 0

    def request(self, *a, **k):
        self.calls += 1
        if self.exc:
            raise self.exc
        return Resp(self.status)


def test_install_wraps_the_engine_request_and_skips_a_cooling_backend():
    pytest.importorskip("ddgs")
    from ddgs.base import BaseSearchEngine

    original = BaseSearchEngine.request
    h = BackendHealth(Clock())
    try:
        assert backend_health.install(h) and backend_health.install(h)  # twice is fine

        class Engine(BaseSearchEngine):  # type: ignore[misc]
            name = "brave"
            provider = "brave"
            category = "text"
            search_url = "https://x.invalid"

            def __init__(self, client):
                self.http_client = client

            def build_payload(self, *a, **k):
                return {}

        client = Client(429)
        e = Engine(client)
        assert e.request("GET", "https://x.invalid") is None  # 429 -> nothing, as ddgs always did
        assert h.blocked("brave") and client.calls == 1
        assert e.request("GET", "https://x.invalid") is None
        assert client.calls == 1  # not asked again while cooling down
    finally:
        BaseSearchEngine.request = original


def test_install_records_a_timeout_and_reraises():
    pytest.importorskip("ddgs")
    from ddgs.base import BaseSearchEngine

    original = BaseSearchEngine.request
    h = BackendHealth(Clock())
    try:
        backend_health.install(h)

        class Engine(BaseSearchEngine):  # type: ignore[misc]
            name = "duckduckgo"
            provider = "bing"
            category = "text"
            search_url = "https://x.invalid"

            def __init__(self, client):
                self.http_client = client

            def build_payload(self, *a, **k):
                return {}

        e = Engine(Client(exc=TimeoutError("timed out")))
        with pytest.raises(TimeoutError):
            e.request("GET", "https://x.invalid")
        assert h.blocked("duckduckgo")
    finally:
        BaseSearchEngine.request = original


async def test_every_backend_cooling_is_reported_as_rate_limited_without_a_request(make_client):
    from types import SimpleNamespace

    from browser_service.engines import Deadline
    from browser_service.scrapers.search import SearchRatelimitError, classify_search_error, ddgs_search

    saved = backend_health.health
    h = BackendHealth(Clock())
    backend_health.health = h
    try:
        h.record_status("brave", 429)
        h.record_status("mojeek", 403)
        deps = SimpleNamespace(
            settings=SimpleNamespace(search_backends="brave, mojeek", search_timeout_s=5, max_download_bytes=1),
            proxy=None,
        )
        with pytest.raises(SearchRatelimitError) as ei:
            await ddgs_search(deps, "text", "q", 5, Deadline(10))
        assert classify_search_error(ei.value) == "rate_limited"
    finally:
        backend_health.health = saved
