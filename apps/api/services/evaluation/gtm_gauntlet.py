"""Machine-scored workflow gauntlet for OpenGTM.

The scorer consumes recorded or live run artifacts. It never scores assistant
prose. Every point comes from structured evidence, action state, or measured
latency in the artifact. Hard failures independently fail a run even when its
weighted score would otherwise pass.
"""

from __future__ import annotations

import argparse
import json
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Iterable, Mapping, Sequence
from urllib.parse import urlsplit


CATEGORY_WEIGHTS: dict[str, int] = {
    "outcome_completion": 30,
    "accuracy_and_evidence": 25,
    "actionability": 15,
    "reliability": 15,
    "speed": 10,
    "ux_clarity": 5,
}

REQUIRED_WORKFLOWS: tuple[str, ...] = (
    "G1",
    "G2",
    "G3",
    "G4",
    "G5",
    "G6",
    "G7",
)
SUPPORTED_WORKFLOWS: tuple[str, ...] = ("G2", "G4", "G5")
RELEASE_SCORE = 95.0
CATEGORY_FLOOR = 0.90
REQUIRED_PRODUCTION_STREAK = 10

_TERMINAL_SCENARIO_STATES = {"completed", "partial", "failed", "cancelled", "timed_out"}
_TERMINAL_ACTION_STATES = {"succeeded", "failed", "cancelled", "timed_out"}
_CLAIM_STATES = {"verified", "uncertain", "unavailable", "rejected", "failed", "contradicted"}
_REQUIRED_CLAIMS = {
    "company_identity",
    "current_employment",
    "title",
    "partnership_function",
    "contactability",
}


@dataclass(frozen=True)
class _Check:
    check_id: str
    category: str
    points: int
    passed: bool
    detail: str
    workflow_ids: tuple[str, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.check_id,
            "passed": self.passed,
            "earned": self.points if self.passed else 0,
            "possible": self.points,
            "detail": self.detail,
            "workflow_ids": list(self.workflow_ids),
        }


def load_artifact(path: str | Path) -> dict[str, Any]:
    """Load one gauntlet artifact and enforce a JSON object at the root."""
    with Path(path).open("r", encoding="utf-8") as handle:
        artifact = json.load(handle)
    if not isinstance(artifact, dict):
        raise ValueError("gauntlet artifact must be a JSON object")
    return artifact


def _dict(value: Any) -> dict[str, Any]:
    return value if isinstance(value, dict) else {}


def _list(value: Any) -> list[Any]:
    return value if isinstance(value, list) else []


def _text(value: Any) -> str:
    return str(value).strip() if value is not None else ""


def _domain(value: Any) -> str:
    raw = _text(value).lower()
    if not raw:
        return ""
    parsed = urlsplit(raw if "://" in raw else f"https://{raw}")
    host = (parsed.hostname or "").lower().rstrip(".")
    return host[4:] if host.startswith("www.") else host


def _is_http_url(value: Any) -> bool:
    parsed = urlsplit(_text(value))
    return parsed.scheme in {"http", "https"} and bool(parsed.netloc)


def _is_confidence(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and 0 <= value <= 1


def _all_scenarios(
    scenarios: Sequence[dict[str, Any]],
    predicate: Callable[[dict[str, Any]], bool],
) -> bool:
    return bool(scenarios) and all(predicate(scenario) for scenario in scenarios)


def _person_ids(people: Any) -> list[str]:
    return [_text(person.get("person_id")) for person in _list(people) if isinstance(person, dict)]


def _selection_ids(scenario: Mapping[str, Any]) -> list[str]:
    return [_text(value) for value in _list(_dict(scenario.get("selection")).get("person_ids"))]


def _claims_by_person(scenario: Mapping[str, Any]) -> dict[str, dict[str, Any]]:
    people = _list(_dict(scenario.get("verification")).get("people"))
    return {
        _text(person.get("person_id")): _dict(person.get("claims"))
        for person in people
        if isinstance(person, dict) and _text(person.get("person_id"))
    }


def _claim_has_evidence(claim: Mapping[str, Any]) -> bool:
    evidence = _list(claim.get("evidence"))
    return bool(evidence) and all(
        isinstance(item, dict)
        and _is_http_url(item.get("url"))
        and bool(_text(item.get("source")))
        for item in evidence
    )


def _claim_is_complete(claim: Mapping[str, Any]) -> bool:
    return (
        _text(claim.get("status")) in _CLAIM_STATES
        and _is_confidence(claim.get("confidence"))
        and bool(_text(claim.get("observed_at")))
        and isinstance(claim.get("contradictions"), list)
        and _claim_has_evidence(claim)
    )


def _selected_claims(
    scenario: Mapping[str, Any],
    claim_name: str,
) -> list[dict[str, Any]]:
    by_person = _claims_by_person(scenario)
    selected = _selection_ids(scenario)
    return [
        _dict(by_person.get(person_id, {}).get(claim_name))
        for person_id in selected
    ]


def _target_resolved(scenario: Mapping[str, Any]) -> bool:
    target = _dict(scenario.get("target"))
    return (
        _text(target.get("resolution_status")) == "resolved"
        and bool(_text(target.get("company")))
        and bool(_domain(target.get("canonical_domain")))
    )


def _accepted_people_match_target(scenario: Mapping[str, Any]) -> bool:
    target_domain = _domain(_dict(scenario.get("target")).get("canonical_domain"))
    people = _list(_dict(scenario.get("research")).get("people"))
    return bool(target_domain and people) and all(
        isinstance(person, dict)
        and _domain(person.get("canonical_company_domain")) == target_domain
        for person in people
    )


def _stable_person_ids(scenario: Mapping[str, Any]) -> bool:
    research_ids = _person_ids(_dict(scenario.get("research")).get("people"))
    verification_ids = _person_ids(_dict(scenario.get("verification")).get("people"))
    selected_ids = _selection_ids(scenario)
    return (
        bool(research_ids and verification_ids and selected_ids)
        and all(research_ids)
        and all(verification_ids)
        and all(selected_ids)
        and len(research_ids) == len(set(research_ids))
        and len(verification_ids) == len(set(verification_ids))
        and len(selected_ids) == len(set(selected_ids))
        and set(selected_ids).issubset(research_ids)
        and set(selected_ids).issubset(verification_ids)
    )


def _claim_set_complete(scenario: Mapping[str, Any]) -> bool:
    by_person = _claims_by_person(scenario)
    selected = _selection_ids(scenario)
    if not selected:
        return False
    for person_id in selected:
        claims = by_person.get(person_id, {})
        if not _REQUIRED_CLAIMS.issubset(claims):
            return False
        if not all(_claim_is_complete(_dict(claims[name])) for name in _REQUIRED_CLAIMS):
            return False
    return True


def _claim_verified_with_evidence(scenario: Mapping[str, Any], name: str) -> bool:
    claims = _selected_claims(scenario, name)
    return bool(claims) and all(
        _text(claim.get("status")) == "verified" and _claim_has_evidence(claim)
        for claim in claims
    )


def _verification_covers_selection(scenario: Mapping[str, Any]) -> bool:
    selected = _selection_ids(scenario)
    verified = _person_ids(_dict(scenario.get("verification")).get("people"))
    return bool(selected) and len(verified) == len(set(verified)) and set(verified) == set(selected)


def _workbook_persisted(scenario: Mapping[str, Any]) -> bool:
    action = _dict(scenario.get("workbook_action"))
    return (
        _text(action.get("status")) == "succeeded"
        and action.get("persisted") is True
        and bool(_text(action.get("workbook_id")))
    )


def _exact_workbook_rows(scenario: Mapping[str, Any]) -> bool:
    action = _dict(scenario.get("workbook_action"))
    selected = _selection_ids(scenario)
    action_selected = [_text(value) for value in _list(action.get("selected_person_ids"))]
    persisted = [_text(value) for value in _list(action.get("persisted_person_ids"))]
    return (
        _workbook_persisted(scenario)
        and bool(selected)
        and action_selected == selected
        and persisted == selected
        and action.get("row_count") == len(selected)
    )


def _usable_person_rows(scenario: Mapping[str, Any]) -> bool:
    people = _list(_dict(scenario.get("research")).get("people"))
    return bool(people) and all(
        isinstance(person, dict)
        and all(
            bool(_text(person.get(field)))
            for field in ("person_id", "name", "title", "public_profile_url", "retrieved_at")
        )
        and _is_http_url(person.get("public_profile_url"))
        and _is_confidence(person.get("confidence"))
        for person in people
    )


def _receipt_complete(scenario: Mapping[str, Any]) -> bool:
    action = _dict(scenario.get("workbook_action"))
    workbook_id = _text(action.get("workbook_id"))
    return (
        _workbook_persisted(scenario)
        and bool(_text(action.get("action_id")))
        and isinstance(action.get("row_count"), int)
        and isinstance(action.get("skipped_count"), int)
        and _text(action.get("url")) == f"/workbooks/{workbook_id}"
    )


def _write_approved(scenario: Mapping[str, Any]) -> bool:
    return _dict(scenario.get("workbook_action")).get("approved") is True


def _has_idempotency_key(scenario: Mapping[str, Any]) -> bool:
    action = _dict(scenario.get("workbook_action"))
    retry = _dict(scenario.get("retry_action"))
    key = _text(action.get("idempotency_key"))
    return bool(key) and _text(retry.get("idempotency_key")) == key


def _retry_reused(scenario: Mapping[str, Any]) -> bool:
    action = _dict(scenario.get("workbook_action"))
    retry = _dict(scenario.get("retry_action"))
    return (
        _text(action.get("status")) == "succeeded"
        and _text(retry.get("status")) == "succeeded"
        and action.get("persisted") is True
        and retry.get("persisted") is True
        and retry.get("reused") is True
        and bool(_text(action.get("workbook_id")))
        and _text(retry.get("workbook_id")) == _text(action.get("workbook_id"))
        and _text(retry.get("action_id")) == _text(action.get("action_id"))
        and _list(retry.get("persisted_person_ids")) == _list(action.get("persisted_person_ids"))
        and retry.get("row_count") == action.get("row_count")
    )


def _no_duplicate_rows(scenario: Mapping[str, Any]) -> bool:
    action = _dict(scenario.get("workbook_action"))
    persisted = [_text(value) for value in _list(action.get("persisted_person_ids"))]
    return bool(persisted) and len(persisted) == len(set(persisted))


def _terminal_states(scenario: Mapping[str, Any]) -> bool:
    return (
        _text(scenario.get("status")) in _TERMINAL_SCENARIO_STATES
        and _text(_dict(scenario.get("research")).get("status")) in _TERMINAL_SCENARIO_STATES
        and _text(_dict(scenario.get("verification")).get("status")) in _TERMINAL_SCENARIO_STATES
        and _text(_dict(scenario.get("workbook_action")).get("status")) in _TERMINAL_ACTION_STATES
        and _text(_dict(scenario.get("retry_action")).get("status")) in _TERMINAL_ACTION_STATES
    )


def _within_timing(scenario: Mapping[str, Any], name: str, maximum_ms: int) -> bool:
    value = _dict(scenario.get("timings_ms")).get(name)
    return isinstance(value, (int, float)) and not isinstance(value, bool) and 0 <= value <= maximum_ms


def _verification_summary_complete(scenario: Mapping[str, Any]) -> bool:
    verification = _dict(scenario.get("verification"))
    summary = _dict(verification.get("summary"))
    keys = ("passed", "failed", "uncertain", "changed")
    if not all(isinstance(summary.get(key), int) and summary[key] >= 0 for key in keys):
        return False
    claim_count = sum(
        len(_dict(person.get("claims")))
        for person in _list(verification.get("people"))
        if isinstance(person, dict)
    )
    return summary["passed"] + summary["failed"] + summary["uncertain"] == claim_count


def _production_streak(artifact: Mapping[str, Any]) -> int:
    streak = 0
    for run in reversed(_list(artifact.get("production_run_history"))):
        if not isinstance(run, dict):
            break
        if run.get("production_like") is True and run.get("passed") is True and not run.get("hard_failures"):
            streak += 1
        else:
            break
    return streak


def _high_priority_issues(artifact: Mapping[str, Any]) -> list[dict[str, Any]]:
    return [
        issue
        for issue in _list(artifact.get("unresolved_issues"))
        if isinstance(issue, dict) and _text(issue.get("severity")).upper() in {"P0", "P1"}
    ]


def _hard_failures(
    artifact: Mapping[str, Any],
    scenarios: Sequence[dict[str, Any]],
) -> list[dict[str, Any]]:
    failures: list[dict[str, Any]] = []
    seen: set[tuple[str, str, str, str]] = set()

    def add(
        code: str,
        scenario: Mapping[str, Any] | None,
        detail: str,
        workflow_ids: Iterable[str],
        *,
        entity_id: str = "",
        claim: str = "",
    ) -> None:
        scenario_id = _text((scenario or {}).get("id")) or "artifact"
        dedupe_key = (code, scenario_id, entity_id, claim)
        if dedupe_key in seen:
            return
        seen.add(dedupe_key)
        failures.append(
            {
                "code": code,
                "scenario_id": scenario_id,
                "entity_id": entity_id or None,
                "claim": claim or None,
                "detail": detail,
                "workflow_ids": list(workflow_ids),
            }
        )

    if (
        _text(artifact.get("schema_version")) != "1.0"
        or not _text(artifact.get("run_id"))
        or _text(artifact.get("mode")) not in {"recorded", "live"}
        or not scenarios
    ):
        add(
            "invalid_artifact",
            None,
            "The artifact is missing its supported schema, run identity, mode, or scenarios.",
            REQUIRED_WORKFLOWS,
        )

    for scenario in scenarios:
        scenario_workflows = tuple(
            workflow
            for workflow in _list(scenario.get("workflow_ids"))
            if workflow in REQUIRED_WORKFLOWS
        )
        target_domain = _domain(_dict(scenario.get("target")).get("canonical_domain"))

        for person in _list(_dict(scenario.get("research")).get("people")):
            if not isinstance(person, dict):
                continue
            person_id = _text(person.get("person_id"))
            person_domain = _domain(person.get("canonical_company_domain"))
            if target_domain and person_domain != target_domain:
                add(
                    "wrong_company_person",
                    scenario,
                    f"Accepted person {person_id or '<missing>'} does not match {target_domain}.",
                    ("G2",),
                    entity_id=person_id,
                )

        for person_id, claims in _claims_by_person(scenario).items():
            for claim_name, raw_claim in claims.items():
                claim = _dict(raw_claim)
                if _text(claim.get("status")) != "verified":
                    continue
                if not _claim_has_evidence(claim):
                    add(
                        "unsupported_verified_claim",
                        scenario,
                        f"Verified claim {claim_name} has no complete supporting evidence.",
                        ("G4",),
                        entity_id=person_id,
                        claim=claim_name,
                    )
                if _list(claim.get("contradictions")):
                    add(
                        "contradicted_verified_claim",
                        scenario,
                        f"Verified claim {claim_name} still has unresolved contradictions.",
                        ("G4",),
                        entity_id=person_id,
                        claim=claim_name,
                    )

            company_claim = _dict(claims.get("company_identity"))
            company_value = _dict(company_claim.get("value"))
            if (
                _text(company_claim.get("status")) == "verified"
                and target_domain
                and _domain(company_value.get("canonical_domain")) != target_domain
            ):
                add(
                    "wrong_company_person",
                    scenario,
                    f"Verified company claim for {person_id} does not match {target_domain}.",
                    ("G2", "G4"),
                    entity_id=person_id,
                    claim="company_identity",
                )

            employment = _dict(claims.get("current_employment"))
            if _text(employment.get("status")) == "verified" and employment.get("value") is not True:
                add(
                    "former_employee_verified",
                    scenario,
                    f"Person {person_id} is labeled current despite a non-current employment value.",
                    ("G2", "G4"),
                    entity_id=person_id,
                    claim="current_employment",
                )

            function = _dict(claims.get("partnership_function"))
            if _text(function.get("status")) == "verified" and function.get("value") is not True:
                add(
                    "unrelated_function_verified",
                    scenario,
                    f"Person {person_id} is labeled a partnership match despite a negative function value.",
                    ("G2", "G4"),
                    entity_id=person_id,
                    claim="partnership_function",
                )

        action = _dict(scenario.get("workbook_action"))
        retry = _dict(scenario.get("retry_action"))
        if _text(action.get("status")) == "succeeded" and action.get("persisted") is not True:
            add(
                "success_without_persistence",
                scenario,
                "Workbook action reports success without persisted state.",
                ("G5",),
            )

        if _text(action.get("status")) == "succeeded" and action.get("persisted") is True:
            selected = _selection_ids(scenario)
            persisted = [_text(value) for value in _list(action.get("persisted_person_ids"))]
            if persisted != selected or action.get("row_count") != len(selected):
                add(
                    "persisted_state_mismatch",
                    scenario,
                    "Workbook success receipt does not match the exact selected person IDs.",
                    ("G5",),
                )
            if len(persisted) != len(set(persisted)):
                add(
                    "duplicate_persisted_rows",
                    scenario,
                    "Workbook contains duplicate persisted person IDs.",
                    ("G5",),
                )
            workbook_id = _text(action.get("workbook_id"))
            if _text(action.get("url")) != f"/workbooks/{workbook_id}":
                add(
                    "wrong_result_link",
                    scenario,
                    "Workbook receipt URL does not open the persisted workbook.",
                    ("G5",),
                )

        if _text(retry.get("status")) == "succeeded" and (
            retry.get("reused") is not True
            or _text(retry.get("workbook_id")) != _text(action.get("workbook_id"))
            or _text(retry.get("idempotency_key")) != _text(action.get("idempotency_key"))
        ):
            add(
                "duplicate_retry_write",
                scenario,
                "Retry did not reuse the original workbook and idempotency key.",
                ("G5",),
            )

        workspace_id = _text(scenario.get("workspace_id"))
        for candidate in (action, retry):
            candidate_workspace = _text(candidate.get("workspace_id"))
            if candidate_workspace and workspace_id and candidate_workspace != workspace_id:
                add(
                    "cross_workspace_state",
                    scenario,
                    "Action state belongs to a different workspace.",
                    ("G5",),
                )

        for external_write in _list(scenario.get("external_writes")):
            if not isinstance(external_write, dict):
                continue
            performed = external_write.get("performed") is True or _text(
                external_write.get("status")
            ) in {"applied", "succeeded", "sent"}
            if performed and external_write.get("approved") is not True:
                add(
                    "unapproved_external_write",
                    scenario,
                    "An external write occurred without explicit approval.",
                    scenario_workflows,
                )

        for job in _list(scenario.get("jobs")):
            if not isinstance(job, dict) or _text(job.get("status")) not in {"queued", "running"}:
                continue
            if not all(_text(job.get(field)) for field in ("heartbeat_at", "timeout_at", "recovery_action")):
                add(
                    "unrecoverable_stuck_job",
                    scenario,
                    "A queued or running job lacks heartbeat, timeout, or recovery metadata.",
                    scenario_workflows,
                    entity_id=_text(job.get("job_id")),
                )

    return failures


def _build_checks(scenarios: Sequence[dict[str, Any]]) -> list[_Check]:
    checks: list[_Check] = []

    def add(
        check_id: str,
        category: str,
        points: int,
        predicate: Callable[[dict[str, Any]], bool],
        detail: str,
        workflow_ids: tuple[str, ...],
    ) -> None:
        relevant_scenarios = [
            scenario
            for scenario in scenarios
            if set(_list(scenario.get("workflow_ids"))).intersection(workflow_ids)
        ]
        checks.append(
            _Check(
                check_id=check_id,
                category=category,
                points=points,
                passed=_all_scenarios(relevant_scenarios, predicate),
                detail=detail,
                workflow_ids=workflow_ids,
            )
        )

    add("scenario_completed", "outcome_completion", 5,
        lambda s: _text(s.get("status")) == "completed",
        "The scenario reached completed state.", ("G2", "G4", "G5"))
    add("accepted_people_found", "outcome_completion", 5,
        lambda s: bool(_list(_dict(s.get("research")).get("people"))),
        "The people research produced an accepted result set.", ("G2",))
    add("verification_covers_selection", "outcome_completion", 5,
        _verification_covers_selection,
        "Verification covers the exact selected people.", ("G4",))
    add("workbook_persisted", "outcome_completion", 8, _workbook_persisted,
        "The requested workbook exists in persisted state.", ("G5",))
    add("exact_workbook_rows", "outcome_completion", 7, _exact_workbook_rows,
        "Workbook rows exactly match the selected person IDs.", ("G5",))

    add("target_company_resolved", "accuracy_and_evidence", 5, _target_resolved,
        "The target company is explicitly resolved to a canonical domain.", ("G2",))
    add("accepted_people_match_target", "accuracy_and_evidence", 5,
        _accepted_people_match_target,
        "Every accepted person matches the target company domain.", ("G2",))
    add("stable_person_ids", "accuracy_and_evidence", 4, _stable_person_ids,
        "Stable person IDs survive research, selection, and verification.", ("G2", "G4"))
    add("employment_evidence", "accuracy_and_evidence", 4,
        lambda s: _claim_verified_with_evidence(s, "current_employment"),
        "Current employment is a separately evidenced verified claim.", ("G2", "G4"))
    add("function_evidence", "accuracy_and_evidence", 4,
        lambda s: _claim_verified_with_evidence(s, "partnership_function"),
        "Partnership function is a separately evidenced verified claim.", ("G2", "G4"))
    add("independent_claim_contract", "accuracy_and_evidence", 3, _claim_set_complete,
        "All required claims carry status, evidence, time, confidence, and contradictions.", ("G4",))

    add("explicit_selection", "actionability", 4,
        lambda s: bool(_selection_ids(s)),
        "The action targets explicit stable person IDs.", ("G2", "G5"))
    add("usable_person_rows", "actionability", 3, _usable_person_rows,
        "People rows contain the fields required for inspection and action.", ("G2",))
    add("complete_action_receipt", "actionability", 5, _receipt_complete,
        "The write receipt contains identity, counts, state, and a correct link.", ("G5",))
    add("can_continue_enrichment", "actionability", 3,
        lambda s: s.get("can_continue_enrichment") is True,
        "The saved selection can continue into enrichment without rediscovery.", ("G5",))

    add("explicit_write_approval", "reliability", 3, _write_approved,
        "The workbook write records explicit approval.", ("G5",))
    add("idempotency_key_recorded", "reliability", 3, _has_idempotency_key,
        "The original action and retry share an idempotency key.", ("G5",))
    add("retry_reuses_workbook", "reliability", 5, _retry_reused,
        "A retry reuses the exact persisted workbook and rows.", ("G5",))
    add("no_duplicate_rows", "reliability", 2, _no_duplicate_rows,
        "Persisted person IDs are unique.", ("G5",))
    add("terminal_honest_states", "reliability", 2, _terminal_states,
        "Scenario, research, verification, and writes expose terminal states.", ("G2", "G4", "G5"))

    add("fast_acknowledgement", "speed", 3,
        lambda s: _within_timing(s, "acknowledgement", 1_000),
        "The request is acknowledged within one second.", ("G2",))
    add("bounded_people_research", "speed", 3,
        lambda s: _within_timing(s, "research", 45_000),
        "Recorded people research completes within 45 seconds.", ("G2",))
    add("bounded_verification", "speed", 2,
        lambda s: _within_timing(s, "verification", 40_000),
        "Recorded claim verification completes within 40 seconds.", ("G4",))
    add("bounded_workbook_write", "speed", 2,
        lambda s: _within_timing(s, "workbook_creation", 5_000),
        "Recorded workbook creation completes within five seconds.", ("G5",))

    add("verification_summary", "ux_clarity", 2, _verification_summary_complete,
        "Verification summarizes passed, failed, uncertain, and changed claims.", ("G4",))
    add("receipt_clarity", "ux_clarity", 3, _receipt_complete,
        "The user receives a concrete workbook state, counts, ID, and link.", ("G5",))

    totals = {
        category: sum(check.points for check in checks if check.category == category)
        for category in CATEGORY_WEIGHTS
    }
    if totals != CATEGORY_WEIGHTS:
        raise RuntimeError(f"gauntlet check weights drifted: {totals}")
    return checks


def score_gauntlet(artifact: Mapping[str, Any]) -> dict[str, Any]:
    """Score one structured gauntlet run and return a JSON-safe report."""
    artifact = artifact if isinstance(artifact, Mapping) else {}
    scenarios = [item for item in _list(artifact.get("scenarios")) if isinstance(item, dict)]
    checks = _build_checks(scenarios)
    hard_failures = _hard_failures(artifact, scenarios)

    category_scores: dict[str, dict[str, Any]] = {}
    for category, possible in CATEGORY_WEIGHTS.items():
        category_checks = [check for check in checks if check.category == category]
        earned = sum(check.points for check in category_checks if check.passed)
        category_scores[category] = {
            "earned": earned,
            "possible": possible,
            "ratio": round(earned / possible, 4),
            "checks": [check.to_dict() for check in category_checks],
        }

    score = float(sum(category["earned"] for category in category_scores.values()))
    declared = sorted(
        {
            workflow
            for scenario in scenarios
            for workflow in _list(scenario.get("workflow_ids"))
            if workflow in REQUIRED_WORKFLOWS
        }
    )
    evaluated = [workflow for workflow in declared if workflow in SUPPORTED_WORKFLOWS]
    failed_by_hard_gate = {
        workflow
        for failure in hard_failures
        for workflow in failure.get("workflow_ids", [])
    }
    workflows: dict[str, dict[str, Any]] = {}
    for workflow in REQUIRED_WORKFLOWS:
        relevant = [check for check in checks if workflow in check.workflow_ids]
        failed_checks = [check.check_id for check in relevant if not check.passed]
        if workflow not in evaluated:
            status = "not_evaluated"
        elif failed_checks or workflow in failed_by_hard_gate:
            status = "failed"
        else:
            status = "passed"
        workflows[workflow] = {"status": status, "failed_checks": failed_checks}

    category_floor_met = all(
        category["ratio"] >= CATEGORY_FLOOR for category in category_scores.values()
    )
    evaluated_workflows_pass = bool(evaluated) and all(
        workflows[workflow]["status"] == "passed" for workflow in evaluated
    )
    run_passed = (
        score >= RELEASE_SCORE
        and category_floor_met
        and evaluated_workflows_pass
        and not hard_failures
    )

    all_workflows_pass = all(
        workflows[workflow]["status"] == "passed" for workflow in REQUIRED_WORKFLOWS
    )
    production_streak = _production_streak(artifact)
    high_priority_issues = _high_priority_issues(artifact)
    reason_codes: list[str] = []
    if score < RELEASE_SCORE:
        reason_codes.append("score_below_threshold")
    if not category_floor_met:
        reason_codes.append("category_floor_not_met")
    if not all_workflows_pass:
        reason_codes.append("workflow_coverage_incomplete")
    if production_streak < REQUIRED_PRODUCTION_STREAK:
        reason_codes.append("production_streak_incomplete")
    if hard_failures:
        reason_codes.append("hard_failure")
    if high_priority_issues:
        reason_codes.append("unresolved_high_priority_issues")

    return {
        "schema_version": "1.0",
        "run_id": _text(artifact.get("run_id")),
        "mode": _text(artifact.get("mode")),
        "build_sha": _text(artifact.get("build_sha")),
        "score": score,
        "score_threshold": RELEASE_SCORE,
        "run_passed": run_passed,
        "category_floor": CATEGORY_FLOOR,
        "category_scores": category_scores,
        "declared_workflows": declared,
        "evaluated_workflows": evaluated,
        "workflows": workflows,
        "hard_failures": hard_failures,
        "release": {
            "eligible": not reason_codes,
            "reason_codes": reason_codes,
            "required_workflows": list(REQUIRED_WORKFLOWS),
            "passed_workflows": [
                workflow
                for workflow in REQUIRED_WORKFLOWS
                if workflows[workflow]["status"] == "passed"
            ],
            "consecutive_production_like_passes": production_streak,
            "required_consecutive_production_like_passes": REQUIRED_PRODUCTION_STREAK,
            "unresolved_high_priority_issues": high_priority_issues,
        },
    }


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Score a structured OpenGTM workflow gauntlet artifact."
    )
    parser.add_argument("--input", required=True, help="Recorded or live artifact JSON path")
    parser.add_argument("--output", help="Write the JSON report to this path")
    parser.add_argument(
        "--require-release",
        action="store_true",
        help="Exit nonzero unless the full seven-workflow release gate passes",
    )
    parser.add_argument("--compact", action="store_true", help="Emit compact JSON")
    args = parser.parse_args(argv)

    try:
        report = score_gauntlet(load_artifact(args.input))
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"Unable to score gauntlet artifact: {exc}", file=sys.stderr)
        return 2

    rendered = json.dumps(
        report,
        indent=None if args.compact else 2,
        separators=(",", ":") if args.compact else None,
        sort_keys=False,
    )
    if args.output:
        output = Path(args.output)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(rendered + "\n", encoding="utf-8")
    else:
        print(rendered)

    passed = report["release"]["eligible"] if args.require_release else report["run_passed"]
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
