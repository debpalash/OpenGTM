"""Combined native-action validation for all seven release workflows.

Provider responses are recorded seams, but approvals, action execution,
idempotency, persistence, readback adapters, tenant checks, and scoring are the
same product code used at runtime. These runs are deliberately classified as
``local_native`` and cannot satisfy the controlled-live release streak.
"""

from __future__ import annotations

from pathlib import Path

from apps.api.services.evaluation.chat_trace import build_people_workflow_artifact
from apps.api.services.evaluation.gtm_gauntlet import REQUIRED_WORKFLOWS, score_gauntlet
from tests.test_gtm_gauntlet_account_trace import _passing_artifact as g1_artifact
from tests.test_gtm_gauntlet_chat_trace import _real_recorded_trace
from tests.test_gtm_gauntlet_outreach_trace import _passing_artifact as g7_artifact
from tests.test_gtm_gauntlet_signal_trace import _passing_artifact as g6_artifact


def _run_dir(root: Path, name: str) -> Path:
    path = root / name
    path.mkdir(parents=True)
    return path


def _combined_native_artifact(monkeypatch, root: Path, run_number: int) -> dict:
    g1 = g1_artifact(monkeypatch, _run_dir(root, "g1"))

    people_trace, people_factory = _real_recorded_trace(
        monkeypatch,
        _run_dir(root, "people"),
    )
    with people_factory() as db:
        people = build_people_workflow_artifact(people_trace, db)

    g6 = g6_artifact(monkeypatch)
    g7 = g7_artifact(monkeypatch, _run_dir(root, "g7"))

    return {
        "schema_version": "1.0",
        "fixture_kind": "recorded_providers_native_actions",
        "validation_tier": "local_native",
        "run_id": f"local-native-all-workflows-{run_number}",
        "mode": "recorded",
        "build_sha": "working-tree",
        "started_at": f"2026-08-28T12:{run_number:02d}:00Z",
        "finished_at": f"2026-08-28T12:{run_number:02d}:30Z",
        "unresolved_issues": [],
        "production_run_history": [],
        "local_validation_history": [],
        "scenarios": [
            g1["scenarios"][0],
            people["scenarios"][0],
            g6["scenarios"][-1],
            g7["scenarios"][-1],
        ],
    }


def _local_run_record(artifact: dict, report: dict) -> dict:
    category_floor_met = all(
        category["ratio"] >= report["category_floor"]
        for category in report["category_scores"].values()
    )
    return {
        "run_id": artifact["run_id"],
        "validation_tier": "local_native",
        "mode": "recorded",
        "production_like": False,
        "passed": report["run_passed"],
        "score": report["score"],
        "category_floor_met": category_floor_met,
        "passed_workflows": report["release"]["passed_workflows"],
        "hard_failures": report["hard_failures"],
        "unresolved_high_priority_issues": report["release"][
            "unresolved_high_priority_issues"
        ],
        "build_sha": artifact["build_sha"],
        "finished_at": artifact["finished_at"],
        "workspace_id": "W1",
        "external_sends_blocked": True,
        "provider_health_recorded": False,
        "dedicated_workspace": False,
    }


def test_ten_run_local_native_streak_passes_all_g1_through_g7(monkeypatch, tmp_path):
    history = []
    final_artifact = None
    for run_number in range(1, 11):
        artifact = _combined_native_artifact(
            monkeypatch,
            _run_dir(tmp_path, f"run-{run_number}"),
            run_number,
        )
        report = score_gauntlet(artifact)
        assert report["score"] == 100.0
        assert report["run_passed"] is True
        assert report["hard_failures"] == []
        assert report["evaluated_workflows"] == list(REQUIRED_WORKFLOWS)
        assert report["release"]["passed_workflows"] == list(REQUIRED_WORKFLOWS)
        history.append(_local_run_record(artifact, report))
        final_artifact = artifact

    assert final_artifact is not None
    final_artifact["local_validation_history"] = history
    final_report = score_gauntlet(final_artifact)
    assert final_report["release"]["consecutive_local_native_passes"] == 10
    assert final_report["release"]["consecutive_production_like_passes"] == 0
    assert final_report["release"]["eligible"] is False
    assert final_report["release"]["reason_codes"] == ["production_streak_incomplete"]
