"""Tests for the re-implemented Internet Archive (archive.org) source.

The HTTP layer is fully mocked so these run deterministically offline. A single
live smoke test is included but guarded behind RUN_LIVE_ARCHIVE_TESTS so CI/dev
without network stays green.
"""

import asyncio
import os

import pytest

from apps.api.sources import FileType
from apps.api.sources.legacy import ArchiveSource


# --- Fake aiohttp plumbing -------------------------------------------------


class _FakeResponse:
    def __init__(self, status=200, payload=None, raise_on_json=None):
        self.status = status
        self._payload = payload
        self._raise_on_json = raise_on_json

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False

    async def json(self, content_type=None):
        if self._raise_on_json is not None:
            raise self._raise_on_json
        return self._payload


class _FakeSession:
    """Records the request and returns a canned response."""

    last_url = None
    last_params = None

    def __init__(self, response):
        self._response = response

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False

    def get(self, url, params=None, timeout=None):
        type(self).last_url = url
        type(self).last_params = params
        return self._response


def _patch_session(monkeypatch, response, session_cls=_FakeSession):
    import apps.api.sources.legacy as legacy

    monkeypatch.setattr(
        legacy.aiohttp,
        "ClientSession",
        lambda *a, **k: session_cls(response),
    )


SAMPLE_PAYLOAD = {
    "response": {
        "numFound": 2,
        "docs": [
            {
                "identifier": "the-art-of-war",
                "title": "The Art of War",
                "creator": ["Sun Tzu", "Lionel Giles"],
                "year": "1910",
                "mediatype": "texts",
                "format": ["PDF", "Text", "EPUB"],
                "description": "A classic treatise on military strategy.",
            },
            {
                "identifier": "weird item/with spaces",
                "title": ["Multi", "Valued Title"],
                "creator": "Anonymous",
                "date": "2001-05-03T00:00:00Z",
                "mediatype": "movies",
                "format": "MPEG4",
            },
        ],
    }
}


# --- Tests -----------------------------------------------------------------


def test_query_maps_to_results(monkeypatch):
    src = ArchiveSource()
    _patch_session(monkeypatch, _FakeResponse(payload=SAMPLE_PAYLOAD))

    results = asyncio.run(src.search("art of war", limit=20))

    assert len(results) == 2
    first = results[0]
    assert first.id == "archive_the-art-of-war"
    assert first.title == "The Art of War"
    assert first.source == "archive"
    assert first.url == "https://archive.org/details/the-art-of-war"
    assert first.download_url == "https://archive.org/download/the-art-of-war"
    assert first.thumbnail == "https://archive.org/services/img/the-art-of-war"
    assert first.author == "Sun Tzu, Lionel Giles"
    assert first.year == 1910
    assert first.file_type == FileType.PDF.value
    assert "military strategy" in first.snippet


def test_query_is_passed_to_api(monkeypatch):
    src = ArchiveSource()
    _patch_session(monkeypatch, _FakeResponse(payload=SAMPLE_PAYLOAD))

    asyncio.run(src.search("python programming", limit=5))

    assert _FakeSession.last_url == ArchiveSource.BASE_URL
    params = _FakeSession.last_params
    assert params["q"] == "python programming"
    assert params["rows"] == 5
    assert params["output"] == "json"


def test_special_chars_and_list_fields(monkeypatch):
    src = ArchiveSource()
    _patch_session(monkeypatch, _FakeResponse(payload=SAMPLE_PAYLOAD))

    results = asyncio.run(src.search("x"))
    second = results[1]
    # identifier with spaces/slash must be URL-encoded in links
    assert second.url == "https://archive.org/details/weird%20item%2Fwith%20spaces"
    # list title is joined
    assert second.title == "Multi, Valued Title"
    # string creator preserved
    assert second.author == "Anonymous"
    # year parsed out of an ISO date string
    assert second.year == 2001
    # unmapped format falls back to OTHER
    assert second.file_type == FileType.OTHER.value


def test_limit_is_respected(monkeypatch):
    src = ArchiveSource()
    _patch_session(monkeypatch, _FakeResponse(payload=SAMPLE_PAYLOAD))

    results = asyncio.run(src.search("anything", limit=1))
    assert len(results) == 1


def test_empty_query_returns_empty(monkeypatch):
    src = ArchiveSource()
    # session should never be hit; patch with one that explodes if used
    _patch_session(
        monkeypatch, _FakeResponse(raise_on_json=AssertionError("must not call"))
    )
    assert asyncio.run(src.search("   ")) == []


def test_non_200_returns_empty(monkeypatch):
    src = ArchiveSource()
    _patch_session(monkeypatch, _FakeResponse(status=503, payload={}))
    assert asyncio.run(src.search("boom")) == []


def test_json_decode_error_is_handled(monkeypatch):
    src = ArchiveSource()
    _patch_session(
        monkeypatch,
        _FakeResponse(raise_on_json=ValueError("not json")),
    )
    assert asyncio.run(src.search("bad")) == []


def test_network_error_is_handled(monkeypatch):
    import apps.api.sources.legacy as legacy

    src = ArchiveSource()

    class _BoomSession:
        def __init__(self, *a, **k):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *exc):
            return False

        def get(self, *a, **k):
            raise legacy.aiohttp.ClientError("connection refused")

    monkeypatch.setattr(legacy.aiohttp, "ClientSession", _BoomSession)
    assert asyncio.run(src.search("down")) == []


def test_doc_without_identifier_skipped(monkeypatch):
    src = ArchiveSource()
    payload = {"response": {"docs": [{"title": "no id"}, {"identifier": "ok"}]}}
    _patch_session(monkeypatch, _FakeResponse(payload=payload))

    results = asyncio.run(src.search("x"))
    assert len(results) == 1
    assert results[0].id == "archive_ok"


@pytest.mark.skipif(
    not os.environ.get("RUN_LIVE_ARCHIVE_TESTS"),
    reason="live network test; set RUN_LIVE_ARCHIVE_TESTS=1 to enable",
)
def test_live_archive_search():
    src = ArchiveSource()
    results = asyncio.run(src.search("python programming", limit=3))
    assert len(results) > 0
    for r in results:
        assert r.source == "archive"
        assert r.url.startswith("https://archive.org/details/")
        assert r.title
