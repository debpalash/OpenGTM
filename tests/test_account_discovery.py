from apps.api.services.leadgen.account_discovery import (
    build_account_discovery_brief,
    evaluate_account_fit,
    extract_account_discovery_request,
)


QUERY = "Find 20 B2B SaaS companies in India that use Stripe and are hiring partnership roles."


def test_builds_structured_g1_brief_from_explicit_request():
    brief = build_account_discovery_brief(QUERY)

    assert brief["complete"] is True
    assert brief["requested_count"] == 20
    assert brief["company_types"] == ["B2B SaaS"]
    assert brief["geographies"] == ["India"]
    assert brief["technologies"] == ["Stripe"]
    assert brief["hiring_roles"] == ["partnership"]
    assert brief["missing_fields"] == []
    assert brief["brief_id"].startswith("accounts_")


def test_only_complete_explicit_market_requests_take_deterministic_route():
    assert extract_account_discovery_request(QUERY)["requested_count"] == 20
    assert extract_account_discovery_request("Stripe partnership teams") is None
    assert extract_account_discovery_request("Find B2B SaaS companies in India") is None


def test_account_fit_requires_evidence_for_every_requested_criterion():
    brief = build_account_discovery_brief(QUERY)
    lead = {
        "company": "Fixture SaaS",
        "website": "https://fixture.example",
        "source_url": "https://evidence.example/fixture-saas",
        "specialization": "B2B SaaS",
        "industry_tags": "Software",
        "city": "Bengaluru",
        "state": "Karnataka",
        "address": "Bengaluru, Karnataka, India",
        "technologies": "Stripe, HubSpot",
        "hiring_signals": '{"roles":["Director of Partnerships"]}',
        "score": 88,
        "updated_at": "2026-08-28T09:00:00+00:00",
    }

    result = evaluate_account_fit(lead, brief)

    assert result["accepted"] is True
    assert result["canonical_domain"] == "fixture.example"
    assert len(result["fit_reasons"]) == 4
    assert result["evidence_urls"] == [
        "https://evidence.example/fixture-saas",
        "https://fixture.example",
    ]
    assert result["field_confidence"] == 0.88


def test_account_fit_rejects_missing_technology_and_hiring_evidence():
    brief = build_account_discovery_brief(QUERY)
    lead = {
        "company": "Padded Result",
        "website": "https://padded.example",
        "source_url": "https://directory.example/padded",
        "specialization": "B2B SaaS",
        "address": "Mumbai, India",
        "description": "Payments company",
        "score": 90,
        "updated_at": "2026-08-28T09:00:00+00:00",
    }

    result = evaluate_account_fit(lead, brief)

    assert result["accepted"] is False
    assert result["rejection_reasons"] == [
        "technology_evidence_missing",
        "hiring_evidence_missing",
    ]


def test_website_alone_does_not_prove_technology_or_hiring_fit():
    brief = build_account_discovery_brief(QUERY)
    lead = {
        "company": "Stripe Named Site",
        "website": "https://stripe-users.example",
        "source_url": "https://stripe-users.example",
        "specialization": "B2B SaaS",
        "address": "Delhi, India",
        "score": 70,
        "created_at": "2026-08-28T09:00:00+00:00",
    }

    result = evaluate_account_fit(lead, brief)

    assert result["accepted"] is False
    assert "technology_evidence_missing" in result["rejection_reasons"]
    assert "hiring_evidence_missing" in result["rejection_reasons"]
