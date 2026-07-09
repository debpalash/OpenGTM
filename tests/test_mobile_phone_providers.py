"""
Person mobile-phone finder waterfall — Prospeo + LeadMagic (BYOK, declarative
manifests) feeding the new DEFAULT_WATERFALLS["mobile_phone"] chain.

NEVER hits a real vendor: all HTTP goes through httpx.MockTransport (same
pattern as tests/test_companies_house_provider.py) and the SSRF url_guard is
stubbed so no DNS resolution happens. Async entrypoints run via asyncio.run
(the repo has no pytest-asyncio). Covers, per vendor:
  • success → mobile_phone extracted + E.164-normalized, correct auth header/body
  • no-result → success=False (no garbage value)
  • 4xx → success=False, http_4xx error
  • missing key → graceful skip (unavailable, failure result, ZERO network)
plus waterfall registration/order and phone normalization.
"""
import asyncio
import json

import httpx

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.enrichment.normalize import normalize_phone
from apps.api.services.leadgen.enrichment.declarative.manifest import load_all_manifests
from apps.api.services.leadgen.enrichment.declarative.compiler import compile_manifest


# ── helpers ──────────────────────────────────────────────────────────────

def run(coro):
    return asyncio.run(coro)


def _manifest(name):
    m = next((m for m in load_all_manifests() if m.name == name), None)
    assert m is not None, f"manifest {name} not found"
    return m


def _provider(name, key=""):
    """Compile the bundled manifest with an injected env resolver."""
    env_var = _manifest(name).auth.env_var
    return compile_manifest(_manifest(name), env_resolver=lambda v: key if v == env_var else "")


def _patch_transport(monkeypatch, handler):
    """Route every httpx.AsyncClient through a MockTransport.
    `handler(request) -> httpx.Response`."""
    real_init = httpx.AsyncClient.__init__

    def init(self, *a, **kw):
        kw["transport"] = httpx.MockTransport(handler)
        real_init(self, *a, **kw)

    monkeypatch.setattr(httpx.AsyncClient, "__init__", init)


def _no_dns(monkeypatch):
    """Stub the SSRF guard so tests never resolve vendor hostnames."""
    import apps.api.core.url_guard as ug
    monkeypatch.setattr(ug, "check_url", lambda url, **kw: url)


_LEAD = Lead(company="SpaceX", website="spacex.com",
             linkedin_url="https://linkedin.com/in/elonmusk",
             email="elon@spacex.com")


# ── phone normalization (no phonenumbers dep — regex only) ───────────────

def test_normalize_phone_e164_from_plus():
    assert normalize_phone("+1 (415) 555-0132") == "+14155550132"

def test_normalize_phone_00_prefix():
    assert normalize_phone("0044 20 7946 0958") == "+442079460958"

def test_normalize_phone_no_country_info_just_cleans():
    assert normalize_phone("415.555.0132") == "4155550132"

def test_normalize_phone_passthrough_garbage():
    assert normalize_phone("") == ""
    assert normalize_phone(None) is None
    assert normalize_phone("not a phone") == "not a phone"


# ── manifests load + shape ───────────────────────────────────────────────

def test_bundled_mobile_manifests_load():
    prospeo = _manifest("prospeo_mobile")
    assert prospeo.capability == "mobile_phone"
    assert prospeo.auth.env_var == "PROSPEO_API_KEY"
    assert prospeo.auth.param == "X-KEY"
    lm = _manifest("leadmagic_mobile")
    assert "mobile_phone" in lm.all_capabilities()
    assert lm.auth.env_var == "LEADMAGIC_API_KEY"
    assert lm.auth.param == "X-API-Key"


# ── missing key → graceful skip, zero network ────────────────────────────

def test_prospeo_mobile_missing_key_skips_without_network(monkeypatch):
    calls = []

    def handler(request):
        calls.append(request)
        return httpx.Response(200, json={})

    _patch_transport(monkeypatch, handler)
    _no_dns(monkeypatch)
    p = _provider("prospeo_mobile", key="")
    assert p.is_available() is False
    res = run(p.enrich(_LEAD))
    assert res.success is False
    assert "PROSPEO_API_KEY" in (res.error or "")
    assert calls == []  # never hit the network


def test_leadmagic_mobile_missing_key_skips_without_network(monkeypatch):
    calls = []
    _patch_transport(monkeypatch, lambda r: (calls.append(r), httpx.Response(200, json={}))[1])
    _no_dns(monkeypatch)
    p = _provider("leadmagic_mobile", key="")
    assert p.is_available() is False
    res = run(p.enrich(_LEAD))
    assert res.success is False and "LEADMAGIC_API_KEY" in (res.error or "")
    assert calls == []


# ── Prospeo mobile-finder ────────────────────────────────────────────────

def test_prospeo_mobile_success(monkeypatch):
    seen = {}

    def handler(request):
        seen["url"] = str(request.url)
        seen["x_key"] = request.headers.get("X-KEY")
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json={
            "error": False,
            "response": {
                "raw_format": "+14155550132",
                "international_format": "+1 415-555-0132",
                "national_format": "(415) 555-0132",
                "country_code": "US",
            },
        })

    _patch_transport(monkeypatch, handler)
    _no_dns(monkeypatch)
    res = run(_provider("prospeo_mobile", key="pk-test").enrich(_LEAD))
    assert res.success is True
    assert res.fields["mobile_phone"] == "+14155550132"
    assert res.confidence == 0.8
    assert seen["url"] == "https://api.prospeo.io/mobile-finder"
    assert seen["x_key"] == "pk-test"
    assert seen["body"] == {"url": "https://linkedin.com/in/elonmusk"}


def test_prospeo_mobile_normalizes_to_e164(monkeypatch):
    _patch_transport(monkeypatch, lambda r: httpx.Response(200, json={
        "error": False, "response": {"raw_format": "+1 (415) 555-0132"},
    }))
    _no_dns(monkeypatch)
    res = run(_provider("prospeo_mobile", key="k").enrich(_LEAD))
    assert res.fields["mobile_phone"] == "+14155550132"


def test_prospeo_mobile_error_envelope_is_failure(monkeypatch):
    # Prospeo signals no-result/errors as {"error": true, "message": ...} on a 200
    _patch_transport(monkeypatch, lambda r: httpx.Response(200, json={
        "error": True, "message": "NO_RESULT",
    }))
    _no_dns(monkeypatch)
    res = run(_provider("prospeo_mobile", key="k").enrich(_LEAD))
    assert res.success is False
    assert "NO_RESULT" in (res.error or "")


def test_prospeo_mobile_4xx_is_failure(monkeypatch):
    _patch_transport(monkeypatch, lambda r: httpx.Response(401, json={"error": True}))
    _no_dns(monkeypatch)
    res = run(_provider("prospeo_mobile", key="bad").enrich(_LEAD))
    assert res.success is False and res.error == "http_401"


# ── LeadMagic mobile-finder ──────────────────────────────────────────────

def test_leadmagic_mobile_success(monkeypatch):
    seen = {}

    def handler(request):
        seen["url"] = str(request.url)
        seen["api_key"] = request.headers.get("X-API-Key")
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json={
            "message": "Mobile number found",
            "credits_consumed": 1,
            "mobile_number": "+1 415 555 0132",
        })

    _patch_transport(monkeypatch, handler)
    _no_dns(monkeypatch)
    res = run(_provider("leadmagic_mobile", key="lm-test").enrich(_LEAD))
    assert res.success is True
    assert res.fields["mobile_phone"] == "+14155550132"  # normalized
    assert res.fields["phone"] == "+14155550132"         # back-compat mapping
    assert seen["url"] == "https://api.leadmagic.io/mobile-finder"
    assert seen["api_key"] == "lm-test"
    assert seen["body"]["profile_url"] == "https://linkedin.com/in/elonmusk"
    assert seen["body"]["work_email"] == "elon@spacex.com"


def test_leadmagic_mobile_no_result_is_failure(monkeypatch):
    # Per LeadMagic OpenAPI, mobile_number is nullable on a 200
    _patch_transport(monkeypatch, lambda r: httpx.Response(200, json={
        "message": "No mobile number found", "credits_consumed": 0,
        "mobile_number": None,
    }))
    _no_dns(monkeypatch)
    res = run(_provider("leadmagic_mobile", key="k").enrich(_LEAD))
    assert res.success is False and res.error == "no_data"


def test_leadmagic_mobile_4xx_is_failure(monkeypatch):
    _patch_transport(monkeypatch, lambda r: httpx.Response(403, json={"message": "invalid key"}))
    _no_dns(monkeypatch)
    res = run(_provider("leadmagic_mobile", key="bad").enrich(_LEAD))
    assert res.success is False and res.error == "http_403"


# ── waterfall registration + ordering ────────────────────────────────────

def test_mobile_phone_waterfall_is_byok_only_and_cost_ordered():
    from apps.api.services.workbook.enrichment import DEFAULT_WATERFALLS
    from apps.api.services.workbook import vendor_catalog

    chain = DEFAULT_WATERFALLS["mobile_phone"]
    assert chain == ["leadmagic_mobile", "prospeo_mobile"]

    # No company-phone providers in the person-mobile chain
    for company_phone in ("numverify", "google_maps", "facebook_pages",
                          "local_business", "deep_scraper", "website_scraper"):
        assert company_phone not in chain

    # Cost-ordered: cheapest first
    costs = [vendor_catalog.base_cost(n) for n in chain]
    assert costs == sorted(costs)
    assert all(c > 0 for c in costs)  # all BYOK/paid, none free-OSS


def test_mobile_providers_registered_with_capability():
    from apps.api.services.workbook.providers import get_provider

    for name in ("leadmagic_mobile", "prospeo_mobile"):
        p = get_provider(name)
        assert p is not None, f"{name} not registered"
        assert p.can_provide("mobile_phone")
        # BYOK: unavailable in the test env (no keys) but registered + skippable
        assert callable(getattr(p, "is_available", None))
