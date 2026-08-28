from __future__ import annotations

import copy
import json
from pathlib import Path

from apps.api.services.evaluation.gtm_gauntlet import (
    CATEGORY_WEIGHTS,
    REQUIRED_WORKFLOWS,
    load_artifact,
    main,
    score_gauntlet,
)


FIXTURE = (
    Path(__file__).parent
    / "fixtures"
    / "gtm_gauntlet"
    / "partnership_people_pass.json"
)


def _artifact() -> dict:
    return copy.deepcopy(load_artifact(FIXTURE))


def _scenario(artifact: dict) -> dict:
    return artifact["scenarios"][0]


def test_recorded_g2_g4_g5_slice_scores_100_but_does_not_unlock_release():
    report = score_gauntlet(_artifact())

    assert report["score"] == 100.0
    assert report["run_passed"] is True
    assert report["hard_failures"] == []
    assert report["evaluated_workflows"] == ["G2", "G4", "G5"]
    assert report["release"]["eligible"] is False
    assert report["release"]["reason_codes"] == [
        "workflow_coverage_incomplete",
        "production_streak_incomplete",
    ]
    assert sum(CATEGORY_WEIGHTS.values()) == 100
    assert all(
        category["ratio"] == 1.0
        for category in report["category_scores"].values()
    )


def test_wrong_company_person_is_a_hard_failure():
    artifact = _artifact()
    scenario = _scenario(artifact)
    scenario["research"]["people"][0]["canonical_company_domain"] = "namesake.test"
    scenario["verification"]["people"][0]["claims"]["company_identity"]["value"][
        "canonical_domain"
    ] = "namesake.test"

    report = score_gauntlet(artifact)

    assert report["run_passed"] is False
    assert "wrong_company_person" in {
        failure["code"] for failure in report["hard_failures"]
    }
    assert report["workflows"]["G2"]["status"] == "failed"


def test_verified_claim_without_evidence_is_a_hard_failure():
    artifact = _artifact()
    claim = _scenario(artifact)["verification"]["people"][0]["claims"][
        "current_employment"
    ]
    claim["evidence"] = []

    report = score_gauntlet(artifact)

    assert "unsupported_verified_claim" in {
        failure["code"] for failure in report["hard_failures"]
    }
    assert report["workflows"]["G4"]["status"] == "failed"


def test_former_employee_cannot_be_labeled_current_and_verified():
    artifact = _artifact()
    claim = _scenario(artifact)["verification"]["people"][0]["claims"][
        "current_employment"
    ]
    claim["value"] = False

    report = score_gauntlet(artifact)

    assert "former_employee_verified" in {
        failure["code"] for failure in report["hard_failures"]
    }


def test_success_without_persisted_workbook_is_a_hard_failure():
    artifact = _artifact()
    _scenario(artifact)["workbook_action"]["persisted"] = False

    report = score_gauntlet(artifact)

    assert "success_without_persistence" in {
        failure["code"] for failure in report["hard_failures"]
    }
    assert report["workflows"]["G5"]["status"] == "failed"


def test_retry_that_creates_another_workbook_is_a_hard_failure():
    artifact = _artifact()
    retry = _scenario(artifact)["retry_action"]
    retry["workbook_id"] = "wb_duplicate"
    retry["url"] = "/workbooks/wb_duplicate"
    retry["reused"] = False

    report = score_gauntlet(artifact)

    assert "duplicate_retry_write" in {
        failure["code"] for failure in report["hard_failures"]
    }


def test_unrelated_function_cannot_be_labeled_verified():
    artifact = _artifact()
    claim = _scenario(artifact)["verification"]["people"][0]["claims"][
        "partnership_function"
    ]
    claim["value"] = False

    report = score_gauntlet(artifact)

    assert "unrelated_function_verified" in {
        failure["code"] for failure in report["hard_failures"]
    }


def test_verified_claim_with_unresolved_contradiction_is_a_hard_failure():
    artifact = _artifact()
    claim = _scenario(artifact)["verification"]["people"][0]["claims"]["title"]
    claim["contradictions"] = [
        {
            "url": "https://evidence.example.test/stripe/conflicting-title",
            "value": "Former employee",
        }
    ]

    report = score_gauntlet(artifact)

    assert "contradicted_verified_claim" in {
        failure["code"] for failure in report["hard_failures"]
    }


def test_queued_job_without_recovery_metadata_is_a_hard_failure():
    artifact = _artifact()
    _scenario(artifact)["jobs"] = [{"job_id": "job_frozen", "status": "queued"}]

    report = score_gauntlet(artifact)

    assert "unrecoverable_stuck_job" in {
        failure["code"] for failure in report["hard_failures"]
    }


def test_unapproved_external_write_is_a_hard_failure():
    artifact = _artifact()
    _scenario(artifact)["external_writes"] = [
        {"kind": "outreach_send", "status": "sent", "approved": False}
    ]

    report = score_gauntlet(artifact)

    assert "unapproved_external_write" in {
        failure["code"] for failure in report["hard_failures"]
    }


def test_wrong_workbook_link_is_a_hard_failure():
    artifact = _artifact()
    _scenario(artifact)["workbook_action"]["url"] = "/workbooks/wb_other"

    report = score_gauntlet(artifact)

    assert "wrong_result_link" in {
        failure["code"] for failure in report["hard_failures"]
    }


def test_cross_workspace_action_state_is_a_hard_failure():
    artifact = _artifact()
    _scenario(artifact)["workbook_action"]["workspace_id"] = "ws_other"

    report = score_gauntlet(artifact)

    assert "cross_workspace_state" in {
        failure["code"] for failure in report["hard_failures"]
    }


def test_declaring_unimplemented_workflows_cannot_unlock_release():
    artifact = _artifact()
    _scenario(artifact)["workflow_ids"] = list(REQUIRED_WORKFLOWS)

    report = score_gauntlet(artifact)

    assert report["declared_workflows"] == list(REQUIRED_WORKFLOWS)
    assert report["evaluated_workflows"] == ["G2", "G4", "G5"]
    assert report["workflows"]["G1"]["status"] == "not_evaluated"
    assert report["release"]["eligible"] is False


def test_honest_partial_result_loses_points_without_fabrication_failure():
    artifact = _artifact()
    scenario = _scenario(artifact)
    scenario["status"] = "partial"
    scenario["workbook_action"] = {
        "status": "failed",
        "persisted": False,
        "approved": True,
        "selected_person_ids": ["person_fixture_partner_01"],
    }
    scenario["retry_action"] = {
        "status": "failed",
        "persisted": False,
        "approved": True,
        "selected_person_ids": ["person_fixture_partner_01"],
    }

    report = score_gauntlet(artifact)

    assert report["score"] < 95
    assert report["run_passed"] is False
    assert report["hard_failures"] == []
    assert report["workflows"]["G5"]["status"] == "failed"


def test_cli_writes_machine_readable_report_and_can_enforce_release(tmp_path):
    output = tmp_path / "gauntlet-report.json"

    assert main(["--input", str(FIXTURE), "--output", str(output)]) == 0
    report = json.loads(output.read_text(encoding="utf-8"))
    assert report["run_passed"] is True
    assert report["release"]["eligible"] is False

    assert main(["--input", str(FIXTURE), "--require-release"]) == 1
