import json
from types import SimpleNamespace

import pytest

from apps.api.database import SessionLocal
from apps.api.models import Job
from apps.api.services.queue_load import CONFIRMATION, run_queue_load_test
from apps.api.cli import cmd_queue_load_test


@pytest.fixture(autouse=True)
def clean_queue():
    with SessionLocal() as db:
        db.query(Job).delete()
        db.commit()
    yield
    with SessionLocal() as db:
        db.query(Job).delete()
        db.commit()


def test_load_harness_measures_invariants_and_cleans_up():
    report = run_queue_load_test(
        jobs=60, tenants=6, claimers=8, tenant_cap=2, hold_ms=1,
        confirmation=CONFIRMATION, allow_sqlite=True,
    )
    assert report["ok"] and report["completed"] == 60
    assert report["duplicate_claims"] == 0 and not report["cap_violations"]
    assert max(report["max_active_by_tenant"].values()) <= 2
    assert report["claim_latency_ms"]["p95"] >= 0
    with SessionLocal() as db:
        assert db.query(Job).count() == 0


def test_load_harness_requires_confirmation_and_quiescent_queue():
    with pytest.raises(ValueError, match="confirmation"):
        run_queue_load_test(
            jobs=1, tenants=1, claimers=1, tenant_cap=1,
            confirmation="", allow_sqlite=True,
        )
    with SessionLocal() as db:
        db.add(Job(type="real", payload={}, status="pending"))
        db.commit()
    with pytest.raises(RuntimeError, match="no pending"):
        run_queue_load_test(
            jobs=1, tenants=1, claimers=1, tenant_cap=1,
            confirmation=CONFIRMATION, allow_sqlite=True,
        )


def test_queue_load_cli_writes_clean_json_artifact(tmp_path):
    output = tmp_path / "queue-scale.json"
    cmd_queue_load_test(SimpleNamespace(
        jobs=20, tenants=4, claimers=4, tenant_cap=2, hold_ms=1,
        confirm=CONFIRMATION, allow_sqlite=True, output=str(output),
    ))

    report = json.loads(output.read_text(encoding="utf-8"))
    assert report["gate"] == "queue_scale" and report["completed"] == 20
