"""
Offline tests for the enrichment bundle (PR: feat/enrich-people-and-email).

Covers, with all network mocked (LinkedIn/DNS/SMTP/Hunter/Snov):
  • email-finder waterfall — provider order, stop-on-success, provenance, no-op
    when a lead already has an email, graceful no-op with no API keys.
  • email deliverability tagger — verified / risky(catch-all) / risky(role) /
    unknown / dropped(invalid/disposable) mapping.
  • decision-maker discovery — populates decision_makers and the score bonus is
    applied (re-score reflects the bonus).
"""
import asyncio
import json

import pytest

from apps.api.services.leadgen.models import Lead


# ── helpers ──────────────────────────────────────────────────────────────

def run(coro):
    return asyncio.run(coro)


# ════════════════════════════════════════════════════════════════════════
# Email-finder waterfall
# ════════════════════════════════════════════════════════════════════════

import apps.api.services.leadgen.enrichment.email_waterfall as ew


def _patch_chain(monkeypatch, *, website=None, hunter=None, snovio=None,
                 hunter_key=True, snovio_key=True):
    """Patch each finder + availability gate. A finder value of None = no email,
    an Exception class/instance = raise, otherwise a (email, extra) tuple."""
    async def _mk(value):
        if value is None:
            return ("", {})
        if isinstance(value, BaseException):
            raise value
        return value

    calls = []

    async def website_finder(lead, domain):
        calls.append("website")
        return await _mk(website)

    async def hunter_finder(lead, domain):
        calls.append("hunter")
        return await _mk(hunter)

    async def snovio_finder(lead, domain):
        calls.append("snovio")
        return await _mk(snovio)

    monkeypatch.setattr(ew, "_find_via_website", website_finder)
    monkeypatch.setattr(ew, "_find_via_hunter", hunter_finder)
    monkeypatch.setattr(ew, "_find_via_snovio", snovio_finder)
    monkeypatch.setattr(ew, "_hunter_available", lambda: hunter_key)
    monkeypatch.setattr(ew, "_snovio_available", lambda: snovio_key)
    return calls


def test_waterfall_stops_at_first_success(monkeypatch):
    calls = _patch_chain(monkeypatch, website=("hi@acme.com", {}),
                         hunter=("x@acme.com", {}))
    lead = Lead(company="Acme", website="https://acme.com")
    out = run(ew.find_email_waterfall(lead))

    assert out.email == "hi@acme.com"
    assert out.provider == "website"
    assert calls == ["website"]            # stopped before hunter/snov
    assert lead.email == "hi@acme.com"
    assert lead.email_provider == "website"


def test_waterfall_tries_providers_in_order(monkeypatch):
    # website yields nothing → hunter yields nothing → snovio wins
    calls = _patch_chain(monkeypatch, website=None, hunter=None,
                         snovio=("ceo@acme.com", {"contact_person": "Jane Roe"}))
    lead = Lead(company="Acme", website="https://acme.com")
    out = run(ew.find_email_waterfall(lead))

    assert calls == ["website", "hunter", "snovio"]   # strict order
    assert out.provider == "snovio"
    assert lead.email == "ceo@acme.com"
    assert lead.contact_person == "Jane Roe"          # bonus field carried over


def test_waterfall_records_provenance(monkeypatch):
    _patch_chain(monkeypatch, website=None, hunter=("found@acme.com", {}))
    lead = Lead(company="Acme", website="https://acme.com")
    run(ew.find_email_waterfall(lead))

    log = json.loads(lead.enrichment_waterfall)
    entry = next(e for e in log if e["field"] == "email_finder")
    assert entry["winner"] == "hunter"
    providers = [a["provider"] for a in entry["attempts"]]
    assert providers == ["website", "hunter"]
    assert lead.enrichment_attempts == 2              # website + hunter tried


def test_waterfall_noop_when_keys_absent(monkeypatch):
    # No API keys → hunter/snov skipped; website returns nothing → no email.
    calls = _patch_chain(monkeypatch, website=None,
                         hunter_key=False, snovio_key=False)
    lead = Lead(company="Acme", website="https://acme.com")
    out = run(ew.find_email_waterfall(lead))

    assert calls == ["website"]            # only the free step ran
    assert out.found is False
    assert lead.email == ""
    assert lead.email_provider == ""
    assert out.attempts == 1               # skipped providers don't count
    log = json.loads(lead.enrichment_waterfall)
    attempts = log[0]["attempts"]
    assert {a["provider"] for a in attempts if a.get("skipped")} == {"hunter", "snovio"}


def test_waterfall_skips_when_email_present(monkeypatch):
    calls = _patch_chain(monkeypatch, website=("override@acme.com", {}))
    lead = Lead(company="Acme", website="https://acme.com",
                email="existing@acme.com")
    out = run(ew.find_email_waterfall(lead))

    assert calls == []                     # no provider invoked
    assert out.found is False
    assert lead.email == "existing@acme.com"   # untouched


def test_waterfall_provider_error_falls_through(monkeypatch):
    calls = _patch_chain(monkeypatch, website=RuntimeError("boom"),
                         hunter=("ok@acme.com", {}))
    lead = Lead(company="Acme", website="https://acme.com")
    out = run(ew.find_email_waterfall(lead))

    assert calls == ["website", "hunter"]
    assert out.provider == "hunter"
    log = json.loads(lead.enrichment_waterfall)[0]
    web_attempt = next(a for a in log["attempts"] if a["provider"] == "website")
    assert "boom" in web_attempt["error"]


def test_waterfall_rejects_junk_email(monkeypatch):
    # website returns a junk address → not accepted, falls through to hunter
    calls = _patch_chain(monkeypatch, website=("noreply@acme.com", {}),
                         hunter=("real@acme.com", {}))
    lead = Lead(company="Acme", website="https://acme.com")
    out = run(ew.find_email_waterfall(lead))
    assert out.email == "real@acme.com"
    assert calls == ["website", "hunter"]


def test_batch_respects_budget(monkeypatch):
    _patch_chain(monkeypatch, website=("a@acme.com", {}))
    leads = [Lead(company=f"C{i}", website=f"https://c{i}.com") for i in range(5)]
    run(ew.enrich_emails_waterfall(leads, limit=2))
    assert sum(1 for l in leads if l.email) == 2


# ════════════════════════════════════════════════════════════════════════
# Email deliverability tagger
# ════════════════════════════════════════════════════════════════════════

import apps.api.services.leadgen.enrichment.email_deliverability as deliv
import apps.api.services.leadgen.enrichment.email_verify_cascade as cascade


def _stub_cascade(monkeypatch, status):
    async def _verify(email, **kw):
        return cascade.VerifyResult(email, status, "stub", 0.9)
    monkeypatch.setattr(cascade, "verify_email", _verify)


def test_tag_verified(monkeypatch):
    _stub_cascade(monkeypatch, cascade.VALID)
    lead = Lead(company="Acme", email="jane@acme.com")
    res = run(deliv.tag_email_confidence(lead))
    assert res.confidence == deliv.VERIFIED
    assert lead.email_confidence == "verified"


def test_tag_catch_all_is_risky(monkeypatch):
    _stub_cascade(monkeypatch, cascade.CATCH_ALL)
    lead = Lead(company="Acme", email="jane@acme.com")
    res = run(deliv.tag_email_confidence(lead))
    assert res.confidence == deliv.RISKY
    assert res.catch_all is True
    assert lead.email_confidence == "risky"


def test_tag_role_account_is_risky(monkeypatch):
    # A valid mailbox that is a role account (info@) → demoted to risky.
    _stub_cascade(monkeypatch, cascade.VALID)
    lead = Lead(company="Acme", email="info@acme.com")
    res = run(deliv.tag_email_confidence(lead))
    assert res.is_role is True
    assert res.confidence == deliv.RISKY
    assert lead.email_confidence == "risky"


def test_tag_unknown_when_inconclusive(monkeypatch):
    _stub_cascade(monkeypatch, cascade.UNKNOWN)
    lead = Lead(company="Acme", email="jane@acme.com")
    res = run(deliv.tag_email_confidence(lead))
    assert res.confidence == deliv.UNKNOWN
    assert lead.email_confidence == "unknown"


def test_tag_invalid_drops_email(monkeypatch):
    _stub_cascade(monkeypatch, cascade.INVALID)
    lead = Lead(company="Acme", email="jane@acme.com",
                email_provider="hunter", email_confidence="pattern")
    res = run(deliv.tag_email_confidence(lead))
    assert res.keep is False
    assert lead.email == ""               # known-bad address dropped
    assert lead.email_confidence == ""
    assert lead.email_provider == ""


def test_tag_disposable_dropped_without_network(monkeypatch):
    # Disposable detection is purely classification — force a disposable domain
    # and ensure the cascade is never consulted.
    import apps.api.services.leadgen.enrichment.email_verify as ev
    monkeypatch.setattr(ev, "classify_email", lambda e: ev.EmailClassification(
        email=e, valid_syntax=True, local_part="x", domain="trash.test",
        is_disposable=True))

    called = {"n": 0}
    async def _boom(email, **kw):
        called["n"] += 1
        return cascade.VerifyResult(email, cascade.VALID)
    monkeypatch.setattr(cascade, "verify_email", _boom)

    lead = Lead(company="Acme", email="x@trash.test")
    res = run(deliv.tag_email_confidence(lead))
    assert res.keep is False
    assert lead.email == ""
    assert called["n"] == 0                # no network call for disposable


def test_tag_noop_without_email():
    lead = Lead(company="Acme")
    assert run(deliv.tag_email_confidence(lead)) is None


# ════════════════════════════════════════════════════════════════════════
# Decision-maker discovery + score bonus
# ════════════════════════════════════════════════════════════════════════

import apps.api.services.leadgen.enrichment.decision_maker_finder as dmf
from apps.api.services.leadgen.scoring import score_lead


def test_decision_makers_populated(monkeypatch):
    async def _fake_search(company, title):
        if title == "CEO":
            return {"name": "Jane Roe", "title": "CEO",
                    "linkedin": "https://linkedin.com/in/janeroe"}
        return None
    monkeypatch.setattr(dmf, "_search_decision_maker", _fake_search)
    # don't actually sleep between titles
    async def _nosleep(*a, **k):
        return None
    monkeypatch.setattr(dmf.asyncio, "sleep", _nosleep)

    lead = Lead(company="Acme Corp", website="https://acme.com")
    run(dmf.find_decision_makers_for_lead(lead, max_contacts=1))

    dms = json.loads(lead.decision_makers)
    assert dms[0]["name"] == "Jane Roe"
    assert lead.contact_person == "Jane Roe"
    assert lead.contact_title == "CEO"


def test_decision_maker_score_bonus():
    # Same lead, with vs without decision makers — DM version must score higher
    # (scoring.score_lead rewards has_contact_person + multiple decision_makers).
    base = Lead(company="Acme Corp", website="https://acme.com",
                specialization="IT Staffing", city="Bangalore")
    enriched = Lead(company="Acme Corp", website="https://acme.com",
                    specialization="IT Staffing", city="Bangalore",
                    contact_person="Jane Roe", contact_title="CEO",
                    decision_makers=json.dumps([
                        {"name": "Jane Roe", "title": "CEO"},
                        {"name": "John Doe", "title": "CTO"},
                    ]))
    assert score_lead(enriched) > score_lead(base)


def test_linkedin_find_decision_makers_offline(monkeypatch):
    # scrapers.linkedin.find_decision_makers (the sync fallback the pipeline now
    # calls) must work against a mocked DDG client.
    import apps.api.services.leadgen.scrapers.linkedin as li

    class _FakeDDGS:
        def __enter__(self): return self
        def __exit__(self, *a): return False
        def text(self, query, max_results=2):
            return [{"href": "https://linkedin.com/in/janeroe",
                     "title": "Jane Roe - CEO at Acme | LinkedIn"}]

    monkeypatch.setattr(li, "get_ddgs", lambda: _FakeDDGS())
    monkeypatch.setattr(li.time, "sleep", lambda *a, **k: None)

    people = li.find_decision_makers("Acme", titles=["CEO"], max_results=2)
    assert people and people[0]["name"] == "Jane Roe"
    assert people[0]["title"] == "CEO"
