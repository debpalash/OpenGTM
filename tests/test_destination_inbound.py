from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.services.audiences.models import Audience
from apps.api.services.destinations.inbound import reconcile
from apps.api.services.destinations.models import AudienceDestination, DestinationInboundReceipt
from apps.api.services.leadgen.orm_models import LeadRow


def _db():
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[Audience.__table__, AudienceDestination.__table__, DestinationInboundReceipt.__table__, LeadRow.__table__])
    return sessionmaker(bind=engine)()


def test_inbound_reconciliation_is_idempotent_allowlisted_and_fill_missing():
    db = _db()
    db.add(Audience(id="aud", workspace_id="ws", name="A", filters={}))
    destination = AudienceDestination(id="dest", workspace_id="ws", audience_id="aud", name="HubSpot", destination_type="hubspot", field_map={"contact_title": "jobtitle"}, config={"inbound_conflict_policy": "fill_missing"})
    lead = LeadRow(id=1, workspace_id="ws", company="Acme", email="old@acme.test", phone="", contact_title="", score=90)
    db.add_all([destination, lead]); db.commit()
    body = {"external_event_id": "evt-1", "lead_id": 1, "fields": {"email": "new@acme.test", "phone": "123", "jobtitle": "VP Sales", "score": 1, "workspace_id": "evil"}}
    first, replay = reconcile(db, destination, body)
    second, replay_second = reconcile(db, destination, body)
    db.refresh(lead)
    assert replay is False and replay_second is True and first["id"] == second["id"]
    assert lead.email == "old@acme.test" and lead.phone == "123" and lead.contact_title == "VP Sales" and lead.score == 90
    assert first["applied_fields"] == ["contact_title", "phone"]
    assert set(first["ignored_fields"]) == {"email", "score", "workspace_id"}
    assert db.query(DestinationInboundReceipt).count() == 1
    db.close()


def test_crm_wins_and_unmatched_callbacks_have_durable_receipts():
    db = _db()
    db.add(Audience(id="aud", workspace_id="ws", name="A", filters={}))
    destination = AudienceDestination(id="dest", workspace_id="ws", audience_id="aud", name="CRM", destination_type="salesforce", config={"inbound_conflict_policy": "crm_wins"})
    lead = LeadRow(id=1, workspace_id="ws", company="Acme", email="old@acme.test")
    db.add_all([destination, lead]); db.commit()
    applied, _ = reconcile(db, destination, {"external_event_id": "evt-1", "lead_id": 1, "fields": {"email": "new@acme.test"}})
    unmatched, _ = reconcile(db, destination, {"external_event_id": "evt-2", "lead_id": 999, "fields": {"email": "nobody@test"}})
    db.refresh(lead)
    assert lead.email == "new@acme.test" and applied["status"] == "applied"
    assert unmatched["status"] == "unmatched" and unmatched["error"]
    db.close()
