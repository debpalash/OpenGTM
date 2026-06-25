"""
Google Books source (re-implemented from the legacy stub).

The HTTP layer is mocked at the aiohttp.ClientSession.get boundary so the
query -> SearchResult mapping is exercised deterministically with no network.
An optional live test (guarded by GOOGLE_BOOKS_LIVE=1) hits the real API.
"""
import asyncio
import os
import urllib.parse

import pytest

import apps.api.sources.legacy as gb_mod
from apps.api.sources.legacy import GoogleBooksSource
from apps.api.sources import FileType


# --- Canned Google Books API payload -------------------------------------

SAMPLE_RESPONSE = {
    "kind": "books#volumes",
    "totalItems": 2,
    "items": [
        {
            "id": "vol_public",
            "selfLink": "https://www.googleapis.com/books/v1/volumes/vol_public",
            "volumeInfo": {
                "title": "Pride and Prejudice",
                "subtitle": "A Novel",
                "authors": ["Jane Austen"],
                "publishedDate": "1813-01-28",
                "description": "A classic romance about the Bennet family.",
                "infoLink": "https://books.google.com/books?id=vol_public",
                "imageLinks": {
                    "smallThumbnail": "http://example.com/small.jpg",
                    "thumbnail": "http://example.com/thumb.jpg",
                },
            },
            "accessInfo": {
                "pdf": {
                    "isAvailable": True,
                    "downloadLink": "http://example.com/vol_public.pdf",
                },
                "epub": {"isAvailable": True, "downloadLink": "http://e.com/x.epub"},
            },
            "searchInfo": {"textSnippet": "It is a truth universally acknowledged..."},
        },
        {
            "id": "vol_modern",
            "volumeInfo": {
                "title": "Modern Book",
                "authors": ["A. One", "B. Two", "C. Three", "D. Four"],
                "publishedDate": "2021",
                "infoLink": "https://books.google.com/books?id=vol_modern",
            },
            "accessInfo": {
                "pdf": {"isAvailable": False},
                "epub": {"isAvailable": False},
            },
        },
    ],
}


# --- aiohttp.ClientSession.get mock --------------------------------------


class _FakeResponse:
    def __init__(self, status, payload):
        self.status = status
        self._payload = payload

    async def json(self, *a, **k):
        return self._payload

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False


class _FakeSession:
    """Stands in for aiohttp.ClientSession; records the requested URL."""

    captured = {}

    def __init__(self, *a, **k):
        pass

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False

    def get(self, url, *a, **k):
        _FakeSession.captured["url"] = url
        _FakeSession.captured["params"] = k.get("params", {})
        return _FakeResponse(_FakeSession.captured["status"],
                             _FakeSession.captured["payload"])


def _patch(monkeypatch, payload, status=200):
    _FakeSession.captured = {"payload": payload, "status": status}
    monkeypatch.setattr(gb_mod.aiohttp, "ClientSession", _FakeSession)


# --- Tests ----------------------------------------------------------------


def test_maps_volumes_to_search_results(monkeypatch):
    _patch(monkeypatch, SAMPLE_RESPONSE)
    src = GoogleBooksSource(api_key=None)

    results = asyncio.run(src.search("austen", limit=20))

    assert len(results) == 2

    first = results[0]
    assert first.id == "google_books_vol_public"
    assert first.title == "Pride and Prejudice: A Novel"  # subtitle joined
    assert first.author == "Jane Austen"
    assert first.year == 1813  # parsed from "1813-01-28"
    assert first.source == "google_books"
    assert first.url == "https://books.google.com/books?id=vol_public"
    assert first.thumbnail == "http://example.com/thumb.jpg"
    # PDF available + downloadLink -> PDF type + download_url
    assert first.file_type == FileType.PDF.value
    assert first.download_url == "http://example.com/vol_public.pdf"
    assert first.snippet == "It is a truth universally acknowledged..."

    second = results[1]
    assert second.year == 2021  # bare-year publishedDate
    assert second.author == "A. One, B. Two, C. Three et al."  # truncated to 3
    assert second.download_url is None  # nothing downloadable
    assert second.file_type == FileType.OTHER.value


def test_query_and_limit_forwarded(monkeypatch):
    _patch(monkeypatch, SAMPLE_RESPONSE)
    src = GoogleBooksSource(api_key=None)
    asyncio.run(src.search("data science", limit=5))

    params = _FakeSession.captured["params"]
    assert _FakeSession.captured["url"] == GoogleBooksSource.BASE_URL
    assert params["q"] == "data science"
    assert params["maxResults"] == 5
    assert "key" not in params  # no api key configured


def test_limit_capped_at_40(monkeypatch):
    _patch(monkeypatch, {"items": []})
    src = GoogleBooksSource(api_key=None)
    asyncio.run(src.search("x", limit=1000))
    assert _FakeSession.captured["params"]["maxResults"] == 40


def test_api_key_forwarded_when_present(monkeypatch):
    _patch(monkeypatch, {"items": []})
    src = GoogleBooksSource(api_key="SECRET123")
    asyncio.run(src.search("x", limit=3))
    assert _FakeSession.captured["params"]["key"] == "SECRET123"


def test_api_key_read_from_env(monkeypatch):
    monkeypatch.setenv("GOOGLE_BOOKS_API_KEY", "ENVKEY")
    _patch(monkeypatch, {"items": []})
    src = GoogleBooksSource()  # no explicit key
    asyncio.run(src.search("x"))
    assert _FakeSession.captured["params"]["key"] == "ENVKEY"


def test_empty_query_returns_empty_without_request(monkeypatch):
    # If a request were made the mock would have no captured url.
    _FakeSession.captured = {}
    monkeypatch.setattr(gb_mod.aiohttp, "ClientSession", _FakeSession)
    src = GoogleBooksSource(api_key=None)
    assert asyncio.run(src.search("   ")) == []
    assert _FakeSession.captured == {}


def test_non_200_returns_empty(monkeypatch):
    _patch(monkeypatch, {"items": []}, status=503)
    src = GoogleBooksSource(api_key=None)
    assert asyncio.run(src.search("x")) == []


def test_missing_items_and_bad_volume_are_tolerated(monkeypatch):
    payload = {"items": [{"no_id": True}, {"id": "ok", "volumeInfo": {}}]}
    _patch(monkeypatch, payload)
    src = GoogleBooksSource(api_key=None)
    results = asyncio.run(src.search("x"))
    assert len(results) == 1  # bad volume (no id) skipped
    assert results[0].id == "google_books_ok"
    assert results[0].title == "Untitled"


# --- Optional live test ---------------------------------------------------


@pytest.mark.skipif(
    os.getenv("GOOGLE_BOOKS_LIVE") != "1",
    reason="set GOOGLE_BOOKS_LIVE=1 to hit the real Google Books API",
)
def test_live_google_books_search():
    src = GoogleBooksSource()  # picks up GOOGLE_BOOKS_API_KEY if set
    results = asyncio.run(src.search("python programming", limit=5))
    assert results, "expected at least one live result"
    r = results[0]
    assert r.id.startswith("google_books_")
    assert r.title
    assert r.url
    assert r.source == "google_books"
