"""SEC EDGAR provider — keyless US funding/intent + decision-makers.

Fully offline: all HTTP is served by an httpx.MockTransport router keyed by URL.
Covers: name→CIK resolution (tickers + full-text fallback), Form D mapping
(amount/date/related persons→decision_makers/industry), graceful no-match,
User-Agent header presence, and the self-rate-limit guard.
"""
import asyncio
import json

import httpx
import pytest

import apps.api.services.leadgen.enrichment.providers.sec_edgar as sec
from apps.api.services.leadgen.enrichment.providers.sec_edgar import (
    SecEdgarProvider, _RateLimiter, _norm_name, _pad_cik, _parse_form_d,
)
from apps.api.services.leadgen.models import Lead


# ── Fixtures: real-shape SEC payloads ──────────────────────────────────────

_TICKERS = {
    "0": {"cik_str": 789019, "ticker": "MSFT", "title": "MICROSOFT CORP"},
    "1": {"cik_str": 320193, "ticker": "AAPL", "title": "Apple Inc."},
}

# Submissions JSON — parallel arrays, newest first. A 10-K precedes the Form D
# to prove we skip non-D forms.
_SUBMISSIONS = {
    "filings": {
        "recent": {
            "form": ["10-K", "D", "8-K"],
            "accessionNumber": [
                "0000320193-24-000010",
                "0001234567-23-000042",
                "0000320193-23-000005",
            ],
        }
    }
}

# Form D primary_doc.xml — element names verified against a live SEC filing.
_FORM_D_XML = """<?xml version="1.0"?>
<edgarSubmission>
  <schemaVersion>X0708</schemaVersion>
  <submissionType>D</submissionType>
  <primaryIssuer>
    <cik>0000320193</cik>
    <entityName>Acme Robotics Inc.</entityName>
  </primaryIssuer>
  <offeringData>
    <industryGroup>
      <industryGroupType>Technology</industryGroupType>
    </industryGroup>
    <typeOfFiling>
      <dateOfFirstSale>
        <value>2024-03-15</value>
      </dateOfFirstSale>
    </typeOfFiling>
    <offeringSalesAmounts>
      <totalOfferingAmount>12000000</totalOfferingAmount>
      <totalAmountSold>9500000</totalAmountSold>
      <totalRemaining>2500000</totalRemaining>
    </offeringSalesAmounts>
    <relatedPersonsList>
      <relatedPersonInfo>
        <relatedPersonName>
          <firstName>Jane</firstName>
          <lastName>Doe</lastName>
        </relatedPersonName>
        <relatedPersonRelationshipList>
          <relationship>Executive Officer</relationship>
          <relationship>Director</relationship>
        </relatedPersonRelationshipList>
      </relatedPersonInfo>
      <relatedPersonInfo>
        <relatedPersonName>
          <firstName>N/A</firstName>
          <lastName>Holdco LLC</lastName>
        </relatedPersonName>
        <relatedPersonRelationshipList>
          <relationship>Promoter</relationship>
        </relatedPersonRelationshipList>
      </relatedPersonInfo>
    </relatedPersonsList>
  </offeringData>
</edgarSubmission>
"""


def _router(captured_headers=None):
    """Build an httpx handler that routes by URL to the right fixture and,
    optionally, records the request headers for assertion."""
    def handler(request: httpx.Request) -> httpx.Response:
        if captured_headers is not None:
            captured_headers.append(dict(request.headers))
        url = str(request.url)
        if "company_tickers.json" in url:
            return httpx.Response(200, json=_TICKERS)
        if "efts.sec.gov" in url:
            return httpx.Response(200, json={"hits": {"hits": [
                {"_source": {"ciks": ["0001234567"],
                             "display_names": ["Acme Robotics Inc. (CIK 0001234567)"]}}
            ]}})
        if "/submissions/" in url:
            return httpx.Response(200, json=_SUBMISSIONS)
        if "primary_doc.xml" in url:
            return httpx.Response(200, text=_FORM_D_XML)
        return httpx.Response(404)
    return handler


def _patch(monkeypatch, handler):
    """Force httpx.AsyncClient onto a MockTransport and neutralize the rate
    limiter's sleep so tests stay fast (we assert the guard separately)."""
    real_init = httpx.AsyncClient.__init__

    def init(self, *a, **kw):
        kw["transport"] = httpx.MockTransport(handler)
        real_init(self, *a, **kw)

    monkeypatch.setattr(httpx.AsyncClient, "__init__", init)


# ── name→CIK resolution ────────────────────────────────────────────────────

def test_cik_from_tickers_exact_name(monkeypatch):
    _patch(monkeypatch, _router())
    p = SecEdgarProvider()
    res = asyncio.run(p.enrich(Lead(company="Apple")))  # "Apple Inc." after suffix-strip
    assert res.fields.get("sec_cik") == "0000320193"


def test_cik_fulltext_fallback_for_private_company(monkeypatch):
    # Not in company_tickers → resolves via Form D full-text search.
    _patch(monkeypatch, _router())
    p = SecEdgarProvider()
    res = asyncio.run(p.enrich(Lead(company="Acme Robotics")))
    assert res.success is True
    assert res.fields.get("sec_cik") == "0001234567"


def test_norm_name_strips_suffixes():
    assert _norm_name("Apple Inc.") == "apple"
    assert _norm_name("MICROSOFT CORP") == "microsoft"
    assert _norm_name("Acme Robotics, LLC") == "acme robotics"


def test_pad_cik():
    assert _pad_cik(320193) == "0000320193"
    assert _pad_cik("789019") == "0000789019"
    assert _pad_cik(None) is None
    assert _pad_cik("nan") is None


# ── Form D mapping: amount / date / related persons → decision_makers ──────

def test_form_d_mapping_full(monkeypatch):
    _patch(monkeypatch, _router())
    p = SecEdgarProvider()
    res = asyncio.run(p.enrich(Lead(company="Apple")))
    f = res.fields
    assert res.success is True
    assert f["funding_amount"] == 9500000           # totalAmountSold preferred
    assert f["funding_date"] == "2024-03-15"
    assert f["industry"] == "Technology"
    assert f["funding_stage_signal"] == "private_placement_form_d"

    dms = json.loads(f["decision_makers"])
    names = {d["name"] for d in dms}
    assert "Jane Doe" in names
    # Entity-typed related person uses "N/A" first name → kept by last name only.
    assert "Holdco LLC" in names
    jane = next(d for d in dms if d["name"] == "Jane Doe")
    assert jane["title"] == "Executive Officer, Director"   # de-duped, ordered
    assert jane["source"] == "sec_form_d"


def test_parse_form_d_unit():
    out = _parse_form_d(_FORM_D_XML)
    assert out["funding_amount"] == 9500000
    assert out["funding_date"] == "2024-03-15"
    assert out["industry"] == "Technology"
    assert json.loads(out["decision_makers"])[0]["name"] == "Jane Doe"


def test_parse_form_d_malformed_returns_empty():
    assert _parse_form_d("<not-valid-xml") == {}


def test_industry_other_is_dropped():
    xml = _FORM_D_XML.replace("Technology", "Other")
    out = _parse_form_d(xml)
    assert "industry" not in out  # "Other" is noise, not a real industry


# ── Graceful no-match / errors ─────────────────────────────────────────────

def test_no_match_returns_empty_not_crash(monkeypatch):
    _patch(monkeypatch, lambda r: httpx.Response(404))
    p = SecEdgarProvider()
    res = asyncio.run(p.enrich(Lead(company="Nonexistent Co")))
    assert res.success is False
    assert res.error == "no_cik_match"


def test_empty_company_short_circuits():
    p = SecEdgarProvider()
    res = asyncio.run(p.enrich(Lead(company="")))
    assert res.success is False and res.error == "no_company"


def test_server_error_does_not_raise(monkeypatch):
    _patch(monkeypatch, lambda r: httpx.Response(500))
    p = SecEdgarProvider()
    res = asyncio.run(p.enrich(Lead(company="Apple")))
    assert res.success is False  # 500 on tickers → no CIK, handled gracefully


# ── Skeptic constraints: User-Agent header + rate-limit guard ──────────────

def test_user_agent_header_present_on_every_request(monkeypatch):
    captured = []
    _patch(monkeypatch, _router(captured))
    p = SecEdgarProvider()
    asyncio.run(p.enrich(Lead(company="Apple")))
    assert captured, "expected at least one HTTP request"
    for headers in captured:
        ua = headers.get("user-agent", "")
        assert "Yupcha" in ua and "@" in ua, f"bad UA: {ua!r}"


def test_rate_limiter_enforces_minimum_spacing():
    # 8 req/s → 125 ms min spacing. Three acquires must take >= ~2 intervals.
    limiter = _RateLimiter(max_per_sec=8.0)

    async def run():
        import time
        t0 = time.monotonic()
        for _ in range(3):
            await limiter.acquire()
        return time.monotonic() - t0

    elapsed = asyncio.run(run())
    assert elapsed >= 2 * (1.0 / 8.0) * 0.9  # 2 gaps, small tolerance


def test_provider_is_keyless():
    p = SecEdgarProvider()
    assert p.requires_api_key is False
    assert p.cost_per_lookup == 0.0
    assert "sec_cik" in p.capabilities
