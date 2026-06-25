"""GLEIF provider — keyless, CC0 legal-entity + corporate-hierarchy registry.

Fully offline: all HTTP is served by an httpx.MockTransport router keyed by URL.
Covers: fuzzy name->LEI mapping, canonical name + country/city extraction, the
direct/ultimate parent hierarchy mapping, graceful no-match, and the keyless
contract.
"""
import asyncio
import json

import httpx

import apps.api.services.leadgen.enrichment.providers.gleif as gleif
from apps.api.services.leadgen.enrichment.providers.gleif import (
    GleifProvider, _RateLimiter, _norm_name, _record_to_fields, _first_record,
)
from apps.api.services.leadgen.models import Lead


# ── Fixtures: real-shape GLEIF JSON:API payloads ────────────────────────────

_CHILD_LEI = "54930027SQL2KPSDBM58"   # subsidiary, has parents
_PARENT_LEI = "HWUPKR0MPOU8FGXBT394"  # ultimate parent (Apple Inc.)

# fuzzycompletions response: each row carries a value + a related lei-records id.
_FUZZY = {
    "data": [
        {
            "type": "fuzzycompletions",
            "attributes": {"value": "APPLE DISTRIBUTION INTERNATIONAL LIMITED"},
            "relationships": {
                "lei-records": {
                    "data": {"type": "lei-records", "id": _CHILD_LEI},
                    "links": {"related": f"https://api.gleif.org/api/v1/lei-records/{_CHILD_LEI}"},
                }
            },
        }
    ]
}


def _lei_record(lei, name, country, city, region, status="ACTIVE",
                ra="RA000402", jurisdiction="IE", with_parents=False):
    rels = {
        "lei-issuer": {"links": {"related": f"https://api.gleif.org/api/v1/lei-records/{lei}/lei-issuer"}},
    }
    if with_parents:
        rels["direct-parent"] = {"links": {
            "relationship-record": f"https://api.gleif.org/api/v1/lei-records/{lei}/direct-parent-relationship",
            "lei-record": f"https://api.gleif.org/api/v1/lei-records/{lei}/direct-parent",
        }}
        rels["ultimate-parent"] = {"links": {
            "relationship-record": f"https://api.gleif.org/api/v1/lei-records/{lei}/ultimate-parent-relationship",
            "lei-record": f"https://api.gleif.org/api/v1/lei-records/{lei}/ultimate-parent",
        }}
    return {
        "type": "lei-records",
        "id": lei,
        "attributes": {
            "lei": lei,
            "entity": {
                "legalName": {"name": name, "language": "en"},
                "legalAddress": {"city": city, "region": region, "country": country},
                "status": status,
                "registeredAt": {"id": ra, "other": None},
                "jurisdiction": jurisdiction,
            },
        },
        "relationships": rels,
    }


_CHILD_RECORD = _lei_record(
    _CHILD_LEI, "APPLE DISTRIBUTION INTERNATIONAL LIMITED",
    "IE", "CORK", "IE-CO", jurisdiction="IE", ra="RA000402", with_parents=True,
)
_PARENT_RECORD = _lei_record(
    _PARENT_LEI, "Apple Inc.", "US", "CUPERTINO", "US-CA",
    jurisdiction="US-CA", ra="RA000598",
)


def _router(captured_headers=None):
    """Route by URL to the right fixture; optionally record request headers."""
    def handler(request: httpx.Request) -> httpx.Response:
        if captured_headers is not None:
            captured_headers.append(dict(request.headers))
        url = str(request.url)
        if "fuzzycompletions" in url:
            return httpx.Response(200, json=_FUZZY)
        # parent lei-record links resolve to the parent's full record (object).
        if url.endswith(f"/lei-records/{_CHILD_LEI}/direct-parent") or \
           url.endswith(f"/lei-records/{_CHILD_LEI}/ultimate-parent"):
            return httpx.Response(200, json={"data": _PARENT_RECORD})
        if f"/lei-records/{_CHILD_LEI}" in url:
            return httpx.Response(200, json={"data": _CHILD_RECORD})
        if "filter%5Bentity.legalName%5D" in url or "filter[entity.legalName]" in url:
            return httpx.Response(200, json={"data": [_CHILD_RECORD]})
        return httpx.Response(404)
    return handler


def _patch(monkeypatch, handler):
    """Force httpx.AsyncClient onto a MockTransport and neutralize the rate
    limiter's sleep so tests stay fast (the guard is asserted separately)."""
    real_init = httpx.AsyncClient.__init__

    def init(self, *a, **kw):
        kw["transport"] = httpx.MockTransport(handler)
        real_init(self, *a, **kw)

    monkeypatch.setattr(httpx.AsyncClient, "__init__", init)


# ── fuzzy name→LEI mapping ──────────────────────────────────────────────────

def test_fuzzy_name_resolves_to_lei(monkeypatch):
    _patch(monkeypatch, _router())
    p = GleifProvider()
    res = asyncio.run(p.enrich(Lead(company="Apple Distribution International")))
    assert res.success is True
    assert res.fields.get("lei") == _CHILD_LEI


# ── canonical name + country/city extraction ────────────────────────────────

def test_canonical_name_and_address(monkeypatch):
    _patch(monkeypatch, _router())
    p = GleifProvider()
    res = asyncio.run(p.enrich(Lead(company="Apple Distribution International")))
    f = res.fields
    assert f["legal_name"] == "APPLE DISTRIBUTION INTERNATIONAL LIMITED"
    assert f["country"] == "IE"
    assert f["city"] == "CORK"
    assert f["region"] == "IE-CO"
    assert f["entity_status"] == "ACTIVE"
    assert f["registration_authority"] == "RA000402"
    assert f["jurisdiction"] == "IE"


def test_cc0_provenance_tagged(monkeypatch):
    _patch(monkeypatch, _router())
    p = GleifProvider()
    res = asyncio.run(p.enrich(Lead(company="Apple Distribution International")))
    assert res.fields["gleif_source"] == "gleif"
    assert res.fields["gleif_license"] == "CC0-1.0"


# ── parent / hierarchy mapping ──────────────────────────────────────────────

def test_corporate_hierarchy_mapping(monkeypatch):
    _patch(monkeypatch, _router())
    p = GleifProvider()
    res = asyncio.run(p.enrich(Lead(company="Apple Distribution International")))
    f = res.fields
    assert f["parent_lei"] == _PARENT_LEI
    assert f["ultimate_parent_lei"] == _PARENT_LEI
    assert f["ultimate_parent_name"] == "Apple Inc."

    hierarchy = json.loads(f["corporate_hierarchy"])
    assert hierarchy["direct_parent"]["lei"] == _PARENT_LEI
    assert hierarchy["ultimate_parent"]["legal_name"] == "Apple Inc."
    assert hierarchy["source"] == "gleif"
    assert hierarchy["license"] == "CC0-1.0"


def test_no_parent_links_yields_no_hierarchy(monkeypatch):
    """A top-level entity with no parent links must still succeed, with no
    hierarchy fields (the common 'reporting exception' case)."""
    def handler(request):
        url = str(request.url)
        if "fuzzycompletions" in url:
            return httpx.Response(200, json={"data": [{
                "attributes": {"value": "Apple Inc."},
                "relationships": {"lei-records": {"data": {"id": _PARENT_LEI}}},
            }]})
        if f"/lei-records/{_PARENT_LEI}" in url:
            return httpx.Response(200, json={"data": _PARENT_RECORD})  # no parents
        return httpx.Response(404)
    _patch(monkeypatch, handler)
    p = GleifProvider()
    res = asyncio.run(p.enrich(Lead(company="Apple Inc.")))
    assert res.success is True
    assert res.fields["lei"] == _PARENT_LEI
    assert "parent_lei" not in res.fields
    assert "corporate_hierarchy" not in res.fields


# ── graceful no-match / errors ──────────────────────────────────────────────

def test_no_match_returns_empty_not_crash(monkeypatch):
    _patch(monkeypatch, lambda r: httpx.Response(404))
    p = GleifProvider()
    res = asyncio.run(p.enrich(Lead(company="Nonexistent Co")))
    assert res.success is False
    assert res.error == "no_lei_match"


def test_empty_company_short_circuits():
    p = GleifProvider()
    res = asyncio.run(p.enrich(Lead(company="")))
    assert res.success is False and res.error == "no_company"


def test_server_error_does_not_raise(monkeypatch):
    _patch(monkeypatch, lambda r: httpx.Response(500))
    p = GleifProvider()
    res = asyncio.run(p.enrich(Lead(company="Apple")))
    assert res.success is False  # 500 everywhere → no match, handled gracefully


def test_exact_name_fallback_when_fuzzy_misses(monkeypatch):
    """Fuzzy returns nothing usable → exact legalName filter recovers the record."""
    def handler(request):
        url = str(request.url)
        if "fuzzycompletions" in url:
            return httpx.Response(200, json={"data": []})  # fuzzy whiffs
        if "entity.legalName" in url and "lei-records" in url:
            return httpx.Response(200, json={"data": [_CHILD_RECORD]})
        # parent links
        if url.endswith("/direct-parent") or url.endswith("/ultimate-parent"):
            return httpx.Response(200, json={"data": _PARENT_RECORD})
        return httpx.Response(404)
    _patch(monkeypatch, handler)
    p = GleifProvider()
    res = asyncio.run(p.enrich(Lead(company="Apple Distribution International Limited")))
    assert res.success is True
    assert res.fields["lei"] == _CHILD_LEI


# ── unit: pure mappers ──────────────────────────────────────────────────────

def test_record_to_fields_unit():
    out = _record_to_fields(_CHILD_RECORD)
    assert out["lei"] == _CHILD_LEI
    assert out["legal_name"] == "APPLE DISTRIBUTION INTERNATIONAL LIMITED"
    assert out["country"] == "IE"
    assert out["city"] == "CORK"
    assert out["entity_status"] == "ACTIVE"
    assert out["registration_authority"] == "RA000402"


def test_first_record_normalizes_list_and_object():
    assert _first_record({"data": [{"id": "X"}]})["id"] == "X"  # collection
    assert _first_record({"data": {"id": "Y"}})["id"] == "Y"    # single
    assert _first_record({"data": []}) is None
    assert _first_record(None) is None


def test_norm_name_strips_suffixes():
    assert _norm_name("Apple Inc.") == "apple"
    assert _norm_name("Acme Robotics, LLC") == "acme robotics"
    assert _norm_name("Siemens AG") == "siemens"


# ── keyless contract + rate-limit guard ─────────────────────────────────────

def test_provider_is_keyless():
    p = GleifProvider()
    assert p.requires_api_key is False
    assert p.cost_per_lookup == 0.0
    assert "lei" in p.capabilities
    assert "corporate_hierarchy" in p.capabilities


def test_accept_header_present_on_every_request(monkeypatch):
    captured = []
    _patch(monkeypatch, _router(captured))
    p = GleifProvider()
    asyncio.run(p.enrich(Lead(company="Apple Distribution International")))
    assert captured, "expected at least one HTTP request"
    for headers in captured:
        assert "application/vnd.api+json" in headers.get("accept", "")


def test_rate_limiter_enforces_minimum_spacing():
    # 5 req/s → 200 ms min spacing. Three acquires must take >= ~2 intervals.
    limiter = _RateLimiter(max_per_sec=5.0)

    async def run():
        import time
        t0 = time.monotonic()
        for _ in range(3):
            await limiter.acquire()
        return time.monotonic() - t0

    elapsed = asyncio.run(run())
    assert elapsed >= 2 * (1.0 / 5.0) * 0.9  # 2 gaps, small tolerance
