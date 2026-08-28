"""Regression tests for domain-safe collection intent routing."""

from apps.api.services.leadgen.collection_intent import (
    decide_collection,
    domain_in_query,
    extract_explicit_people_request,
    normalize_domain,
)


def test_bare_domain_requires_clarification_and_cannot_collect():
    decision = decide_collection("stripe.com")

    assert decision.domain == "stripe.com"
    assert decision.clarification_required is True
    assert decision.can_collect is False
    assert {option.intent for option in decision.options} == {
        "company_research",
        "people_at_company",
        "technology_users",
        "signal_monitor",
    }


def test_domain_url_is_normalized_and_still_blocked_from_market_collector():
    assert normalize_domain("https://www.stripe.com/") == "stripe.com"
    assert domain_in_query("Research https://stripe.com/pricing") == "stripe.com"
    assert decide_collection(
        "https://stripe.com", requested_intent="market_search"
    ).clarification_required is True


def test_explicit_market_query_can_collect():
    decision = decide_collection("payment processing companies in Pune")

    assert decision.intent == "market_search"
    assert decision.can_collect is True
    assert decision.clarification_required is False


def test_specialized_domain_requests_route_without_collecting():
    for query, intent in (
        ("find decision makers at stripe.com", "people_at_company"),
        ("find companies using stripe.com", "technology_users"),
        ("monitor stripe.com", "signal_monitor"),
        ("research stripe.com", "company_research"),
    ):
        decision = decide_collection(query)
        assert decision.intent == intent
        assert decision.clarification_required is True
        assert decision.can_collect is False


def test_specialized_request_cannot_be_forced_into_market_search():
    decision = decide_collection(
        "find companies using Stripe",
        requested_intent="market_search",
    )
    assert decision.intent == "technology_users"
    assert decision.can_collect is False


def test_company_research_without_domain_routes_to_chat():
    decision = decide_collection("research Stripe")
    assert decision.intent == "company_research"
    assert decision.clarification_required is True
    assert decision.options[0].route == "/chat"


def test_named_company_team_request_gets_three_specialized_workflows():
    decision = decide_collection("Stripe partnership teams")

    assert decision.intent == "people_at_company"
    assert decision.entity == "Stripe"
    assert decision.clarification_kind == "company_team"
    assert decision.clarification_required is True
    assert decision.can_collect is False
    assert [option.key for option in decision.options] == [
        "team_people", "partner_ecosystem", "team_overview",
    ]
    assert len({option.key for option in decision.options}) == 3
    assert "similarly named companies" in decision.options[0].draft


def test_named_company_team_request_cannot_be_forced_into_collection():
    decision = decide_collection(
        "Stripe partnership teams", requested_intent="market_search"
    )
    assert decision.clarification_required is True
    assert decision.can_collect is False


def test_generic_market_phrase_is_not_misread_as_named_company_team():
    decision = decide_collection("fintech sales companies in London")
    assert decision.intent == "market_search"
    assert decision.can_collect is True


def test_people_workflow_draft_is_parseable_and_never_market_collection():
    draft = decide_collection("Stripe partnership teams").options[0].draft
    request = extract_explicit_people_request(draft)

    assert request is not None
    assert request.entity == "Stripe"
    assert request.function == "partnership"
    assert decide_collection(draft).intent == "people_at_company"
    assert decide_collection(draft).can_collect is False
