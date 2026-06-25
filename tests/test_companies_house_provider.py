"""Offline tests for the Companies House enrichment provider.

All network is mocked with httpx.MockTransport (same pattern as
tests/test_http_column.py). Async entrypoints are driven with asyncio.run
(the repo has no pytest-asyncio). Covers:
  • name → company-number resolution (prefers an active company)
  • officers → director-level decision_makers (role + appointment date)
  • PSC → beneficial owners appended to decision_makers
  • firmographic mapping (company_number/status/incorporation/address)
  • no API key → graceful skip, ZERO network calls
  • no search match → graceful skip
"""
import asyncio
import json

import httpx

import apps.api.services.leadgen.enrichment.providers.companies_house as ch
from apps.api.services.leadgen.enrichment.providers.companies_house import (
    CompaniesHouseProvider,
)
from apps.api.services.leadgen.models import Lead


# ── helpers ──────────────────────────────────────────────────────────────

def run(coro):
    return asyncio.run(coro)


def _enrich(company="Acme"):
    return run(CompaniesHouseProvider().enrich(Lead(company=company)))


def _patch_transport(monkeypatch, handler):
    """Route every httpx.AsyncClient through a MockTransport.
    `handler(request) -> httpx.Response`."""
    real_init = httpx.AsyncClient.__init__

    def init(self, *a, **kw):
        kw["transport"] = httpx.MockTransport(handler)
        real_init(self, *a, **kw)

    monkeypatch.setattr(httpx.AsyncClient, "__init__", init)


def _with_key(monkeypatch, key="TEST-KEY"):
    monkeypatch.setattr(ch, "_get_api_key", lambda: key)


# Sample Companies House API payloads.
_SEARCH = {
    "items": [
        {"company_number": "00000111", "title": "ACME OLD LTD",
         "company_status": "dissolved"},
        {"company_number": "01234567", "title": "ACME LIMITED",
         "company_status": "active"},
    ]
}
_PROFILE = {
    "company_number": "01234567",
    "company_name": "ACME LIMITED",
    "company_status": "active",
    "date_of_creation": "2010-04-21",
    "registered_office_address": {
        "address_line_1": "1 High Street",
        "locality": "London",
        "postal_code": "EC1A 1BB",
        "country": "United Kingdom",
    },
}
_OFFICERS = {
    "items": [
        {"name": "DOE, Jane", "officer_role": "director",
         "appointed_on": "2010-04-21"},
        {"name": "SMITH, John", "officer_role": "director",
         "appointed_on": "2012-01-01", "resigned_on": "2015-06-30"},
        {"name": "BOOKS, Bill", "officer_role": "secretary",
         "appointed_on": "2011-01-01"},
    ]
}
_PSC = {
    "items": [
        {"name": "Jane Doe", "kind": "individual-person-with-significant-control",
         "notified_on": "2016-04-06",
         "natures_of_control": ["ownership-of-shares-75-to-100-percent"]},
        {"name": "Holdco Ventures Ltd",
         "kind": "corporate-entity-person-with-significant-control",
         "notified_on": "2018-02-01"},
    ]
}


def _router(monkeypatch, *, search=_SEARCH, profile=_PROFILE,
            officers=_OFFICERS, psc=_PSC, key="TEST-KEY"):
    """Wire a full happy-path Companies House backend and record visited paths."""
    _with_key(monkeypatch, key)
    visited = []

    def handler(request: httpx.Request) -> httpx.Response:
        path = request.url.path
        visited.append(path)
        if path == "/search/companies":
            return httpx.Response(200, json=search)
        if path.endswith("/officers"):
            return httpx.Response(200, json=officers)
        if path.endswith("/persons-with-significant-control"):
            return httpx.Response(200, json=psc)
        if path.startswith("/company/"):
            return httpx.Response(200, json=profile)
        return httpx.Response(404)

    _patch_transport(monkeypatch, handler)
    return visited


# ── name → company number ────────────────────────────────────────────────

def test_resolves_name_to_active_company_number(monkeypatch):
    visited = _router(monkeypatch)
    res = _enrich()

    assert res.success is True
    # Picked the ACTIVE company (01234567), not the dissolved top hit.
    assert res.fields["company_number"] == "01234567"
    assert "/search/companies" in visited
    assert "/company/01234567" in visited


def test_firmographics_mapping(monkeypatch):
    _router(monkeypatch)
    res = _enrich()

    f = res.fields
    assert f["company_status"] == "active"
    assert f["incorporation_date"] == "2010-04-21"
    assert f["founded_year"] == "2010"
    assert "1 High Street" in f["registered_address"]
    assert "EC1A 1BB" in f["registered_address"]
    # registered office also surfaced into the canonical Lead address field
    assert f["address"] == f["registered_address"]


# ── officers → decision_makers ───────────────────────────────────────────

def test_officers_map_to_decision_makers(monkeypatch):
    _router(monkeypatch)
    res = _enrich()

    dms = json.loads(res.fields["decision_makers"])
    by_name = {d["name"]: d for d in dms}

    # Active director present with role + appointment date.
    assert "DOE, Jane" in by_name
    jane = by_name["DOE, Jane"]
    assert jane["role"] == "director"
    assert jane["title"] == "Director"
    assert jane["appointed_on"] == "2010-04-21"
    assert jane["source"] == "officer"

    # Secretary is NOT counted as a decision-maker.
    assert "BOOKS, Bill" not in by_name

    # The top entry (a live director) is surfaced to the cell.
    assert res.fields["contact_person"] == "DOE, Jane"
    assert res.fields["contact_title"] == "Director"

    # Active director sorts before the resigned one.
    names = [d["name"] for d in dms if d["source"] == "officer"]
    assert names.index("DOE, Jane") < names.index("SMITH, John")


# ── PSC → beneficial owners ──────────────────────────────────────────────

def test_psc_maps_to_beneficial_owners(monkeypatch):
    _router(monkeypatch)
    res = _enrich()

    dms = json.loads(res.fields["decision_makers"])
    owners = [d for d in dms if d["source"] == "psc"]

    holdco = next(d for d in owners if d["name"] == "Holdco Ventures Ltd")
    assert holdco["role"] == "beneficial-owner"
    assert holdco["title"] == "Beneficial Owner"
    assert holdco["appointed_on"] == "2018-02-01"


def test_ceased_psc_excluded(monkeypatch):
    psc = {"items": [
        {"name": "Gone Owner", "notified_on": "2016-01-01",
         "ceased_on": "2020-01-01"},
        {"name": "Live Owner", "notified_on": "2016-01-01"},
    ]}
    _router(monkeypatch, psc=psc)
    res = _enrich()
    owners = [d["name"] for d in json.loads(res.fields["decision_makers"])
              if d["source"] == "psc"]
    assert "Live Owner" in owners
    assert "Gone Owner" not in owners


# ── graceful degradation ─────────────────────────────────────────────────

def test_no_api_key_skips_without_network(monkeypatch):
    monkeypatch.setattr(ch, "_get_api_key", lambda: "")

    called = {"n": 0}

    def handler(request):
        called["n"] += 1
        return httpx.Response(200, json={})

    _patch_transport(monkeypatch, handler)

    res = _enrich()
    assert res.success is False
    assert "key" in res.error.lower()
    assert called["n"] == 0          # never touched the network


def test_no_company_name(monkeypatch):
    _with_key(monkeypatch)
    res = _enrich("  ")
    assert res.success is False
    assert res.error == "no_company"


def test_no_search_match_graceful(monkeypatch):
    _router(monkeypatch, search={"items": []})
    res = _enrich("Nonexistent Co")
    assert res.success is False
    assert res.error == "not_in_companies_house"


def test_api_error_graceful(monkeypatch):
    _with_key(monkeypatch)
    _patch_transport(monkeypatch, lambda r: httpx.Response(500))
    res = _enrich()
    assert res.success is False  # 500 on search → not_in_companies_house, no crash


def test_officers_psc_failure_still_yields_firmographics(monkeypatch):
    # Officers/PSC endpoints error out, but the profile resolved — we should still
    # return firmographics rather than crash.
    _with_key(monkeypatch)

    def handler(request):
        path = request.url.path
        if path == "/search/companies":
            return httpx.Response(200, json=_SEARCH)
        if path.endswith("/officers") or path.endswith("persons-with-significant-control"):
            return httpx.Response(503)
        if path.startswith("/company/"):
            return httpx.Response(200, json=_PROFILE)
        return httpx.Response(404)

    _patch_transport(monkeypatch, handler)
    res = _enrich()
    assert res.success is True
    assert res.fields["company_number"] == "01234567"
    assert "decision_makers" not in res.fields  # none fetched, but no crash
