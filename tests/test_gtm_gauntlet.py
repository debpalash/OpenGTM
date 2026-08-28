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


def _add_passing_g3(artifact: dict) -> dict:
    scenario = _scenario(artifact)
    scenario["workflow_ids"] = ["G2", "G3", "G4", "G5"]
    person_id = scenario["selection"]["person_ids"][0]
    attempts = [
        {
            "stage": "discovery",
            "provider": "prospeo",
            "status": "found",
            "detail": "linkedin",
            "source_license": "proprietary-api",
        },
        {
            "stage": "verification",
            "provider": "reacher",
            "status": "valid",
            "detail": "recorded_fixture",
            "source_license": "proprietary-api",
        },
    ]
    contactability = {
        "email": "fixture.partner@stripe.com",
        "status": "verified",
        "finder_provider": "prospeo",
        "verifier_provider": "reacher",
        "discovery_confidence": 0.9,
        "verification_status": "valid",
        "verification_confidence": 0.95,
        "observed_at": "2026-08-28T07:00:16Z",
        "attempts": attempts,
        "exhausted": False,
    }
    action = {
        "action_id": "contact-fixture-stripe-partners",
        "idempotency_key": "contact-fixture-stripe-partners",
        "workspace_id": scenario["workspace_id"],
        "approved": True,
        "status": "succeeded",
        "reused": False,
        "selected_person_ids": [person_id],
        "returned_person_ids": [person_id],
        "provider_order": ["prospeo", "hunter_io"],
        "people": [{"person_id": person_id, "contactability": contactability}],
        "summary": {
            "verified": 1,
            "risky": 0,
            "catch_all": 0,
            "invalid": 0,
            "unavailable": 0,
        },
    }
    scenario["contact_action"] = action
    scenario["contact_retry_action"] = {
        **copy.deepcopy(action),
        "reused": True,
    }
    scenario["verification"]["people"][0]["claims"]["contactability"] = {
        "status": "verified",
        "value": {
            "email": "fixture.partner@stripe.com",
            "contact_status": "verified",
        },
        "confidence": 0.95,
        "observed_at": "2026-08-28T07:00:16Z",
        "evidence": [
            {
                "kind": "provider_attempt",
                "source": attempt["provider"],
                "stage": attempt["stage"],
                "status": attempt["status"],
                "observed_at": "2026-08-28T07:00:16Z",
            }
            for attempt in attempts
        ],
        "contradictions": [],
    }
    scenario["verification"]["summary"] = {
        "passed": 5,
        "failed": 0,
        "uncertain": 0,
        "changed": 0,
    }
    scenario["timings_ms"]["contact_enrichment"] = 1400
    return scenario


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


def test_recorded_g2_g3_g4_g5_slice_scores_100():
    artifact = _artifact()
    _add_passing_g3(artifact)

    report = score_gauntlet(artifact)

    assert report["score"] == 100.0
    assert report["run_passed"] is True
    assert report["hard_failures"] == []
    assert report["evaluated_workflows"] == ["G2", "G3", "G4", "G5"]
    assert report["workflows"]["G3"]["status"] == "passed"


def test_email_cannot_be_verified_without_exact_discovery_and_verifier():
    artifact = _artifact()
    scenario = _add_passing_g3(artifact)
    contact = scenario["contact_action"]["people"][0]["contactability"]
    contact["attempts"] = [{
        "stage": "discovery",
        "provider": "hunter_io",
        "status": "found",
        "detail": "domain_search",
    }]

    report = score_gauntlet(artifact)

    codes = {failure["code"] for failure in report["hard_failures"]}
    assert "contact_verified_without_verifier" in codes
    assert "non_exact_email_verified" in codes
    assert report["workflows"]["G3"]["status"] == "failed"


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
    assert report["evaluated_workflows"] == ["G1", "G2", "G3", "G4", "G5"]
    assert report["workflows"]["G1"]["status"] == "failed"
    assert report["workflows"]["G3"]["status"] == "failed"
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
