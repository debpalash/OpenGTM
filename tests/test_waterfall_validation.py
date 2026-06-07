"""
Phase-1 waterfall upgrades: output validation accept-gate + best-of-N +
confidence early-exit. See docs/clay-alternatives-ingestion-catalog.md.
"""
import asyncio

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.enrichment.provider import (
    WaterfallEnricher, EnrichmentProvider, EnrichmentResult,
)
from apps.api.services.leadgen.enrichment.validate import (
    is_valid_email, is_valid_linkedin, is_valid_value, validate_field,
)


# ── validate.py ──────────────────────────────────────────────────

def test_email_accepts_real_corporate():
    assert is_valid_email("elon@spacex.com", company_domain="spacex.com")

def test_email_rejects_role_mailbox():
    assert not is_valid_email("info@spacex.com")
    assert not is_valid_email("sales@spacex.com")
    assert is_valid_email("info@spacex.com", allow_role=True)

def test_email_rejects_placeholder_and_sentinel():
    assert not is_valid_email("test@example.com")
    assert not is_valid_email("no-results-found")
    assert not is_valid_email("not_found@domain.com")

def test_email_rejects_company_domain_mismatch_but_allows_freemail():
    assert not is_valid_email("elon@othercorp.com", company_domain="spacex.com")
    assert is_valid_email("elon@gmail.com", company_domain="spacex.com")

def test_email_domain_matches_through_www_and_path():
    assert is_valid_email("a@spacex.com", company_domain="https://www.spacex.com/about")

def test_linkedin_validation():
    assert is_valid_linkedin("https://www.linkedin.com/in/elonmusk")
    assert is_valid_linkedin("https://linkedin.com/company/spacex")
    assert not is_valid_linkedin("https://example.com/elon")
    assert not is_valid_linkedin("not a url")

def test_generic_value_rejects_sentinels():
    assert is_valid_value("Acme Corp")
    assert not is_valid_value("")
    assert not is_valid_value("N/A")
    assert not is_valid_value("unknown")

def test_validate_field_dispatch():
    assert validate_field("email", "x@acme.com", company_domain="acme.com")
    assert not validate_field("email", "info@acme.com")
    assert validate_field("phone", "+1 555 123 4567")
    assert not validate_field("phone", "none")


# ── WaterfallEnricher behavior ───────────────────────────────────

class _StubProvider(EnrichmentProvider):
    def __init__(self, name, conf, value, field_name="email"):
        self.name = name
        self.default_confidence = conf
        self.capabilities = [field_name]
        self._value, self._conf, self._field = value, conf, field_name
        self.calls = 0

    async def enrich(self, lead):
        self.calls += 1
        return EnrichmentResult(success=True, fields={self._field: self._value}, confidence=self._conf)


def _run(coro):
    return asyncio.run(coro)


def test_rejects_garbage_and_falls_through():
    lead = Lead(company="SpaceX", website="spacex.com")
    junk = _StubProvider("junk", 0.9, "info@spacex.com")   # role → rejected despite high conf
    good = _StubProvider("good", 0.6, "elon@spacex.com")
    w = WaterfallEnricher()
    w.register_chain("email", [junk, good])
    res, logs = _run(w.enrich(lead))
    assert res.get("email") == "elon@spacex.com"
    assert logs[0].winner == "good"


def test_early_exit_skips_remaining_providers():
    lead = Lead(company="SpaceX", website="spacex.com")
    hi = _StubProvider("hi", 0.9, "a@spacex.com")
    lo = _StubProvider("lo", 0.5, "b@spacex.com")
    w = WaterfallEnricher()
    w.register_chain("email", [hi, lo])
    res, logs = _run(w.enrich(lead))
    assert res["email"] == "a@spacex.com"
    assert lo.calls == 0  # high-confidence result short-circuited the chain


def test_best_of_n_when_none_clear_threshold():
    lead = Lead(company="SpaceX", website="spacex.com")
    p1 = _StubProvider("p1", 0.5, "x@spacex.com")
    p2 = _StubProvider("p2", 0.8, "y@spacex.com")
    w = WaterfallEnricher()
    w.register_chain("email", [p1, p2])
    res, logs = _run(w.enrich(lead))
    assert res["email"] == "y@spacex.com"  # 0.8 beat 0.5
    assert p1.calls == 1 and p2.calls == 1  # both tried (neither >= 0.85)
