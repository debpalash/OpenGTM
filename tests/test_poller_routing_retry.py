"""Detected company events remain retryable until lead routing is available."""

from uuid import uuid4

from apps.api.database import SessionLocal
from apps.api.core.tenancy import current_workspace_var
from apps.api.services.leadgen.orm_models import LeadRow, SignalRow
from apps.api.services.poller import engine, sources
from apps.api.services.poller.models import WatchSubscription
from apps.api.services.signals.store import SignalStore


def test_unroutable_batch_retries_once_after_lead_resolution(monkeypatch):
    workspace = f"routing-{uuid4()}"
    watch_id = str(uuid4())
    original = {"bootstrapped": True, "hiring_band": "none"}
    events = [sources.DetectedEvent(natural_event_id=f"event-{i}", signal_type="hiring_surge",
                                    title=f"Synthetic hiring event {i}") for i in range(2)]
    with SessionLocal() as db:
        db.add(WatchSubscription(id=watch_id, workspace_id=workspace, kind="hiring",
                                target="Synthetic Routing Company", interval="daily",
                                cursor=original, enabled=True))
        db.commit()
    monkeypatch.setattr(engine, "_debit_source", lambda *args: True)
    monkeypatch.setattr(sources, "fetch_hiring_and_tech", lambda *args, **kw:
                        (events, {"hiring_band": "growing"}))
    store = SignalStore(workspace)
    token = current_workspace_var.set(workspace)
    try:
        result = engine._poll_one_source(store, watch_id, workspace, "hiring", "first", None, False)
        with SessionLocal() as db:
            watch = db.get(WatchSubscription, watch_id)
            assert watch.cursor == original
            assert watch.last_error == "unresolved_lead"
            assert db.query(SignalRow).filter_by(workspace_id=workspace).count() == 0
            assert result is False
            lead = LeadRow(workspace_id=workspace, company="Synthetic Routing Company")
            db.add(lead)
            db.commit()
            lead_id, error = engine._resolve_lead_id(db, watch)
            assert lead_id == lead.id and error is None
        # A retry after routing becomes available emits the complete batch; an
        # additional replay must preserve the same durable signal identities.
        engine._poll_one_source(store, watch_id, workspace, "hiring", "retry", lead_id, False)
        engine._poll_one_source(store, watch_id, workspace, "hiring", "replay", lead_id, False)
        with SessionLocal() as db:
            saved = db.query(SignalRow).filter_by(workspace_id=workspace).all()
            assert len(saved) == 2
            assert {signal.lead_id for signal in saved} == {lead_id}
            assert db.get(WatchSubscription, watch_id).cursor["hiring_band"] == "growing"
    finally:
        with SessionLocal() as db:
            db.query(SignalRow).filter_by(workspace_id=workspace).delete()
            db.query(WatchSubscription).filter_by(id=watch_id).delete()
            db.query(LeadRow).filter_by(workspace_id=workspace).delete()
            db.commit()
        current_workspace_var.reset(token)
