"""An upsert merges into the matching lead instead of overwriting it.

Regression coverage for the lead overwrite reports: a second contact at the same
company and city replaced the first contact, and every field the request left out
was reset to its default, including enrichment, status, score and notes.
"""

import uuid
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from apps.api.core.tenancy import WorkspaceCtx
from apps.api.routers import leads
from apps.api.services.leadgen.db import LeadDB
from apps.api.services.leadgen.lead_merge import LeadContactConflict
from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.store import PgLeadStore


@pytest.fixture(params=["sqlite_leaddb", "shared_tables_store"])
def store(request, tmp_path):
    """Both lead stores run the same merge rules."""
    if request.param == "sqlite_leaddb":
        db = LeadDB(str(tmp_path / "leads.db"))
        db.workspace = "w1"
        yield db
        db.close()
    else:
        s = PgLeadStore(f"merge-{uuid.uuid4().hex[:10]}")
        s.workspace = s.workspace_id
        yield s


def _lead(store, **fields):
    return Lead(workspace_id=store.workspace, **fields)


def _acme(store, **fields):
    base = dict(company="Acme", city="Austin", website="https://acme.example",
                contact_person="Alice Smith", email="alice@acme.example", phone="+1 555 0100",
                specialization="B2B SaaS", status="contacted", score=82, score_tier="hot",
                notes="called twice")
    base.update(fields)
    return _lead(store, **base)


def test_rediscovery_keeps_everything_it_does_not_mention(store):
    lead_id = store.upsert_lead(_acme(store))
    again = store.upsert_lead(_lead(store, company="Acme", city="Austin", source="rerun"))
    assert again == lead_id
    row = store.get_lead(lead_id)
    assert (row.website, row.email, row.phone, row.specialization) == (
        "https://acme.example", "alice@acme.example", "+1 555 0100", "B2B SaaS")
    assert (row.status, row.score, row.score_tier, row.notes) == ("contacted", 82, "hot", "called twice")
    assert row.source == "rerun"          # a provided value still wins


def test_provided_values_still_overwrite(store):
    lead_id = store.upsert_lead(_acme(store))
    store.upsert_lead(_lead(store, company="Acme", city="Austin", website="https://acme.io",
                            score=95, specialization="Fintech"))
    row = store.get_lead(lead_id)
    assert (row.website, row.score, row.specialization) == ("https://acme.io", 95, "Fintech")
    assert row.contact_person == "Alice Smith"


def test_placeholders_do_not_overwrite_real_values(store):
    lead_id = store.upsert_lead(_acme(store))
    store.upsert_lead(_lead(store, company="Acme", city="Austin", website="N/A", description="nan"))
    row = store.get_lead(lead_id)
    assert row.website == "https://acme.example" and row.description == ""


def test_a_different_contact_never_replaces_the_stored_one(store):
    lead_id = store.upsert_lead(_acme(store))
    # Bulk paths keep the stored contact and still take the company details.
    store.upsert_lead(_lead(store, company="Acme", city="Austin", contact_person="Bob Jones",
                            email="bob@acme.example", phone="+1 555 0199", contact_title="CTO",
                            description="Builds rockets"))
    row = store.get_lead(lead_id)
    assert (row.contact_person, row.email, row.phone, row.contact_title) == (
        "Alice Smith", "alice@acme.example", "+1 555 0100", "")
    assert row.description == "Builds rockets"


def test_reject_policy_raises_before_writing_anything(store):
    lead_id = store.upsert_lead(_acme(store))
    with pytest.raises(LeadContactConflict) as caught:
        store.upsert_lead(
            _lead(store, company="Acme", city="Austin", contact_person="Bob Jones",
                  website="https://changed.example"),
            on_contact_conflict="reject",
        )
    assert caught.value.lead_id == lead_id and caught.value.fields == ("contact_person",)
    row = store.get_lead(lead_id)
    assert row.contact_person == "Alice Smith" and row.website == "https://acme.example"


def test_same_contact_is_not_a_conflict(store):
    lead_id = store.upsert_lead(_acme(store))
    again = store.upsert_lead(
        _lead(store, company="Acme", city="Austin", contact_person="alice smith", contact_title="CEO"),
        on_contact_conflict="reject",
    )
    row = store.get_lead(lead_id)
    assert again == lead_id and row.contact_title == "CEO" and row.contact_person == "Alice Smith"


def test_the_fuller_form_of_a_name_is_kept(store):
    lead_id = store.upsert_lead(_acme(store, contact_person="Alice"))
    store.upsert_lead(_lead(store, company="Acme", city="Austin", contact_person="Alice Smith"))
    assert store.get_lead(lead_id).contact_person == "Alice Smith"
    store.upsert_lead(_lead(store, company="Acme", city="Austin", contact_person="A. Smith"))
    assert store.get_lead(lead_id).contact_person == "Alice Smith"


def test_a_new_email_for_the_same_person_drops_the_old_emails_provenance(store):
    lead_id = store.upsert_lead(_acme(store, email_confidence="smtp_verified", email_provider="hunter"))
    store.upsert_lead(_lead(store, company="Acme", city="Austin", contact_person="Alice Smith",
                            email="alice@new.example"))
    row = store.get_lead(lead_id)
    assert row.email == "alice@new.example"
    assert (row.email_confidence, row.email_provider) == ("", "")


def test_without_two_names_a_different_email_is_a_different_contact(store):
    store.upsert_lead(_acme(store))
    with pytest.raises(LeadContactConflict) as caught:
        store.upsert_lead(_lead(store, company="Acme", city="Austin", email="bob@acme.example"),
                          on_contact_conflict="reject")
    assert caught.value.fields == ("email",)


def test_a_contact_fills_a_lead_that_has_none(store):
    lead_id = store.upsert_lead(_lead(store, company="Acme", city="Austin", phone="+1 555 0100"))
    store.upsert_lead(_lead(store, company="Acme", city="Austin", contact_person="Alice Smith",
                            email="alice@acme.example"), on_contact_conflict="reject")
    row = store.get_lead(lead_id)
    assert (row.contact_person, row.email, row.phone) == ("Alice Smith", "alice@acme.example", "+1 555 0100")


def test_a_different_city_is_a_different_lead(store):
    first = store.upsert_lead(_acme(store))
    second = store.upsert_lead(_acme(store, city="Dallas", contact_person="Bob Jones"))
    assert first != second


def test_explicit_update_still_clears_a_field(store):
    lead_id = store.upsert_lead(_acme(store))
    store.update_lead_fields(lead_id, {"website": "", "notes": ""})
    row = store.get_lead(lead_id)
    assert row.website == "" and row.notes == "" and row.contact_person == "Alice Smith"


# ── The reported flow, through the HTTP API ───────────────────────────────────

@pytest.fixture
def api(monkeypatch, tmp_path):
    path = str(tmp_path / "leads.db")
    monkeypatch.setattr("apps.api.services.workspace.manager.workspace_leads_db_path", lambda slug: path)
    ctx = WorkspaceCtx(user=SimpleNamespace(id=1), workspace_id="W1", slug="one")
    app = FastAPI()
    app.include_router(leads.router)
    app.dependency_overrides[leads.require_editor] = lambda: ctx
    app.dependency_overrides[leads.current_workspace] = lambda: ctx
    return TestClient(app)


def _stored(api, lead_id):
    row = api.get(f"/api/lead/{lead_id}")
    assert row.status_code == 200, row.text
    return row.json()


def test_second_contact_is_rejected_and_the_lead_is_untouched(api):
    first = api.post("/api/lead", json={
        "company": "Overwrite repro", "city": "Repro City", "website": "https://example.invalid",
        "contact_person": "Person One", "email": "one@example.invalid"})
    assert first.status_code == 200 and first.json()["ok"] is True
    lead_id = first.json()["id"]
    assert api.put(f"/api/lead/{lead_id}", json={"specialization": "B2B SaaS", "phone": "+15555550100"}).status_code == 200

    second = api.post("/api/lead", json={
        "company": "Overwrite repro", "city": "Repro City",
        "contact_person": "Person Two", "email": "two@example.invalid"})
    assert second.status_code == 409
    detail = second.json()["detail"]
    assert detail["code"] == "contact_conflict" and detail["lead_id"] == lead_id
    assert detail["fields"] == ["contact_person"]

    row = _stored(api, lead_id)
    assert (row["contact_person"], row["email"]) == ("Person One", "one@example.invalid")
    assert (row["website"], row["phone"], row["specialization"]) == (
        "https://example.invalid", "+15555550100", "B2B SaaS")


def test_repeating_a_post_for_the_same_contact_keeps_omitted_fields(api):
    lead_id = api.post("/api/lead", json={
        "company": "Acme", "city": "Austin", "website": "https://acme.example",
        "contact_person": "Alice Smith", "email": "alice@acme.example"}).json()["id"]
    api.put(f"/api/lead/{lead_id}", json={"specialization": "B2B SaaS", "phone": "+15555550100"})

    again = api.post("/api/lead", json={
        "company": "Acme", "city": "Austin", "contact_person": "Alice Smith", "notes": "met at SaaStr"})
    assert again.status_code == 200 and again.json()["id"] == lead_id
    row = _stored(api, lead_id)
    assert (row["website"], row["phone"], row["specialization"]) == (
        "https://acme.example", "+15555550100", "B2B SaaS")
    assert row["notes"] == "met at SaaStr"


def test_a_post_without_a_contact_adds_company_details_to_the_lead(api):
    lead_id = api.post("/api/lead", json={
        "company": "Acme", "city": "Austin", "contact_person": "Alice Smith"}).json()["id"]
    again = api.post("/api/lead", json={"company": "Acme", "city": "Austin", "website": "https://acme.example"})
    assert again.status_code == 200 and again.json()["id"] == lead_id
    row = _stored(api, lead_id)
    assert row["website"] == "https://acme.example" and row["contact_person"] == "Alice Smith"


def test_put_is_the_explicit_way_to_replace_the_contact_or_clear_a_field(api):
    lead_id = api.post("/api/lead", json={
        "company": "Acme", "city": "Austin", "website": "https://acme.example",
        "contact_person": "Alice Smith", "email": "alice@acme.example"}).json()["id"]
    assert api.put(f"/api/lead/{lead_id}", json={
        "contact_person": "Bob Jones", "email": "bob@acme.example", "website": ""}).status_code == 200
    row = _stored(api, lead_id)
    assert (row["contact_person"], row["email"], row["website"]) == ("Bob Jones", "bob@acme.example", "")
