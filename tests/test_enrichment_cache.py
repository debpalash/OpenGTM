"""
Phase 2: cross-provider canonical-key enrichment cache.
See docs/clay-alternatives-ingestion-catalog.md (top-10 #2).
"""
import asyncio
import os
import tempfile

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.enrichment import cache as cache_mod
from apps.api.services.leadgen.enrichment.cache import canonical_key, EnrichmentCache
from apps.api.services.leadgen.enrichment.provider import (
    WaterfallEnricher, EnrichmentProvider, EnrichmentResult,
)


# ── canonical_key ─────────────────────────────────────────────────

def test_canonical_key_stable_across_company_variants():
    a = Lead(company="SpaceX Inc.", website="https://www.spacex.com/about", contact_person="Elon Musk")
    b = Lead(company="SpaceX", website="spacex.com", contact_person="elon  musk")
    assert canonical_key("email", a) == canonical_key("email", b)

def test_canonical_key_person_vs_company_scope():
    lead = Lead(company="SpaceX", website="spacex.com", contact_person="Elon Musk")
    assert canonical_key("email", lead) != canonical_key("company_size", lead)

def test_canonical_key_none_without_identity():
    # person lookup needs a name + org
    assert canonical_key("email", Lead(company="SpaceX")) is None
    # company lookup needs a domain/company
    assert canonical_key("company_size", Lead()) is None
    assert canonical_key("company_size", Lead(website="spacex.com")) is not None


# ── cache store/get + TTL ─────────────────────────────────────────

def _temp_cache():
    path = os.path.join(tempfile.mkdtemp(), "c.db")
    return EnrichmentCache(db_path=path)

def test_cache_set_get_roundtrip():
    c = _temp_cache()
    c.set("k1", "email", "a@x.com", confidence=0.9, provider="apollo")
    hit = c.get("k1", "email")
    assert hit and hit["value"] == "a@x.com" and hit["provider"] == "apollo"
    assert c.get("k1", "phone") is None
    assert c.get("nope", "email") is None

def test_cache_expiry():
    c = _temp_cache()
    c.set("k2", "email", "a@x.com", ttl_days=-1)  # already expired
    assert c.get("k2", "email") is None

def test_cache_ignores_empty():
    c = _temp_cache()
    c.set("k3", "email", "", confidence=0.5)
    assert c.get("k3", "email") is None


# ── waterfall integration ─────────────────────────────────────────

class _CountingProvider(EnrichmentProvider):
    def __init__(self, name, value, conf=0.9):
        self.name = name; self.capabilities = ["email"]; self.default_confidence = conf
        self._v = value; self._c = conf; self.calls = 0
    async def enrich(self, lead):
        self.calls += 1
        return EnrichmentResult(success=True, fields={"email": self._v}, confidence=self._c)


def test_second_run_hits_cache_and_skips_providers(monkeypatch, tmp_path):
    # point the module singleton at a temp DB
    cache_mod._cache = EnrichmentCache(db_path=str(tmp_path / "wf.db"))
    lead = Lead(company="SpaceX", website="spacex.com", contact_person="Elon Musk")

    p = _CountingProvider("apollo", "elon@spacex.com")
    w = WaterfallEnricher()
    w.register_chain("email", [p])

    res1, _ = asyncio.run(w.enrich(lead))
    assert res1["email"] == "elon@spacex.com" and p.calls == 1

    # a fresh lead with the SAME identity → cache hit, provider NOT called again
    p2 = _CountingProvider("apollo", "elon@spacex.com")
    w2 = WaterfallEnricher()
    w2.register_chain("email", [p2])
    lead_same = Lead(company="SpaceX Inc", website="https://spacex.com", contact_person="Elon Musk")
    res2, logs2 = asyncio.run(w2.enrich(lead_same))
    assert res2["email"] == "elon@spacex.com"
    assert p2.calls == 0  # served from cross-provider cache
    assert logs2[0].winner.startswith("cache")
    cache_mod._cache = None  # reset singleton


def test_cache_disabled_flag(tmp_path):
    cache_mod._cache = EnrichmentCache(db_path=str(tmp_path / "wf2.db"))
    lead = Lead(company="SpaceX", website="spacex.com", contact_person="Elon Musk")
    p = _CountingProvider("apollo", "elon@spacex.com")
    w = WaterfallEnricher(cache_enabled=False)
    w.register_chain("email", [p])
    asyncio.run(w.enrich(lead))
    p2 = _CountingProvider("apollo", "elon@spacex.com")
    w2 = WaterfallEnricher(cache_enabled=False)
    w2.register_chain("email", [p2])
    asyncio.run(w2.enrich(lead))
    assert p2.calls == 1  # caching off → provider called again
    cache_mod._cache = None
