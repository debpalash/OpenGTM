"""Audience destination CRUD, durable runs, idempotency, and isolation."""

import asyncio

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.database import Base, get_db
from apps.api.models import Job
from apps.api.routers.destinations import router, require_admin, require_editor
from apps.api.services.audiences.models import Audience, AudienceMember
from apps.api.services.destinations.models import AudienceDestination, DestinationDelivery, DestinationInboundReceipt, DestinationInboundToken, DestinationRun
from apps.api.services.leadgen.orm_models import LeadRow

WS1 = "ws-dest-1"
WS2 = "ws-dest-2"


class _User:
    id = "user-1"


def _ctx(ws=WS1):
    return WorkspaceCtx(user=_User(), workspace_id=ws, slug=ws)


@pytest.fixture()
def destination_app():
    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[
        Audience.__table__, AudienceMember.__table__, AudienceDestination.__table__,
        DestinationRun.__table__, DestinationDelivery.__table__, DestinationInboundToken.__table__,
        DestinationInboundReceipt.__table__, LeadRow.__table__, Job.__table__,
    ])
    Session = sessionmaker(bind=engine)
    session = Session()
    audience = Audience(id="aud-1", workspace_id=WS1, name="Hot", filters={}, member_count=1)
    session.add(audience)
    session.add(AudienceMember(
        workspace_id=WS1, audience_id=audience.id, lead_id=42,
        snapshot={"id": 42, "company": "Acme", "email": "buyer@acme.test"},
    ))
    session.add(LeadRow(id=42, workspace_id=WS1, company="Acme", email="buyer@acme.test", phone=""))
    session.commit()
    session.close()

    app = FastAPI()
    app.include_router(router)

    def override_db():
        db = Session()
        try:
            yield db
        finally:
            db.close()

    app.dependency_overrides[get_db] = override_db
    app.dependency_overrides[current_workspace] = lambda: _ctx()
    app.dependency_overrides[require_editor] = lambda: _ctx()
    app.dependency_overrides[require_admin] = lambda: _ctx()
    return TestClient(app), Session, app


def test_destination_type_catalog_fails_closed_to_beta(destination_app, monkeypatch):
    monkeypatch.delenv("OPENGTM_INTEGRATION_CERTIFICATIONS", raising=False)
    monkeypatch.delenv("OPENGTM_INTEGRATION_CERTIFICATION_KEY", raising=False)
    tc, _, _ = destination_app

    response = tc.get("/api/audience-destinations/types")

    assert response.status_code == 200
    types = response.json()["types"]
    assert {item["id"] for item in types} == {
        "webhook", "hubspot", "salesforce", "warehouse_http",
        "meta_ads", "google_ads", "linkedin_ads", "instantly", "smartlead",
        "google_sheets",
        "airtable",
        "slack",
    }
    assert all(item["maturity"] == "beta" for item in types)


def test_destination_sync_is_durable_and_idempotent(destination_app, monkeypatch):
    tc, Session, _ = destination_app
    created = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Activation webhook", "destination_type": "webhook",
        "config": {"url": "https://hooks.example.test/audience", "method": "POST"},
        "field_map": {"company": "account_name", "email": "email"},
    })
    assert created.status_code == 201, created.text
    destination_id = created.json()["id"]
    assert tc.get("/api/audience-destinations?audience_id=aud-1").json()[0]["health_status"] == "unverified"

    started = tc.post(f"/api/audience-destinations/{destination_id}/sync")
    assert started.status_code == 202
    run_id = started.json()["id"]
    assert tc.post(f"/api/audience-destinations/{destination_id}/sync").json()["id"] == run_id

    from apps.api.services.destinations import engine as destination_engine
    delivered = []

    async def fake_deliver(destination, lead_id, snapshot, idem):
        delivered.append((lead_id, idem))
        return {"success": True, "summary": "POST 202", "error": None}

    monkeypatch.setattr(destination_engine, "SessionLocal", Session)
    monkeypatch.setattr(destination_engine, "_deliver", fake_deliver)
    asyncio.run(destination_engine.handle_destination_sync(1, {"workspace_id": WS1, "run_id": run_id}))

    runs = tc.get(f"/api/audience-destinations/{destination_id}/runs").json()
    assert runs[0]["status"] == "completed"
    assert runs[0]["succeeded"] == 1
    deliveries = tc.get(f"/api/audience-destinations/runs/{run_id}/deliveries").json()
    assert deliveries[0]["status"] == "success"
    assert len(delivered) == 1

    second = tc.post(f"/api/audience-destinations/{destination_id}/sync").json()
    asyncio.run(destination_engine.handle_destination_sync(2, {"workspace_id": WS1, "run_id": second["id"]}))
    second_run = tc.get(f"/api/audience-destinations/{destination_id}/runs").json()[0]
    assert second_run["skipped"] == 1
    assert len(delivered) == 1


def test_destination_queue_failure_reconciles_retry_and_terminal_state(destination_app, monkeypatch):
    tc, Session, _ = destination_app
    created = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Crash-safe webhook", "destination_type": "webhook",
        "config": {"url": "https://hooks.example.test/audience", "method": "POST"},
    })
    destination_id = created.json()["id"]
    run_id = tc.post(f"/api/audience-destinations/{destination_id}/sync").json()["id"]

    from apps.api.services.destinations import engine as destination_engine
    monkeypatch.setattr(destination_engine, "SessionLocal", Session)
    with Session() as db:
        run = db.get(DestinationRun, run_id)
        run.status = "running"
        delivery = DestinationDelivery(
            workspace_id=WS1, run_id=run_id, destination_id=destination_id,
            lead_id=42, idempotency_key=f"crash:{run_id}", payload_fingerprint="abc",
            status="in_flight", attempts=1,
        )
        db.add(delivery); db.commit()
        delivery_id = delivery.id

    payload = {"workspace_id": WS1, "run_id": run_id}
    destination_engine.reconcile_destination_job_failure(7, payload, "worker timeout", True)
    with Session() as db:
        run = db.get(DestinationRun, run_id)
        delivery = db.get(DestinationDelivery, delivery_id)
        assert run.status == "pending" and run.finished_at is None
        assert delivery.status == "pending"
        assert "worker timeout" in run.error

    destination_engine.reconcile_destination_job_failure(7, payload, "worker timeout", False)
    with Session() as db:
        run = db.get(DestinationRun, run_id)
        delivery = db.get(DestinationDelivery, delivery_id)
        destination = db.get(AudienceDestination, destination_id)
        assert run.status == "failed" and run.finished_at is not None
        assert run.attempted == 1 and run.failed == 1
        assert delivery.status == "failed"
        assert destination.health_status == "degraded"
        assert "Final failure" in destination.last_error


def test_failed_destination_deliveries_retry_on_original_run(destination_app, monkeypatch):
    tc, Session, _ = destination_app
    with Session() as session:
        session.add(AudienceMember(
            workspace_id=WS1, audience_id="aud-1", lead_id=43,
            snapshot={"id": 43, "company": "Beta", "email": "buyer@beta.test"},
        ))
        session.commit()
    destination_id = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Retry webhook", "destination_type": "webhook",
        "config": {"url": "https://hooks.example.test/audience"},
    }).json()["id"]
    run_id = tc.post(f"/api/audience-destinations/{destination_id}/sync").json()["id"]

    from apps.api.services.destinations import engine as destination_engine
    calls = []
    async def flaky(destination, lead_id, snapshot, idem):
        calls.append(lead_id)
        success = lead_id == 43 or calls.count(lead_id) > 1
        return {"success": success, "summary": "ok" if success else "", "error": None if success else "temporary"}

    monkeypatch.setattr(destination_engine, "SessionLocal", Session)
    monkeypatch.setattr(destination_engine, "_deliver", flaky)
    asyncio.run(destination_engine.handle_destination_sync(1, {"workspace_id": WS1, "run_id": run_id}))
    assert tc.get(f"/api/audience-destinations/{destination_id}/runs").json()[0]["status"] == "completed_with_errors"
    listed = tc.get("/api/audience-destinations?audience_id=aud-1").json()
    assert next(item for item in listed if item["id"] == destination_id)["latest_run"]["id"] == run_id

    retried = tc.post(f"/api/audience-destinations/runs/{run_id}/retry")
    assert retried.status_code == 202, retried.text
    assert retried.json()["id"] == run_id and retried.json()["status"] == "pending"
    asyncio.run(destination_engine.handle_destination_sync(2, {"workspace_id": WS1, "run_id": run_id}))

    with Session() as session:
        deliveries = {row.lead_id: row for row in session.query(DestinationDelivery).filter_by(run_id=run_id).all()}
        assert calls == [42, 43, 42]
        assert deliveries[42].status == "success" and deliveries[42].attempts == 2
        assert deliveries[43].status == "success" and deliveries[43].attempts == 1
        run = session.get(DestinationRun, run_id)
        assert run.status == "completed" and run.failed == 0 and run.skipped == 1


def test_destination_health_and_bounded_delivery_inspection(destination_app):
    tc, Session, _ = destination_app
    destination_id = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Observable webhook", "destination_type": "webhook",
        "config": {"url": "https://hooks.example.test/audience"},
    }).json()["id"]
    with Session() as session:
        destination = session.get(AudienceDestination, destination_id)
        destination.health_status = "degraded"
        destination.last_error = "2 deliveries failed"
        run = DestinationRun(
            id="health-run", workspace_id=WS1, destination_id=destination_id,
            status="completed_with_errors", attempted=3, succeeded=1, failed=2,
        )
        session.add(run)
        session.flush()
        for lead_id, status in ((42, "success"), (43, "failed"), (44, "failed")):
            session.add(DestinationDelivery(
                workspace_id=WS1, run_id=run.id, destination_id=destination_id,
                lead_id=lead_id, idempotency_key=f"health-{lead_id}",
                payload_fingerprint=str(lead_id), status=status, attempts=1,
                error="provider timeout" if status == "failed" else None,
            ))
        session.commit()

    health = tc.get(f"/api/audience-destinations/{destination_id}/health")
    assert health.status_code == 200
    payload = health.json()
    assert payload["health_status"] == "degraded"
    assert payload["delivery_counts"] == {"total": 3, "succeeded": 1, "failed": 2}
    assert payload["success_rate"] == pytest.approx(1 / 3, abs=0.0001)
    assert payload["consecutive_unhealthy_runs"] == 1
    assert len(payload["recent_failures"]) == 2
    assert payload["latest_run"]["id"] == "health-run"

    failures = tc.get(
        "/api/audience-destinations/runs/health-run/deliveries",
        params={"status": "failed", "limit": 1},
    )
    assert failures.status_code == 200 and len(failures.json()) == 1
    next_page = tc.get(
        "/api/audience-destinations/runs/health-run/deliveries",
        params={"status": "failed", "after_id": failures.json()[0]["id"], "limit": 1},
    )
    assert len(next_page.json()) == 1
    assert tc.get(
        "/api/audience-destinations/runs/health-run/deliveries",
        params={"status": "unknown"},
    ).status_code == 422
    assert tc.get(
        "/api/audience-destinations/runs/health-run/deliveries",
        params={"limit": 501},
    ).status_code == 422


def test_slack_destination_uses_workspace_secret_and_pinned_webhook(destination_app, monkeypatch):
    tc, Session, _ = destination_app
    from apps.api.services.workspace import secrets
    monkeypatch.setattr(
        secrets,
        "get_secret",
        lambda workspace_id, key, default="": (
            "https://hooks.slack.test/services/secret" if workspace_id == WS1 and key == "slack_hook" else default
        ),
    )
    created = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1",
        "name": "Sales alerts",
        "destination_type": "slack",
        "config": {
            "webhook_secret_ref": "slack_hook",
            "message_template": "New fit: {company} ({email})",
        },
    })
    assert created.status_code == 201, created.text
    assert "hooks.slack.test" not in str(created.json())

    from apps.api.services.automations import actions
    from apps.api.services.destinations import engine as destination_engine
    captured = {}

    async def fake_webhook(workspace_id, config, lead, columns):
        captured.update({"workspace_id": workspace_id, "config": config})
        return actions.ActionResult("success", summary="POST 200")

    monkeypatch.setattr(actions, "_act_webhook", fake_webhook)
    with Session() as session:
        destination = session.get(AudienceDestination, created.json()["id"])
        result = asyncio.run(destination_engine._deliver(
            destination, 42, {"company": "Acme", "email": "buyer@acme.test"}, "idem-42",
        ))
    assert result["success"] is True
    assert captured["workspace_id"] == WS1
    assert captured["config"]["url"] == "https://hooks.slack.test/services/secret"
    assert captured["config"]["headers"]["Idempotency-Key"] == "idem-42"
    assert captured["config"]["body"]["text"] == "New fit: Acme (buyer@acme.test)"

    missing = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Missing Slack", "destination_type": "slack",
        "config": {"webhook_secret_ref": "missing"},
    })
    assert missing.status_code == 422

def test_destination_validation_and_tenant_isolation(destination_app):
    tc, Session, app = destination_app
    invalid = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Unsafe", "destination_type": "webhook",
        "config": {"url": "http://user:pass@example.com", "token": "plaintext"},
    })
    assert invalid.status_code == 422
    missing = tc.post("/api/audience-destinations", json={
        "audience_id": "other", "name": "Missing", "destination_type": "hubspot",
    })
    assert missing.status_code == 404

    session = Session()
    hidden_audience = Audience(id="aud-2", workspace_id=WS2, name="Hidden", filters={})
    session.add(hidden_audience)
    session.flush()
    hidden = AudienceDestination(
        id="dest-hidden", workspace_id=WS2, audience_id=hidden_audience.id,
        name="Hidden", destination_type="hubspot", config={}, field_map={},
    )
    session.add(hidden)
    session.commit()
    session.close()
    assert tc.get("/api/audience-destinations").json() == []
    assert tc.patch("/api/audience-destinations/dest-hidden", json={"enabled": False}).status_code == 404

    missing_consent = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Meta", "destination_type": "meta_ads",
        "config": {"custom_audience_id": "123"},
    })
    assert missing_consent.status_code == 422
    missing_campaign = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Instantly",
        "destination_type": "instantly", "config": {},
    })
    assert missing_campaign.status_code == 422
    invalid_sheet = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Sheet",
        "destination_type": "google_sheets",
        "config": {"spreadsheet_id": "sheet-1", "columns": []},
    })
    assert invalid_sheet.status_code == 422
    invalid_airtable = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Airtable",
        "destination_type": "airtable", "config": {"base_id": "base-1"},
    })
    assert invalid_airtable.status_code == 422
    ad = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Meta", "destination_type": "meta_ads",
        "config": {"custom_audience_id": "123", "consent_attested": True, "consent_source": "CRM opt-in"},
    })
    assert ad.status_code == 201, ad.text

    app.dependency_overrides[current_workspace] = lambda: _ctx(WS2)
    assert tc.get("/api/audience-destinations").json()[0]["name"] == "Hidden"


def test_sequencer_destination_uses_campaign_and_field_mapping(destination_app, monkeypatch):
    tc, Session, _ = destination_app
    created = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Instantly campaign",
        "destination_type": "instantly",
        "config": {"campaign_id": "campaign-42", "skip_if_in_campaign": True},
        "field_map": {"email": "email", "company": "company_name"},
    })
    assert created.status_code == 201, created.text
    run_id = tc.post(
        f"/api/audience-destinations/{created.json()['id']}/sync"
    ).json()["id"]
    from apps.api.services.destinations import engine as destination_engine
    from apps.api.services.integrations import instantly

    captured = {}

    async def fake_add(campaign_id, lead, skip_if_in_campaign, workspace_id):
        captured.update(
            campaign_id=campaign_id, lead=lead,
            skip=skip_if_in_campaign, workspace_id=workspace_id,
        )
        return {"success": True, "lead_id": "external-lead-1", "duplicate": False}

    monkeypatch.setattr(destination_engine, "SessionLocal", Session)
    monkeypatch.setattr(instantly, "add_lead_to_campaign", fake_add)
    asyncio.run(destination_engine.handle_destination_sync(
        1, {"workspace_id": WS1, "run_id": run_id},
    ))

    assert captured == {
        "campaign_id": "campaign-42",
        "lead": {"email": "buyer@acme.test", "company_name": "Acme"},
        "skip": True,
        "workspace_id": WS1,
    }
    session = Session()
    delivery = session.query(DestinationDelivery).filter(
        DestinationDelivery.run_id == run_id,
    ).one()
    assert delivery.status == "success"
    assert delivery.external_id == "external-lead-1"
    session.close()


def test_google_sheets_destination_uses_delivery_key_for_upsert(
    destination_app, monkeypatch,
):
    tc, Session, _ = destination_app
    created = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Sheet",
        "destination_type": "google_sheets",
        "config": {
            "spreadsheet_id": "sheet-1", "range": "Leads!A:ZZ",
            "columns": ["company_name", "email"],
        },
        "field_map": {"company": "company_name", "email": "email"},
    })
    assert created.status_code == 201, created.text
    run_id = tc.post(
        f"/api/audience-destinations/{created.json()['id']}/sync"
    ).json()["id"]
    from apps.api.services.destinations import engine as destination_engine
    from apps.api.services.integrations import sheets

    observed = {}

    async def fake_upsert(spreadsheet_id, values, idem, sheet_range, workspace_id):
        observed.update(
            spreadsheet_id=spreadsheet_id, values=values, idem=idem,
            sheet_range=sheet_range, workspace_id=workspace_id,
        )
        return {"success": True, "range": "Leads!A2:C2", "operation": "appended"}

    monkeypatch.setattr(destination_engine, "SessionLocal", Session)
    monkeypatch.setattr(sheets, "upsert_row", fake_upsert)
    asyncio.run(destination_engine.handle_destination_sync(
        1, {"workspace_id": WS1, "run_id": run_id},
    ))

    assert observed["spreadsheet_id"] == "sheet-1"
    assert observed["values"] == ["Acme", "buyer@acme.test"]
    assert observed["idem"] == f"dest:{created.json()['id']}:lead:42"
    assert observed["workspace_id"] == WS1


def test_airtable_destination_uses_delivery_key_for_atomic_upsert(
    destination_app, monkeypatch,
):
    tc, Session, _ = destination_app
    created = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Airtable",
        "destination_type": "airtable",
        "config": {
            "base_id": "base-1", "table": "Leads",
            "idempotency_field": "OpenGTM ID",
        },
        "field_map": {"company": "Company", "email": "Email"},
    })
    assert created.status_code == 201, created.text
    run_id = tc.post(
        f"/api/audience-destinations/{created.json()['id']}/sync"
    ).json()["id"]
    from apps.api.services.destinations import engine as destination_engine
    from apps.api.services.integrations import airtable

    observed = {}

    async def fake_upsert(fields, base_id, table, idem, key_field, typecast, workspace_id):
        observed.update(fields=fields, idem=idem, workspace_id=workspace_id)
        return {"success": True, "record_id": "rec-1", "operation": "created"}

    monkeypatch.setattr(destination_engine, "SessionLocal", Session)
    monkeypatch.setattr(airtable, "upsert_record", fake_upsert)
    asyncio.run(destination_engine.handle_destination_sync(
        1, {"workspace_id": WS1, "run_id": run_id},
    ))
    assert observed == {
        "fields": {"Company": "Acme", "Email": "buyer@acme.test"},
        "idem": f"dest:{created.json()['id']}:lead:42",
        "workspace_id": WS1,
    }


def test_audience_change_auto_enqueues_destination_once(destination_app):
    _, Session, _ = destination_app
    from apps.api.services.destinations.engine import enqueue_audience_syncs

    session = Session()
    destination = AudienceDestination(
        workspace_id=WS1, audience_id="aud-1", name="Auto", destination_type="hubspot",
        config={}, field_map={},
    )
    session.add(destination)
    session.commit()
    assert enqueue_audience_syncs(session, WS1, "aud-1") == 1
    assert enqueue_audience_syncs(session, WS1, "aud-1") == 0
    assert session.query(DestinationRun).filter(DestinationRun.destination_id == destination.id).count() == 1
    assert session.query(Job).filter(Job.type == "audience_destination_sync", Job.status == "pending").count() == 1
    session.close()


def test_paid_media_sync_batches_hashed_identifiers(destination_app, monkeypatch):
    tc, Session, _ = destination_app
    created = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "LinkedIn", "destination_type": "linkedin_ads",
        "config": {"segment_id": "987", "consent_attested": True, "consent_source": "CRM opt-in"},
    })
    run_id = tc.post(f"/api/audience-destinations/{created.json()['id']}/sync").json()["id"]
    from apps.api.services.destinations import ads, engine as destination_engine
    captured = []

    async def fake_batch(workspace_id, dtype, config, snapshots, operation="add"):
        captured.append((operation, snapshots))
        return ads.AdBatchResult(True, f"{operation}ed 1 hashed users", external_id="job-1")

    monkeypatch.setattr(destination_engine, "SessionLocal", Session)
    monkeypatch.setattr(ads, "sync_ad_batch", fake_batch)
    asyncio.run(destination_engine.handle_destination_sync(1, {"workspace_id": WS1, "run_id": run_id}))
    assert captured == [("add", [{"id": 42, "company": "Acme", "email": "buyer@acme.test"}])]
    delivery = tc.get(f"/api/audience-destinations/runs/{run_id}/deliveries").json()[0]
    assert delivery["status"] == "success"
    assert delivery["external_id"] == "job-1"

    # Removing the member must issue a platform REMOVE using only the hash
    # retained in the successful add delivery—never the raw email.
    with Session() as session:
        session.query(AudienceMember).filter_by(audience_id="aud-1", lead_id=42).delete()
        session.commit()
    second_run = tc.post(f"/api/audience-destinations/{created.json()['id']}/sync").json()["id"]
    asyncio.run(destination_engine.handle_destination_sync(2, {"workspace_id": WS1, "run_id": second_run}))
    expected_hash = ads.hash_email("buyer@acme.test")
    assert captured[-1] == ("remove", [{"email": expected_hash}])
    removed = tc.get(f"/api/audience-destinations/runs/{second_run}/deliveries").json()
    assert len(removed) == 1 and removed[0]["operation"] == "remove" and removed[0]["status"] == "success"

    # A subsequent reconciliation sees REMOVE as the latest successful state
    # and does not send another platform request.
    third_run = tc.post(f"/api/audience-destinations/{created.json()['id']}/sync").json()["id"]
    asyncio.run(destination_engine.handle_destination_sync(3, {"workspace_id": WS1, "run_id": third_run}))
    assert len(captured) == 2


def test_paid_media_skips_profiles_without_identifiers_independently(destination_app, monkeypatch):
    tc, Session, _ = destination_app
    with Session() as session:
        session.add(AudienceMember(
            workspace_id=WS1, audience_id="aud-1", lead_id=43,
            snapshot={"id": 43, "company": "No Contact Data"},
        ))
        session.commit()
    created = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Meta mixed batch", "destination_type": "meta_ads",
        "config": {"custom_audience_id": "123", "consent_attested": True, "consent_source": "CRM opt-in"},
    }).json()
    run_id = tc.post(f"/api/audience-destinations/{created['id']}/sync").json()["id"]
    from apps.api.services.destinations import ads, engine as destination_engine
    captured = []

    async def fake_batch(workspace_id, dtype, config, snapshots, operation="add"):
        captured.extend(snapshots)
        return ads.AdBatchResult(True, "added 1 hashed user")

    monkeypatch.setattr(destination_engine, "SessionLocal", Session)
    monkeypatch.setattr(ads, "sync_ad_batch", fake_batch)
    asyncio.run(destination_engine.handle_destination_sync(
        1, {"workspace_id": WS1, "run_id": run_id},
    ))

    assert [item["email"] for item in captured] == ["buyer@acme.test"]
    deliveries = tc.get(f"/api/audience-destinations/runs/{run_id}/deliveries").json()
    by_lead = {item["lead_id"]: item for item in deliveries}
    assert by_lead[42]["status"] == "success"
    assert by_lead[43]["status"] == "skipped"
    assert by_lead[43]["summary"] == "No valid email or phone identifier"
    run = tc.get(f"/api/audience-destinations/{created['id']}/runs").json()[0]
    assert run["attempted"] == 1 and run["succeeded"] == 1 and run["skipped"] == 1


def test_ad_identifier_normalization_never_returns_raw_pii():
    from apps.api.services.destinations.ads import hashed_identifiers, identifiers_supported

    identifiers = hashed_identifiers({"email": " Buyer@Acme.Test ", "phone": "+1 (415) 555-0123"})
    assert identifiers["email"] == "b292f2116ddeba3b424ddeb0ad00067c22b4be4398239732b6a8a615eece634c"
    assert identifiers["phone"] == "413ba75461ab5f99d36820e561ea97e2bd80f9cb586f7ecea6cf4c496518950a"
    assert "buyer" not in str(identifiers)
    assert identifiers_supported("meta_ads", {"phone": identifiers["phone"]})
    assert identifiers_supported("google_ads", {"phone": identifiers["phone"]})
    assert not identifiers_supported("linkedin_ads", {"phone": identifiers["phone"]})
    assert identifiers_supported("linkedin_ads", {"email": identifiers["email"]})


def test_meta_ad_remove_uses_delete_and_hashed_payload(monkeypatch):
    from apps.api.services.destinations import ads

    monkeypatch.setattr(ads, "_secret", lambda workspace_id, key: "token")
    captured = {}

    class _Response:
        def json(self):
            return {"session_id": "session-1"}

    async def fake_request(client, method, url, **kwargs):
        captured.update(method=method, url=url, kwargs=kwargs)
        return _Response()

    monkeypatch.setattr(ads, "_request", fake_request)
    result = asyncio.run(ads.sync_ad_batch(
        WS1,
        "meta_ads",
        {"custom_audience_id": "aud-123"},
        [{"email": " Buyer@Acme.Test "}],
        operation="remove",
    ))
    payload = __import__("json").loads(captured["kwargs"]["data"]["payload"])
    assert captured["method"] == "DELETE"
    assert payload["data"] == [[ads.hash_email("buyer@acme.test")]]
    assert "buyer" not in str(payload).lower()
    assert result.success and result.summary == "removed 1 hashed users"

    persisted_hash = ads.hash_email("buyer@acme.test")
    asyncio.run(ads.sync_ad_batch(
        WS1, "meta_ads", {"custom_audience_id": "aud-123"},
        [{"email": persisted_hash}], operation="remove",
    ))
    payload = __import__("json").loads(captured["kwargs"]["data"]["payload"])
    assert payload["data"] == [[persisted_hash]]


def test_google_ad_partial_failure_is_private_retryable_and_not_run(monkeypatch):
    from apps.api.services.destinations import ads

    monkeypatch.setattr(ads, "_secret", lambda workspace_id, key: "token")
    calls = []

    class _Response:
        def __init__(self, body):
            self.body = body

        def json(self):
            return self.body

    async def fake_request(client, method, url, **kwargs):
        calls.append(url)
        if url.endswith("offlineUserDataJobs:create"):
            return _Response({"resourceName": "customers/123/offlineUserDataJobs/456"})
        return _Response({"partialFailureError": {
            "code": 3,
            "status": "INVALID_ARGUMENT",
            "message": "buyer@acme.test was rejected",
            "details": [{"errors": [{"message": "raw identifier must stay private"}]}],
        }})

    monkeypatch.setattr(ads, "_request", fake_request)
    result = asyncio.run(ads.sync_ad_batch(
        WS1, "google_ads", {"customer_id": "123", "user_list_id": "789"},
        [{"email": "buyer@acme.test"}],
    ))

    assert not result.success
    assert result.external_id == "customers/123/offlineUserDataJobs/456"
    assert result.error == "Google Ads rejected operations (status=INVALID_ARGUMENT, code=3, count=1)"
    assert "buyer" not in result.error and "raw identifier" not in result.error
    assert len(calls) == 2
    assert not any(url.endswith(":run") for url in calls)


def test_google_ad_clean_batch_runs(monkeypatch):
    from apps.api.services.destinations import ads

    monkeypatch.setattr(ads, "_secret", lambda workspace_id, key: "token")
    calls = []

    class _Response:
        def __init__(self, body):
            self.body = body

        def json(self):
            return self.body

    async def fake_request(client, method, url, **kwargs):
        calls.append(url)
        if url.endswith("offlineUserDataJobs:create"):
            return _Response({"resourceName": "customers/123/offlineUserDataJobs/456"})
        return _Response({})

    monkeypatch.setattr(ads, "_request", fake_request)
    result = asyncio.run(ads.sync_ad_batch(
        WS1, "google_ads", {"customer_id": "123", "user_list_id": "789"},
        [{"email": "buyer@acme.test"}],
    ))

    assert result.success
    assert len(calls) == 3
    assert calls[-1].endswith(":run")


def test_crm_inbound_token_auth_replay_and_receipts(destination_app, monkeypatch):
    tc, Session, _ = destination_app
    created = tc.post("/api/audience-destinations", json={"audience_id": "aud-1", "name": "CRM", "destination_type": "hubspot", "config": {"inbound_conflict_policy": "fill_missing"}, "field_map": {"contact_title": "jobtitle"}})
    destination_id = created.json()["id"]
    from apps.api.routers import destinations as router_module
    from apps.api import database as database_module
    monkeypatch.setattr(router_module, "SessionLocal", Session)
    monkeypatch.setattr(database_module, "SessionLocal", Session)
    token = tc.post(f"/api/audience-destinations/{destination_id}/inbound-token").json()["token"]
    body = {"external_event_id": "hub-evt-1", "lead_id": 42, "fields": {"phone": "123", "jobtitle": "VP Sales", "score": 1}}
    denied = tc.post(f"/api/audience-destinations/inbound/{destination_id}", json=body)
    assert denied.status_code == 401
    first = tc.post(f"/api/audience-destinations/inbound/{destination_id}", json=body, headers={"Authorization": f"Bearer {token}"})
    replay = tc.post(f"/api/audience-destinations/inbound/{destination_id}", json=body, headers={"Authorization": f"Bearer {token}"})
    assert first.status_code == 200 and first.json()["applied_fields"] == ["contact_title", "phone"]
    assert replay.json()["replay"] is True
    history = tc.get(f"/api/audience-destinations/{destination_id}/inbound-receipts").json()
    assert len(history) == 1 and history[0]["external_event_id"] == "hub-evt-1"
