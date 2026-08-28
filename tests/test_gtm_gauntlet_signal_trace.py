import asyncio
import copy
import json
from pathlib import Path

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.models import Job
from apps.api.routers import copilotkit as ck
from apps.api.services.evaluation.gtm_gauntlet import load_artifact, score_gauntlet
from apps.api.services.evaluation.signal_trace import build_signal_tracking_artifact
from apps.api.services.poller.models import WatchSchedule, WatchSubscription
from apps.api.services.signals.tracking import ACCOUNT_SIGNAL_TYPES
from apps.api.services.workbook.models import Base, Workbook, WorkbookRow


PEOPLE_FIXTURE = (
    Path(__file__).parent / "fixtures" / "gtm_gauntlet" / "partnership_people_pass.json"
)


def _factory():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(
        engine,
        tables=[
            Workbook.__table__, WorkbookRow.__table__, Job.__table__,
            WatchSubscription.__table__, WatchSchedule.__table__,
        ],
    )
    factory = sessionmaker(bind=engine, autoflush=False, autocommit=False)
    with factory() as db:
        db.add(Workbook(
            id="wb-g6", workspace_id="W1", name="G6 accounts",
            source_type="account_discovery",
        ))
        for index in range(2):
            db.add(WorkbookRow(
                workbook_id="wb-g6", workspace_id="W1", position=index,
                canonical_entity_id=f"account_{index}",
                data={
                    "account_id": f"account_{index}",
                    "company": f"Account {index}",
                    "canonical_domain": f"account-{index}.example",
                },
            ))
        db.commit()
    return factory


def _action(args, *, retry=False):
    result = json.loads(asyncio.run(ck._execute_tool(
        "track_account_signals", args, store=object(), workspace_id="W1", slug="main",
    )))
    return {
        "step_id": "g6-retry" if retry else "g6-create",
        "workflow_id": "G6",
        "tool_name": "track_account_signals",
        "status": "succeeded" if result.get("ok") else "failed",
        "latency_ms": 75 if retry else 240,
        "args": args,
        "approval": {"decision": "approved"},
        "result": result,
    }


def _passing_artifact(monkeypatch):
    factory = _factory()
    monkeypatch.setattr(ck.settings, "INTENT_POLLER_ENABLED", True)
    monkeypatch.setattr(ck.settings, "PG_LEAD_STORE", True)
    monkeypatch.setattr("apps.api.database.SessionLocal", factory)
    args = {
        "workbook_id": "wb-g6",
        "account_ids": ["account_0", "account_1"],
        "cadence": "weekly",
        "signal_types": list(ACCOUNT_SIGNAL_TYPES),
        "idempotency_key": "chat-signals:recorded-g6",
    }
    trace = {
        "fixture_kind": "synthetic_recorded_native_shape",
        "run_id": "recorded_product_g6",
        "mode": "recorded",
        "build_sha": "fixture-build",
        "workspace_id": "W1",
        "conversation_id": "conv-g6",
        "acknowledgement_ms": 120,
        "prompts": [
            "Track these accounts weekly for partnership hiring, leadership changes, funding, and pricing-page changes."
        ],
        "steps": [],
        "jobs": [],
        "external_writes": [],
        "unresolved_issues": [],
        "production_run_history": [],
    }
    trace["steps"].append(_action(args))
    trace["steps"].append(_action(args, retry=True))
    schedule_id = trace["steps"][0]["result"]["schedule_id"]
    with factory() as db:
        watch = db.query(WatchSubscription).filter(
            WatchSubscription.id == schedule_id
        ).one()
        health = {
            f"account_{index}:{signal}": {
                "state": "healthy", "attempt_count": 1, "last_error_class": None,
            }
            for index in range(2)
            for signal in ACCOUNT_SIGNAL_TYPES
        }
        watch.cursor = {
            **(watch.cursor or {}),
            "attempt_count": 1,
            "collector_health": health,
        }
        db.commit()
        g6 = build_signal_tracking_artifact(trace, db)

    combined = copy.deepcopy(load_artifact(PEOPLE_FIXTURE))
    combined["run_id"] = "recorded_product_g2_g3_g4_g5_g6"
    combined["scenarios"].extend(g6["scenarios"])
    return combined


def test_native_schedule_and_saved_readback_pass_g6(monkeypatch):
    artifact = _passing_artifact(monkeypatch)
    report = score_gauntlet(artifact)

    assert report["score"] == 100.0
    assert report["hard_failures"] == []
    assert report["run_passed"] is True
    assert report["workflows"]["G6"] == {"status": "passed", "failed_checks": []}
    assert report["evaluated_workflows"] == ["G2", "G4", "G5", "G6"]


def test_schedule_scope_drift_and_duplicate_retry_are_hard_failures(monkeypatch):
    artifact = _passing_artifact(monkeypatch)
    scenario = artifact["scenarios"][-1]
    scenario["schedule_readback"]["scope"]["account_ids"] = ["account_0"]
    scenario["schedule_readback"]["scope"]["account_count"] = 1
    scenario["schedule_retry_action"]["schedule_id"] = "duplicate-schedule"
    scenario["schedule_retry_action"]["reused"] = False
    scenario["schedule_readback"]["schedule_count_for_scope"] = 2

    report = score_gauntlet(artifact)
    codes = {item["code"] for item in report["hard_failures"]}
    assert {"tracking_scope_drift", "duplicate_tracking_schedule"}.issubset(codes)
    assert report["workflows"]["G6"]["status"] == "failed"


def test_failed_collector_requires_recovery_metadata(monkeypatch):
    artifact = _passing_artifact(monkeypatch)
    scenario = artifact["scenarios"][-1]
    saved = scenario["schedule_readback"]
    saved["collector_health"]["account_0:funding"] = {
        "state": "failed",
        "attempt_count": 2,
        "last_error_class": "funding_timeout",
    }
    saved["state"] = "degraded"
    saved["last_error_class"] = None
    saved["next_retry_at"] = None
    saved["manual_retry_action"] = {}

    report = score_gauntlet(artifact)
    assert "unrecoverable_signal_collector" in {
        item["code"] for item in report["hard_failures"]
    }
    assert "g6_failed_collector_recovery" in report["workflows"]["G6"]["failed_checks"]
