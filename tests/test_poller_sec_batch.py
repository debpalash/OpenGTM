"""Funding and executive detectors must consume their shared filing batch together."""
from types import SimpleNamespace
from uuid import uuid4

import pytest

from apps.api.core.tenancy import current_workspace_var
from apps.api.database import SessionLocal
from apps.api.services.leadgen.enrichment.providers import sec_edgar
from apps.api.services.leadgen.orm_models import LeadRow, SignalRow
from apps.api.services.poller import engine
from apps.api.services.poller.models import WatchSubscription
from apps.api.services.signals.store import SignalStore


@pytest.mark.parametrize("kind,types,expected", [
    ("company", ["company_funded", "executive_hired"], {"company_funded", "executive_hired"}),
    ("funding", ["company_funded", "executive_hired"], {"company_funded", "executive_hired"}),
    ("funding", ["company_funded"], {"company_funded"}),
    ("company", ["executive_hired"], {"executive_hired"}),
])
def test_sec_poll_preserves_enabled_event_types_and_replay(kind, types, expected, monkeypatch):
    workspace, watch_id = f"sec-batch-{uuid4()}", str(uuid4())
    filing = SimpleNamespace(cik="0001234567", accession="acc101", filing_date="2026-01-01",
                             fields={"funding_amount": 5000000},
                             related_persons=[{"name": "Synthetic Person", "title": "Chief Executive Officer"}])
    calls = []
    class Provider:
        async def list_form_d_since(self, target, since):
            calls.append(since)
            return [filing] if not since or filing.accession > since else []
    monkeypatch.setattr(sec_edgar, "SecEdgarProvider", Provider)
    monkeypatch.setattr(engine, "_debit_source", lambda *args: True)
    with SessionLocal() as db:
        lead = LeadRow(workspace_id=workspace, company="Synthetic SEC Company")
        db.add(lead)
        db.flush()
        lead_id = lead.id
        db.add(WatchSubscription(id=watch_id, workspace_id=workspace, kind=kind,
                                target=lead.company, lead_id=lead_id, interval="daily", enabled=True,
                                signal_types=types, cursor={"bootstrapped": True, "sec_last_accession": "acc100"}))
        db.commit()
    store = SignalStore(workspace)
    token = current_workspace_var.set(workspace)
    try:
        for attempt in ("first", "replay"):
            with SessionLocal() as db:
                watch = db.get(WatchSubscription, watch_id)
                kinds = engine._source_set(watch)
            for source in kinds:
                engine._poll_one_source(store, watch_id, workspace, source, attempt, lead_id, False)
            with SessionLocal() as db:
                signals = db.query(SignalRow).filter_by(workspace_id=workspace).all()
                assert {signal.signal_type for signal in signals} == expected
                assert len(signals) == len(expected)
                assert {signal.lead_id for signal in signals} == {lead_id}
                assert db.get(WatchSubscription, watch_id).cursor["sec_last_accession"] == "acc101"
        assert calls == ["acc100", "acc101"]
    finally:
        with SessionLocal() as db:
            db.query(SignalRow).filter_by(workspace_id=workspace).delete()
            db.query(WatchSubscription).filter_by(id=watch_id).delete()
            db.query(LeadRow).filter_by(workspace_id=workspace).delete()
            db.commit()
        current_workspace_var.reset(token)
