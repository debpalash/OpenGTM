from types import SimpleNamespace

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.models import Job
from apps.api.services.poller.account_group import fetch_account_group
from apps.api.services.poller.models import WatchSchedule, WatchSubscription
from apps.api.services.signals.tracking import (
    ACCOUNT_SIGNAL_TYPES,
    SignalTrackingError,
    extract_signal_tracking_request,
    upsert_account_signal_schedule,
)
from apps.api.services.workbook.models import Base, Workbook, WorkbookRow


PROMPT = (
    "Track these accounts weekly for partnership hiring, leadership changes, "
    "funding, and pricing-page changes."
)


@pytest.fixture()
def db_factory():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(
        engine,
        tables=[
            Workbook.__table__,
            WorkbookRow.__table__,
            Job.__table__,
            WatchSubscription.__table__,
            WatchSchedule.__table__,
        ],
    )
    return sessionmaker(bind=engine, autoflush=False, autocommit=False)


def _seed(factory, workspace_id="W1"):
    with factory() as db:
        workbook = Workbook(
            id="wb-accounts",
            workspace_id=workspace_id,
            name="Exact accounts",
            source_type="source",
        )
        db.add(workbook)
        for index in range(2):
            db.add(WorkbookRow(
                workspace_id=workspace_id,
                workbook_id=workbook.id,
                position=index,
                canonical_entity_id=f"account_{index}",
                data={
                    "account_id": f"account_{index}",
                    "company": f"Account {index}",
                    "canonical_domain": f"account-{index}.example",
                },
            ))
        db.commit()


def test_exact_schedule_is_read_back_and_retry_safe(db_factory):
    _seed(db_factory)
    args = {
        "workspace_id": "W1",
        "workbook_id": "wb-accounts",
        "account_ids": ["account_0", "account_1"],
        "cadence": "weekly",
        "signal_types": list(ACCOUNT_SIGNAL_TYPES),
        "idempotency_key": "chat-signals:one",
    }
    with db_factory() as db:
        first = upsert_account_signal_schedule(db, **args)
        retry = upsert_account_signal_schedule(db, **args)

        assert first["persisted"] is True
        assert first["readback_confirmed"] is True
        assert first["scope"]["account_ids"] == ["account_0", "account_1"]
        assert first["signal_types"] == list(ACCOUNT_SIGNAL_TYPES)
        assert first["next_run_at"]
        assert first["state"] == "active"
        assert retry["reused"] is True
        assert retry["schedule_id"] == first["schedule_id"]
        assert db.query(WatchSubscription).count() == 1
        assert db.query(WatchSchedule).count() == 1
        assert db.query(Job).filter(Job.type == "watch_poll").count() == 1


def test_same_scope_updates_one_schedule(db_factory):
    _seed(db_factory)
    with db_factory() as db:
        first = upsert_account_signal_schedule(
            db,
            workspace_id="W1",
            workbook_id="wb-accounts",
            account_ids=["account_0", "account_1"],
            cadence="weekly",
            signal_types=list(ACCOUNT_SIGNAL_TYPES),
            idempotency_key="action-one",
        )
        updated = upsert_account_signal_schedule(
            db,
            workspace_id="W1",
            workbook_id="wb-accounts",
            account_ids=["account_0", "account_1"],
            cadence="daily",
            signal_types=["funding"],
            idempotency_key="action-two",
        )

        assert updated["schedule_id"] == first["schedule_id"]
        assert updated["reused"] is True
        assert updated["updated"] is True
        assert updated["cadence"] == "daily"
        assert updated["signal_types"] == ["funding"]
        assert db.query(WatchSubscription).count() == 1
        assert db.query(Job).filter(
            Job.type == "watch_poll", Job.status == "pending"
        ).count() == 1
        assert db.query(Job).filter(
            Job.type == "watch_poll", Job.status == "cancelled"
        ).count() == 1


def test_selection_drift_and_idempotency_conflicts_fail_closed(db_factory):
    _seed(db_factory)
    with db_factory() as db:
        with pytest.raises(SignalTrackingError, match="missing persisted IDs"):
            upsert_account_signal_schedule(
                db,
                workspace_id="W1",
                workbook_id="wb-accounts",
                account_ids=["account_0", "not_saved"],
                cadence="weekly",
                signal_types=["funding"],
                idempotency_key="drift",
            )
        upsert_account_signal_schedule(
            db,
            workspace_id="W1",
            workbook_id="wb-accounts",
            account_ids=["account_0", "account_1"],
            cadence="weekly",
            signal_types=["funding"],
            idempotency_key="same-key",
        )
        with pytest.raises(SignalTrackingError, match="different tracking request"):
            upsert_account_signal_schedule(
                db,
                workspace_id="W1",
                workbook_id="wb-accounts",
                account_ids=["account_0", "account_1"],
                cadence="daily",
                signal_types=["funding"],
                idempotency_key="same-key",
            )


def test_account_group_collector_records_health_and_diffs():
    watch = SimpleNamespace(
        id="watch-1",
        signal_types=list(ACCOUNT_SIGNAL_TYPES),
        config={
            "accounts": [{
                "account_id": "account_0",
                "company": "Account 0",
                "website": "https://account-0.example",
                "lead_id": 42,
            }],
        },
        cursor={"bootstrapped": False},
    )
    initial = {
        "account_0": {
            "partnership_hiring": {"ok": True, "items": [{"id": "job-1", "title": "Head of Partnerships"}]},
            "leadership_change": {"ok": True, "events": []},
            "funding": {"ok": True, "events": []},
            "pricing_page_change": {"ok": True, "fingerprint": "price-v1", "url": "https://account-0.example/pricing"},
        },
    }
    events, patch = fetch_account_group(
        watch, backfill=False, observations_override=initial
    )
    assert events == []
    assert patch["_collector_failures"] == []
    assert all(
        item["state"] == "healthy"
        for item in patch["collector_health"].values()
    )

    watch.cursor = {
        "bootstrapped": True,
        "account_group": patch["account_group"],
        "collector_health": patch["collector_health"],
    }
    changed = {
        "account_0": {
            "partnership_hiring": {"ok": True, "items": [
                {"id": "job-1", "title": "Head of Partnerships"},
                {"id": "job-2", "title": "Partner Development Manager"},
            ]},
            "leadership_change": {"ok": False, "error_class": "leadership_timeout"},
            "funding": {"ok": True, "events": []},
            "pricing_page_change": {"ok": True, "fingerprint": "price-v2", "url": "https://account-0.example/pricing"},
        },
    }
    events, second_patch = fetch_account_group(
        watch, backfill=False, observations_override=changed
    )
    assert {event.signal_type for event in events} == {
        "partnership_hiring", "pricing_page_change",
    }
    failed = second_patch["collector_health"]["account_0:leadership_change"]
    assert failed == {
        "state": "failed",
        "attempt_count": 2,
        "last_error_class": "leadership_timeout",
    }
    assert second_patch["_collector_failures"] == ["leadership_timeout"]


def test_parser_requires_explicit_account_tracking():
    parsed = extract_signal_tracking_request(PROMPT)
    assert parsed["cadence"] == "weekly"
    assert parsed["signal_types"] == list(ACCOUNT_SIGNAL_TYPES)
    assert extract_signal_tracking_request("Watch the market") is None
