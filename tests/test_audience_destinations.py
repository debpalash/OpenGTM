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
    ad = tc.post("/api/audience-destinations", json={
        "audience_id": "aud-1", "name": "Meta", "destination_type": "meta_ads",
        "config": {"custom_audience_id": "123", "consent_attested": True, "consent_source": "CRM opt-in"},
    })
    assert ad.status_code == 201, ad.text

    app.dependency_overrides[current_workspace] = lambda: _ctx(WS2)
    assert tc.get("/api/audience-destinations").json()[0]["name"] == "Hidden"


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

    async def fake_batch(workspace_id, dtype, config, snapshots):
        captured.extend(snapshots)
        return ads.AdBatchResult(True, "uploaded 1 hashed users", external_id="job-1")

    monkeypatch.setattr(destination_engine, "SessionLocal", Session)
    monkeypatch.setattr(ads, "sync_ad_batch", fake_batch)
    asyncio.run(destination_engine.handle_destination_sync(1, {"workspace_id": WS1, "run_id": run_id}))
    assert captured == [{"id": 42, "company": "Acme", "email": "buyer@acme.test"}]
    delivery = tc.get(f"/api/audience-destinations/runs/{run_id}/deliveries").json()[0]
    assert delivery["status"] == "success"
    assert delivery["external_id"] == "job-1"


def test_ad_identifier_normalization_never_returns_raw_pii():
    from apps.api.services.destinations.ads import hashed_identifiers

    identifiers = hashed_identifiers({"email": " Buyer@Acme.Test ", "phone": "+1 (415) 555-0123"})
    assert identifiers["email"] == "b292f2116ddeba3b424ddeb0ad00067c22b4be4398239732b6a8a615eece634c"
    assert identifiers["phone"] == "413ba75461ab5f99d36820e561ea97e2bd80f9cb586f7ecea6cf4c496518950a"
    assert "buyer" not in str(identifiers)


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
