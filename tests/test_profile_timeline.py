"""Unified profile timeline ordering and tenant isolation."""

from datetime import datetime, timedelta, timezone
from types import SimpleNamespace

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.routers.leads import lead_timeline
from apps.api.services.audiences.models import Audience, AudienceMembershipEvent
from apps.api.services.destinations.models import AudienceDestination, DestinationDelivery, DestinationRun
from apps.api.services.outreach.orm_models import OutreachSend


class _LeadDb:
    def __init__(self, lead):
        self.lead = lead

    def get_lead(self, lead_id):
        return self.lead if lead_id == self.lead.id else None

    def close(self):
        pass


class _Ctx:
    workspace_id = "ws-timeline"

    def __init__(self, lead):
        self.lead = lead

    def lead_db(self):
        return _LeadDb(self.lead)


def test_profile_timeline_merges_orders_and_scopes(monkeypatch):
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[Audience.__table__, AudienceMembershipEvent.__table__, AudienceDestination.__table__, DestinationRun.__table__, DestinationDelivery.__table__, OutreachSend.__table__])
    Session = sessionmaker(bind=engine)
    db = Session()
    now = datetime.now(timezone.utc)
    audience = Audience(id="aud", workspace_id="ws-timeline", name="High intent", filters={})
    db.add(audience)
    db.add(AudienceMembershipEvent(workspace_id="ws-timeline", audience_id="aud", lead_id=7, event_type="entered", snapshot={}, created_at=now - timedelta(minutes=2)))
    db.add(AudienceMembershipEvent(workspace_id="other", audience_id="aud", lead_id=7, event_type="exited", snapshot={}, created_at=now))
    destination = AudienceDestination(id="dest", workspace_id="ws-timeline", audience_id="aud", name="CRM", destination_type="hubspot", config={}, field_map={})
    run = DestinationRun(id="run", workspace_id="ws-timeline", destination_id="dest")
    db.add_all([destination, run])
    db.flush()
    db.add(DestinationDelivery(workspace_id="ws-timeline", run_id="run", destination_id="dest", lead_id=7, operation="upsert", idempotency_key="one", payload_fingerprint="a" * 64, status="success", delivered_at=now - timedelta(minutes=1)))
    db.add(OutreachSend(workspace_id="ws-timeline", lead_id=7, to_email="hidden@example.test", subject="Hello", status="sent", idempotency_key="send-one", sent_at=now))
    db.commit()

    class _Signals:
        def get_signals(self, **kwargs):
            assert kwargs["lead_id"] == 7
            return [{"id": "sig", "title": "Funding", "signal_type": "funding", "description": "Series A", "source": "SEC", "source_url": "https://example.test", "weight": 8, "created_at": (now - timedelta(minutes=3)).timestamp()}]

    monkeypatch.setattr("apps.api.services.signals.store.get_signal_store", lambda workspace_id: _Signals())
    lead = SimpleNamespace(id=7, created_at=(now - timedelta(days=1)).isoformat(), updated_at="", last_enriched_at="")
    result = lead_timeline(7, limit=100, before=None, db=db, ctx=_Ctx(lead))
    assert [item["kind"] for item in result["items"][:4]] == ["outreach", "activation", "audience", "signal"]
    assert not any(item["status"] == "exited" for item in result["items"])
    assert "hidden@example.test" not in str(result)
    first = lead_timeline(7, limit=2, before=None, before_id=None, db=db, ctx=_Ctx(lead))
    second = lead_timeline(7, limit=2, before=first["next_before"], before_id=first["next_before_id"], db=db, ctx=_Ctx(lead))
    assert not ({item["id"] for item in first["items"]} & {item["id"] for item in second["items"]})
    db.close()
