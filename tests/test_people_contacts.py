import asyncio

from apps.api.services.leadgen.enrichment.email_deliverability import (
    DeliverabilityResult,
)
from apps.api.services.leadgen import people_contacts as pc


def _person(**updates):
    person = {
        "person_id": "person_jane",
        "name": "Jane Valid",
        "title": "VP Partnerships",
        "linkedin_url": "https://www.linkedin.com/in/jane-valid",
        "canonical_company_domain": "stripe.com",
    }
    person.update(updates)
    return person


def _deliverability(status, confidence, *, source="smtp", **updates):
    verdict = DeliverabilityResult(
        email="jane.valid@stripe.com",
        status=status,
        confidence=confidence,
        source=source,
        verification_confidence=0.95 if status == "valid" else 0.5,
        verification_attempts=[{
            "provider": source,
            "status": status,
            "detail": "recorded_test",
        }],
    )
    for key, value in updates.items():
        setattr(verdict, key, value)
    return verdict


def _run(monkeypatch, provider_results, verdict):
    calls = []

    async def fake_provider(name, lead, *, timeout):
        calls.append((name, lead.contact_person, lead.website, timeout))
        return provider_results[name]

    async def fake_verify(email, *, workspace_id):
        assert email == "jane.valid@stripe.com"
        assert workspace_id == "W1"
        return verdict

    monkeypatch.setattr(pc, "_run_discovery_provider", fake_provider)
    monkeypatch.setattr(pc, "_verify_discovered_email", fake_verify)
    result = asyncio.run(pc.enrich_people_contacts(
        "Stripe",
        "partnerships",
        [_person()],
        company_resolution={
            "status": "resolved",
            "canonical_domain": "stripe.com",
        },
        person_ids=["person_jane"],
        workspace_id="W1",
        action_id="contact-action-1",
    ))
    return result, calls


def test_exact_person_email_requires_separate_valid_verdict(monkeypatch):
    result, calls = _run(monkeypatch, {
        "prospeo": {
            "success": True,
            "fields": {
                "email": "Jane.Valid@Stripe.com",
                "email_match_method": "linkedin",
                "email_verified": True,
            },
            "confidence": 0.9,
            "duration_ms": 12,
            "license": "proprietary-api",
        },
        "hunter_io": None,
    }, _deliverability("valid", "verified"))

    assert result["ok"] is True
    assert result["action_id"] == "contact-action-1"
    assert result["selected_person_ids"] == ["person_jane"]
    assert result["summary"]["verified"] == 1
    contact = result["people"][0]["contactability"]
    assert contact["email"] == "jane.valid@stripe.com"
    assert contact["status"] == "verified"
    assert [attempt["provider"] for attempt in contact["attempts"]] == [
        "prospeo", "smtp",
    ]
    assert calls == [("prospeo", "Jane Valid", "https://stripe.com", 20.0)]


def test_unknown_verdict_is_risky_even_if_finder_claims_verified(monkeypatch):
    result, _ = _run(monkeypatch, {
        "prospeo": {
            "success": True,
            "fields": {
                "email": "jane.valid@stripe.com",
                "email_match_method": "exact_name",
                "email_verified": True,
            },
            "confidence": 0.99,
        },
        "hunter_io": None,
    }, _deliverability("unknown", "unknown", source="verification_cascade"))

    contact = result["people"][0]["contactability"]
    assert contact["status"] == "risky"
    assert contact["verification_status"] == "unknown"
    assert result["summary"]["verified"] == 0
    assert result["summary"]["risky"] == 1


def test_catch_all_and_invalid_remain_distinct(monkeypatch):
    provider_results = {
        "prospeo": {
            "success": True,
            "fields": {
                "email": "jane.valid@stripe.com",
                "email_match_method": "exact_name",
            },
            "confidence": 0.8,
        },
        "hunter_io": None,
    }
    catch_all, _ = _run(
        monkeypatch,
        provider_results,
        _deliverability("catch_all", "risky", catch_all=True),
    )
    assert catch_all["people"][0]["contactability"]["status"] == "catch_all"
    assert catch_all["people"][0]["email"] == "jane.valid@stripe.com"

    invalid, _ = _run(
        monkeypatch,
        provider_results,
        _deliverability("invalid", ""),
    )
    assert invalid["people"][0]["contactability"]["status"] == "invalid"
    assert invalid["people"][0]["email"] is None
    assert invalid["people"][0]["contactability"]["email"] is None


def test_rejects_generic_or_wrong_person_results_and_records_exhaustion(monkeypatch):
    result, calls = _run(monkeypatch, {
        "prospeo": {
            "success": True,
            "fields": {
                "email": "other.person@stripe.com",
                "email_match_method": "exact_name",
                "contact_person": "Other Person",
            },
            "confidence": 0.9,
        },
        "hunter_io": {
            "success": True,
            "fields": {
                "email": "best.contact@stripe.com",
                "email_match_method": "domain_search",
                "contact_person": "Best Contact",
            },
            "confidence": 0.95,
        },
    }, _deliverability("valid", "verified"))

    contact = result["people"][0]["contactability"]
    assert contact["status"] == "unavailable"
    assert contact["exhausted"] is True
    assert [attempt["detail"] for attempt in contact["attempts"]] == [
        "person_identity_mismatch", "non_exact_match_method",
    ]
    assert [call[0] for call in calls] == ["prospeo", "hunter_io"]


def test_rejects_non_company_email_domain(monkeypatch):
    result, _ = _run(monkeypatch, {
        "prospeo": {
            "success": True,
            "fields": {
                "email": "jane.valid@gmail.com",
                "email_match_method": "linkedin",
            },
            "confidence": 0.9,
        },
        "hunter_io": {"success": False, "fields": {}, "error": "no data"},
    }, _deliverability("valid", "verified"))

    contact = result["people"][0]["contactability"]
    assert contact["status"] == "unavailable"
    assert contact["attempts"][0]["detail"] == "company_domain_mismatch"


def test_unresolved_company_blocks_spend_and_stays_unavailable(monkeypatch):
    async def unexpected_provider(*args, **kwargs):
        raise AssertionError("provider should not run without canonical company identity")

    monkeypatch.setattr(pc, "_run_discovery_provider", unexpected_provider)
    result = asyncio.run(pc.enrich_people_contacts(
        "Acme",
        "partnerships",
        [_person(canonical_company_domain="")],
        company_resolution={"status": "ambiguous", "canonical_domain": ""},
        workspace_id="W1",
    ))

    contact = result["people"][0]["contactability"]
    assert contact["status"] == "unavailable"
    assert contact["attempts"] == [{
        "stage": "precondition",
        "provider": "company_identity",
        "status": "blocked",
        "detail": "canonical_company_unresolved",
        "source_license": "unknown",
    }]


def test_unknown_selection_fails_before_provider_calls(monkeypatch):
    async def unexpected_provider(*args, **kwargs):
        raise AssertionError("provider should not run for unknown people")

    monkeypatch.setattr(pc, "_run_discovery_provider", unexpected_provider)
    result = asyncio.run(pc.enrich_people_contacts(
        "Stripe",
        "partnerships",
        [_person()],
        company_resolution={"status": "resolved", "canonical_domain": "stripe.com"},
        person_ids=["person_missing"],
        workspace_id="W1",
    ))

    assert result == {
        "ok": False,
        "error": "Unknown people selection",
        "unknown_person_ids": ["person_missing"],
    }
