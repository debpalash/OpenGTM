"""
Unit tests for the lead-data enrichment upgrades (offline — no network/ddgs).

Covers the always-on additions from the goldmine wire-in:
  - email_verify: classification + SMTP-disabled degradation
  - jsonld_firmographics: schema.org / OpenGraph extraction
  - tech_stack_provider: full-DB fingerprint detection + `implies`
  - provider registry + DEFAULT_WATERFALLS wiring
  - gated clients are inert when unconfigured

Run: LEADGEN_SMTP_VERIFY=0 python3 -m pytest tests/test_enrichment_upgrades.py -q
"""

import os

# Force live SMTP off so these tests never touch the network.
os.environ.setdefault("LEADGEN_SMTP_VERIFY", "0")

import pytest


# ── email_verify: classification ─────────────────────────────────────────

from apps.api.services.leadgen.enrichment import email_verify as ev


@pytest.mark.parametrize("email,attr", [
    ("info@acme.com", "is_role"),
    ("sales@acme.com", "is_role"),
    ("someone@gmail.com", "is_free"),
    ("user@mailinator.com", "is_disposable"),
])
def test_classify_flags(email, attr):
    assert getattr(ev.classify_email(email), attr) is True


def test_classify_personal_corporate():
    c = ev.classify_email("john.doe@acme.com")
    assert c.valid_syntax and c.is_personal_corporate
    assert not (c.is_role or c.is_free or c.is_disposable)


def test_classify_invalid_syntax():
    c = ev.classify_email("not-an-email")
    assert not c.valid_syntax and not c.is_personal_corporate


def test_classifier_data_sets_loaded():
    assert len(ev._role_accounts()) > 100
    assert len(ev._free_providers()) > 1000
    assert len(ev._disposable_domains()) > 10000


def test_smtp_disabled_never_false_rejects():
    # With SMTP off, probe is inert: unknown results, not rejects.
    probe = ev.probe_domain("example.com", ["a@example.com"], timeout=2)
    assert probe.reachable is False
    assert probe.results == {"a@example.com": None}


def test_pick_best_degrades_to_pattern_when_unreachable():
    # MX-backed fallback returns the top candidate as a "pattern" guess.
    res = ev.pick_best_email(["john.doe@example.com", "jdoe@example.com"])
    assert res.confidence in ("pattern", "")  # depends on MX presence; never smtp_verified offline
    assert res.deliverable is None


# ── jsonld_firmographics ─────────────────────────────────────────────────

from apps.api.services.leadgen.enrichment.providers.jsonld_firmographics import extract_firmographics


def test_jsonld_full_organization():
    html = (
        '<script type="application/ld+json">'
        '{"@type":"Organization","name":"Acme Staffing","telephone":"+91-80-12345678",'
        '"email":"hr@acme.in","foundingDate":"2011-06-01",'
        '"address":{"@type":"PostalAddress","streetAddress":"12 MG Rd","addressLocality":"Bangalore","addressRegion":"Karnataka"},'
        '"sameAs":["https://www.linkedin.com/company/acme","https://twitter.com/acme","https://facebook.com/acme"]}'
        '</script>'
    )
    f = extract_firmographics(html)
    assert f["company"] == "Acme Staffing"
    assert f["phone"] == "+91-80-12345678"
    assert f["email"] == "hr@acme.in"
    assert f["founded_year"] == "2011"
    assert f["city"] == "Bangalore" and f["state"] == "Karnataka"
    assert "linkedin.com/company/acme" in f["linkedin_url"]
    assert "twitter.com/acme" in f["twitter_url"]
    assert "facebook.com/acme" in f["facebook_url"]


def test_jsonld_graph_and_array_picks_org():
    html = (
        '<script type="application/ld+json">'
        '[{"@type":"WebSite","name":"site"},{"@type":"LocalBusiness","name":"Beta Tech","telephone":"080-999"}]'
        '</script>'
    )
    f = extract_firmographics(html)
    assert f["company"] == "Beta Tech" and f["phone"] == "080-999"


def test_jsonld_opengraph_fallback():
    html = '<meta property="og:site_name" content="OG Co"><meta property="og:description" content="we do things">'
    f = extract_firmographics(html)
    assert f["company"] == "OG Co" and f["description"] == "we do things"


@pytest.mark.parametrize("html", [
    "<html>nothing structured here</html>",
    '<script type="application/ld+json">{bad json}</script>',
    "",
])
def test_jsonld_garbage_safe(html):
    assert extract_firmographics(html) == {}


# ── tech_stack: full DB + implies ────────────────────────────────────────

from apps.api.services.leadgen.enrichment.providers.tech_stack_provider import (
    detect_tech_from_response, _get_compiled,
)


def test_tech_db_is_large():
    # License-clean MIT bundle (developit/wappalyzer, pinned) — see
    # data/tech_fingerprints.NOTICE. Smaller than the prior unattributed GPL-era
    # blob but provably-permissive + reproducible (scripts/build_tech_fingerprints.py).
    assert len(_get_compiled()) > 1500


def test_tech_detect_and_implies():
    names = {t["name"] for t in detect_tech_from_response(
        {"server": "cloudflare"}, "wp-content/ woocommerce", [],
    )}
    assert "WordPress" in names
    assert "WooCommerce" in names
    assert "Cloudflare" in names
    assert "PHP" in names  # implied by WordPress/WooCommerce


def test_tech_detect_headers_and_cookies():
    names = {t["name"] for t in detect_tech_from_response(
        {"x-powered-by": "PHP/8.1"}, "", ["laravel_session"],
    )}
    assert "PHP" in names and "Laravel" in names


def test_tech_detect_empty():
    assert detect_tech_from_response({}, "", []) == []


# ── registry + waterfall wiring ──────────────────────────────────────────

def test_new_providers_registered():
    import apps.api.services.workbook.providers as p
    assert {"jsonld_firmographics", "staffspy", "tech_stack", "mailscout"} <= set(p._registry)


def test_providers_wired_into_waterfalls():
    from apps.api.services.workbook.enrichment import DEFAULT_WATERFALLS
    assert "jsonld_firmographics" in DEFAULT_WATERFALLS["email"]
    assert "jsonld_firmographics" in DEFAULT_WATERFALLS["address"]
    assert DEFAULT_WATERFALLS["linkedin_url"][0] == "jsonld_firmographics"
    assert "staffspy" in DEFAULT_WATERFALLS["decision_makers"]


# ── gated clients inert by default ───────────────────────────────────────

def test_gated_clients_inert():
    from apps.api.services.leadgen.scrapers import flaresolverr_client as fs
    from apps.api.services.leadgen.scrapers import gmaps_service_client as gm
    from apps.api.services.leadgen.enrichment import content_extract as ce
    from apps.api.services.leadgen.enrichment.providers import staffspy_provider as ss
    # No env/deps configured in CI → all must report unavailable, not crash.
    assert fs.is_available() is False
    assert gm.is_available() is False
    assert ss.is_available() is False
    # content_extract depends only on an optional import; just must not raise.
    assert ce.is_available() in (True, False)
