"""Regression tests for source-document rejection and provenance."""

from apps.api.services.leadgen.db import LeadDB
from apps.api.services.leadgen.lead_validator import validate_lead
from apps.api.services.leadgen.models import Lead


def test_technology_list_page_is_not_a_lead():
    lead = Lead(
        company="128 Companies That Use Stripe",
        website="https://trends.builtwith.com/websitelist/Stripe",
        source="web_search",
    )

    valid, reason = validate_lead(lead)

    assert valid is False
    assert reason == "article_title_pattern"


def test_publisher_and_job_pages_are_evidence_not_company_websites():
    for website in (
        "https://jobs.theirstack.com/company/stripe",
        "https://www.bayt.com/en/company/stripe/jobs/",
        "https://economictimes.indiatimes.com/topic/stripe",
    ):
        lead = Lead(company="Stripe", website=website)
        valid, reason = validate_lead(lead)
        assert valid is False
        assert reason == "no_contact_data"
        assert lead.website == ""


def test_contact_from_unrelated_page_is_removed():
    lead = Lead(
        company="Barcelino",
        website="https://twitter.com/unrelated-profile",
        email="hello@barcelino.com",
    )

    valid, reason = validate_lead(lead)

    assert valid is False
    assert reason == "no_contact_data"
    assert lead.email == ""
    assert lead.website == ""


def test_real_company_with_attributable_contact_passes():
    lead = Lead(
        company="Acme Labs",
        website="https://www.acme.example/about",
        email="sales@acme.example",
    )

    valid, reason = validate_lead(lead)

    assert valid is True
    assert reason == ""
    assert lead.website == "https://www.acme.example"


def test_job_association_does_not_overwrite_source_provenance(tmp_path):
    db = LeadDB(str(tmp_path / "leads.db"))
    lead = Lead(
        company="Acme Labs",
        website="https://acme.example",
        source="web_search",
        source_url="https://example-search.test/result/acme",
        collection_job_id="job-123",
        score=45,
        score_tier="cold",
    )
    db.upsert_lead(lead)

    stored = db.get_job_leads("job-123")
    queried = db.get_leads(collection_job_id="job-123")
    db.close()

    assert stored[0]["source"] == "web_search"
    assert stored[0]["source_url"] == "https://example-search.test/result/acme"
    assert queried[0].source == "web_search"
