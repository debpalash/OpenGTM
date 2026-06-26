"""Website technographics — fingerprint matching, SSRF-guarded homepage fetch,
domain cache, structured persistence, flag gating, license NOTICE, and the
poller new_tech_adopted(source=website) diff.

No network: the homepage fetch is always mocked; fingerprints use the bundled DB.
"""
import asyncio
import json
import re
from pathlib import Path
from types import SimpleNamespace

import pytest

from apps.api.core.config import settings
from apps.api.core.url_guard import BlockedUrlError
from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.enrichment.cache import EnrichmentCache
from apps.api.services.leadgen.enrichment.providers import tech_stack_provider as ts


# ── helpers ──────────────────────────────────────────────────────────────────

def _run(coro):
    return asyncio.run(coro)


def _mock_safe_get(monkeypatch, *, headers=None, html="", cookies=None, counter=None):
    async def fake(url):
        if counter is not None:
            counter["n"] += 1
        return (headers or {}, html, cookies or [])
    monkeypatch.setattr(ts, "_safe_get", fake)


def _fresh_cache(monkeypatch, tmp_path):
    cache = EnrichmentCache(db_path=str(tmp_path / "cache.db"))
    monkeypatch.setattr(ts, "get_cache", lambda: cache)
    return cache


# ── 1. fingerprint matching (AC referencing headers/html/meta/cookies) ───────

def test_detect_from_html():
    techs = ts.detect_tech_from_response({}, "<div>powered by wp-content/themes</div>", [])
    names = {t["name"] for t in techs}
    assert "WordPress" in names


def test_detect_from_headers():
    techs = ts.detect_tech_from_response({"Server": "cloudflare", "CF-RAY": "abc"}, "", [])
    assert "Cloudflare" in {t["name"] for t in techs}


def test_detect_from_meta():
    html = '<meta name="generator" content="Drupal 9 (https://drupal.org)">'
    assert "Drupal" in {t["name"] for t in ts.detect_tech_from_response({}, html, [])}


def test_detect_from_cookies():
    techs = ts.detect_tech_from_response({}, "", ["_shopify_s"])
    assert "Shopify" in {t["name"] for t in techs}


def test_implies_resolution_and_confidence():
    # Concrete CMS (bundled DB) implies PHP — exercises transitive implies.
    techs = ts.detect_tech_from_response({}, "<script src='/concrete/js/app.js'></script>", [])
    by = {t["name"]: t for t in techs}
    assert "Concrete CMS" in by
    assert by["Concrete CMS"]["confidence"] == ts._CONF_DIRECT
    # implied techs carry the lower confidence
    assert "PHP" in by and by["PHP"]["confidence"] == ts._CONF_IMPLIED


def test_no_detection_returns_empty():
    assert ts.detect_tech_from_response({}, "<html>nothing here</html>", []) == []


# ── 2. SSRF guard blocks internal/private URLs (no fetch) ────────────────────

@pytest.mark.parametrize("bad", [
    "http://127.0.0.1/", "http://localhost/", "http://169.254.169.254/latest/",
    "http://10.0.0.5/", "http://192.168.1.1/", "http://2130706433/",
])
def test_safe_get_blocks_private(monkeypatch, bad):
    calls = {"n": 0}

    class _Boom:
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return False
        async def get(self, *a, **k):
            calls["n"] += 1
            raise AssertionError("network must not be hit for a blocked URL")

    import httpx
    monkeypatch.setattr(httpx, "AsyncClient", lambda *a, **k: _Boom())
    with pytest.raises(BlockedUrlError):
        _run(ts._safe_get(bad))
    assert calls["n"] == 0


def test_enrich_blocked_url_no_tech(monkeypatch):
    monkeypatch.setattr(settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", True, raising=False)
    monkeypatch.setattr(settings, "TECH_STACK_RESPECT_ROBOTS", False, raising=False)

    async def boom(url):
        raise BlockedUrlError("ip '127.0.0.1' is private/blocked")
    monkeypatch.setattr(ts, "_safe_get", boom)

    res = _run(ts.TechStackProvider().enrich(Lead(company="X", website="http://127.0.0.1")))
    assert not res.success and res.error.startswith("blocked_url")


# ── 3. flag OFF ⇒ no fetch, byte-identical disabled result ───────────────────

def test_flag_off_no_network(monkeypatch):
    monkeypatch.setattr(settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", False, raising=False)

    async def boom(url):
        raise AssertionError("must not fetch when flag is OFF")
    monkeypatch.setattr(ts, "_safe_get", boom)

    res = _run(ts.TechStackProvider().enrich(Lead(company="X", website="https://example.com")))
    assert not res.success and res.error == "website_fetch_disabled"


# ── 4. structured technographics + comma-string back-compat ──────────────────

def test_enrich_populates_both_fields(monkeypatch, tmp_path):
    monkeypatch.setattr(settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", True, raising=False)
    monkeypatch.setattr(settings, "TECH_STACK_RESPECT_ROBOTS", False, raising=False)
    _fresh_cache(monkeypatch, tmp_path)
    _mock_safe_get(monkeypatch, headers={"Server": "cloudflare", "CF-RAY": "x"},
                   html="<div>wp-content/</div>")

    res = _run(ts.TechStackProvider().enrich(Lead(company="X", website="https://acme.com")))
    assert res.success
    # legacy flat comma string preserved
    assert "technologies" in res.fields and "WordPress" in res.fields["technologies"]
    # structured column populated with name/category/source/confidence
    structured = json.loads(res.fields["technographics"])
    assert all({"name", "category", "source", "confidence"} <= set(e) for e in structured)
    assert all(e["source"] == "website" for e in structured)
    assert "Cloudflare" in {e["name"] for e in structured}


# ── 5. cache hit avoids refetch (90d TTL) ────────────────────────────────────

def test_cache_hit_avoids_refetch(monkeypatch, tmp_path):
    monkeypatch.setattr(settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", True, raising=False)
    monkeypatch.setattr(settings, "TECH_STACK_RESPECT_ROBOTS", False, raising=False)
    _fresh_cache(monkeypatch, tmp_path)
    counter = {"n": 0}
    _mock_safe_get(monkeypatch, html="<div>wp-content/</div>", counter=counter)

    prov = ts.TechStackProvider()
    a = _run(prov.enrich(Lead(company="X", website="https://acme.com/about")))
    b = _run(prov.enrich(Lead(company="X2", website="https://www.acme.com/team")))
    assert a.success and b.success
    assert counter["n"] == 1  # second lookup (same domain) served from cache


def test_cache_set_uses_90day_ttl(monkeypatch, tmp_path):
    monkeypatch.setattr(settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", True, raising=False)
    monkeypatch.setattr(settings, "TECH_STACK_RESPECT_ROBOTS", False, raising=False)
    cache = _fresh_cache(monkeypatch, tmp_path)
    recorded = {}
    orig_set = cache.set
    def spy(ck, fn, value, **kw):
        recorded.update(kw)
        return orig_set(ck, fn, value, **kw)
    monkeypatch.setattr(cache, "set", spy)
    _mock_safe_get(monkeypatch, html="<div>wp-content/</div>")
    _run(ts.TechStackProvider().enrich(Lead(company="X", website="https://acme.com")))
    assert recorded.get("ttl_days") == 90


# ── 6. license NOTICE + DB integrity ─────────────────────────────────────────

def test_notice_file_present_and_attributes_mit():
    notice = ts._DB_PATH.parent / "tech_fingerprints.NOTICE"
    assert notice.exists(), "tech_fingerprints.NOTICE must ship beside the blob"
    text = notice.read_text()
    assert "MIT" in text
    assert "developit/wappalyzer" in text
    assert "b502633885dfea2221ae0e87a275a8120072afa7" in text  # pinned commit


def test_fingerprint_db_parses_and_compiles():
    db = json.loads(ts._DB_PATH.read_text())
    assert len(db) > 1000
    # every pattern in every entry must compile (no re.error in the bundle)
    for name, spec in db.items():
        for p in spec.get("html", []):
            re.compile(p, re.I)
        for p in spec.get("headers", {}).values():
            re.compile(p or ".", re.I)
        for p in spec.get("meta", {}).values():
            re.compile(p or ".", re.I)


# ── 7. poller new_tech_adopted(source=website) emits once + dedups ───────────

class _FakeProvider:
    def __init__(self, techs):
        self._techs = techs
    async def enrich(self, lead):
        return SimpleNamespace(
            success=True, error="",
            fields={"technographics": json.dumps(self._techs)},
        )


def _watch(cursor=None):
    return SimpleNamespace(id=1, target="Acme", resolved_cik=None,
                           cursor={"bootstrapped": True} if cursor is None else cursor)


def test_web_tech_emits_once_and_dedups(monkeypatch):
    from apps.api.services.poller import sources
    monkeypatch.setattr(settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", True, raising=False)
    techs = [
        {"name": "WordPress", "category": "CMS", "source": "website", "confidence": 0.75},
        {"name": "Cloudflare", "category": "CDN", "source": "website", "confidence": 0.75},
    ]
    prov = _FakeProvider(techs)

    watch = _watch()
    events, patch = sources.fetch_web_tech(watch, "https://acme.com", backfill=False, provider=prov)
    assert {e.signal_type for e in events} == {"new_tech_adopted"}
    assert all(e.source == "website" for e in events)
    assert len(events) == 2
    assert "known_web_tech" in patch and len(patch["known_web_tech"]) == 2

    # second poll with the advanced cursor → no new events (dedup)
    watch2 = _watch(cursor={"bootstrapped": True, "known_web_tech": patch["known_web_tech"]})
    events2, patch2 = sources.fetch_web_tech(watch2, "https://acme.com", backfill=False, provider=prov)
    assert events2 == []


def test_web_tech_bootstrap_suppresses_emit(monkeypatch):
    from apps.api.services.poller import sources
    monkeypatch.setattr(settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", True, raising=False)
    prov = _FakeProvider([{"name": "WordPress", "category": "CMS"}])
    watch = _watch(cursor={})  # not bootstrapped
    events, patch = sources.fetch_web_tech(watch, "https://acme.com", backfill=False, provider=prov)
    assert events == []                     # suppressed on bootstrap
    assert patch["known_web_tech"] == ["wordpress"] or len(patch["known_web_tech"]) == 1


def test_web_tech_no_website_is_noop(monkeypatch):
    from apps.api.services.poller import sources
    monkeypatch.setattr(settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", True, raising=False)
    events, patch = sources.fetch_web_tech(_watch(), None, backfill=False)
    assert events == [] and patch == {}


def test_web_tech_flag_off_is_noop(monkeypatch):
    from apps.api.services.poller import sources
    monkeypatch.setattr(settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", False, raising=False)
    events, patch = sources.fetch_web_tech(_watch(), "https://acme.com", backfill=False)
    assert events == [] and patch == {}


# ── 8. engine selects exactly one tech path (no double-count) ─────────────────

def test_engine_selects_one_tech_path(monkeypatch):
    from apps.api.services.poller import engine
    w = SimpleNamespace(kind="company", signal_types=["new_tech_adopted"])

    monkeypatch.setattr(settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", False, raising=False)
    assert "tech" in engine._source_set(w) and "web_tech" not in engine._source_set(w)

    monkeypatch.setattr(settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", True, raising=False)
    assert "web_tech" in engine._source_set(w) and "tech" not in engine._source_set(w)
