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
SUPPORTED_WORKFLOWS: tuple[str, ...] = (
    "G1", "G2", "G3", "G4", "G5", "G6", "G7",
)
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


def _declares(scenario: Mapping[str, Any], workflow_id: str) -> bool:
    return workflow_id in _list(scenario.get("workflow_ids"))


def _contact_people(scenario: Mapping[str, Any]) -> list[dict[str, Any]]:
    return [
        person
        for person in _list(_dict(scenario.get("contact_action")).get("people"))
        if isinstance(person, dict)
    ]


def _account_rows(scenario: Mapping[str, Any]) -> list[dict[str, Any]]:
    return [
        account for account in _list(scenario.get("accounts"))
        if isinstance(account, dict)
    ]


def _brief_complete(scenario: Mapping[str, Any]) -> bool:
    brief = _dict(scenario.get("brief"))
    evidence_requirements = {
        _text(value) for value in _list(brief.get("evidence_requirements"))
    }
    return (
        brief.get("complete") is True
        and isinstance(brief.get("requested_count"), int)
        and 1 <= brief["requested_count"] <= 500
        and bool(_list(brief.get("company_types")))
        and not _list(brief.get("missing_fields"))
        and {
            "canonical_company_domain",
            "criterion_evidence",
            "evidence_url",
            "retrieved_at",
            "field_confidence",
        }.issubset(evidence_requirements)
        and bool(_text(brief.get("brief_id")))
    )


def _account_rows_match_brief(scenario: Mapping[str, Any]) -> bool:
    if not _declares(scenario, "G1"):
        return True
    brief = _dict(scenario.get("brief"))
    accounts = _account_rows(scenario)
    expected_criteria = (
        len(_list(brief.get("company_types")))
        + len(_list(brief.get("geographies")))
        + len(_list(brief.get("technologies")))
        + len(_list(brief.get("hiring_roles")))
    )
    if not accounts or expected_criteria <= 0:
        return False
    for account in accounts:
        criteria = _dict(account.get("criteria_evidence"))
        evidence_urls = _list(account.get("evidence_urls"))
        if (
            not _text(account.get("account_id"))
            or not _text(account.get("company"))
            or not _domain(account.get("canonical_domain"))
            or len(criteria) != expected_criteria
            or not all(_dict(item).get("matched") is True for item in criteria.values())
            or not evidence_urls
            or not all(_is_http_url(url) for url in evidence_urls)
            or not _text(account.get("retrieved_at"))
            or not _is_confidence(account.get("field_confidence"))
            or len(_list(account.get("fit_reasons"))) != expected_criteria
        ):
            return False
    return True


def _account_action_persisted(scenario: Mapping[str, Any]) -> bool:
    action = _dict(scenario.get("account_action"))
    return (
        _text(action.get("status")) == "succeeded"
        and action.get("persisted") is True
        and bool(_text(action.get("workbook_id")))
    )


def _account_run_exact(scenario: Mapping[str, Any]) -> bool:
    if not _declares(scenario, "G1"):
        return True
    brief = _dict(scenario.get("brief"))
    run = _dict(scenario.get("source_run"))
    action = _dict(scenario.get("account_action"))
    requested = brief.get("requested_count")
    domains = [_domain(account.get("canonical_domain")) for account in _account_rows(scenario)]
    account_ids = [_text(account.get("account_id")) for account in _account_rows(scenario)]
    return (
        _account_action_persisted(scenario)
        and _text(run.get("status")) == "complete"
        and isinstance(requested, int)
        and run.get("requested_count") == requested
        and run.get("delivered_count") == requested
        and run.get("shortfall") == 0
        and action.get("row_count") == requested
        and len(domains) == requested
        and all(domains)
        and len(domains) == len(set(domains))
        and all(account_ids)
        and len(account_ids) == len(set(account_ids))
    )


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
        and (
            (
                _is_http_url(item.get("url"))
                and bool(_text(item.get("source")))
            )
            or (
                _text(item.get("kind")) == "provider_attempt"
                and bool(_text(item.get("source")))
                and bool(_text(item.get("stage")))
                and bool(_text(item.get("status")))
                and bool(_text(item.get("observed_at")))
            )
        )
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
    if _declares(scenario, "G1") and not _declares(scenario, "G2"):
        return _brief_complete(scenario)
    target = _dict(scenario.get("target"))
    return (
        _text(target.get("resolution_status")) == "resolved"
        and bool(_text(target.get("company")))
        and bool(_domain(target.get("canonical_domain")))
    )


def _accepted_people_match_target(scenario: Mapping[str, Any]) -> bool:
    if _declares(scenario, "G1") and not _declares(scenario, "G2"):
        return _account_rows_match_brief(scenario)
    target_domain = _domain(_dict(scenario.get("target")).get("canonical_domain"))
    people = _list(_dict(scenario.get("research")).get("people"))
    return bool(target_domain and people) and all(
        isinstance(person, dict)
        and _domain(person.get("canonical_company_domain")) == target_domain
        for person in people
    )


def _stable_person_ids(scenario: Mapping[str, Any]) -> bool:
    if _declares(scenario, "G1") and not _declares(scenario, "G2"):
        account_ids = [_text(account.get("account_id")) for account in _account_rows(scenario)]
        return bool(account_ids) and all(account_ids) and len(account_ids) == len(set(account_ids))
    research_ids = _person_ids(_dict(scenario.get("research")).get("people"))
    verification_ids = _person_ids(_dict(scenario.get("verification")).get("people"))
    selected_ids = _selection_ids(scenario)
    stable = (
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
    if not stable or not _declares(scenario, "G3"):
        return stable
    contact_ids = _person_ids(_dict(scenario.get("contact_action")).get("people"))
    return (
        bool(contact_ids)
        and len(contact_ids) == len(set(contact_ids))
        and set(contact_ids) == set(selected_ids)
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
    verification_matches = (
        bool(selected)
        and len(verified) == len(set(verified))
        and set(verified) == set(selected)
    )
    if not verification_matches or not _declares(scenario, "G3"):
        return verification_matches
    action = _dict(scenario.get("contact_action"))
    returned = [_text(value) for value in _list(action.get("returned_person_ids"))]
    action_selected = [_text(value) for value in _list(action.get("selected_person_ids"))]
    return (
        _text(action.get("status")) == "succeeded"
        and action_selected == selected
        and returned == selected
        and len(returned) == len(set(returned))
    )


def _contact_attempt_contract(scenario: Mapping[str, Any]) -> bool:
    if not _declares(scenario, "G3"):
        return True
    action = _dict(scenario.get("contact_action"))
    provider_order = [_text(value) for value in _list(action.get("provider_order"))]
    people = _contact_people(scenario)
    if not provider_order or not people:
        return False
    allowed_statuses = {"verified", "risky", "catch_all", "invalid", "unavailable"}
    exact_methods = {"linkedin", "exact_name"}
    for person in people:
        contact = _dict(person.get("contactability"))
        status = _text(contact.get("status"))
        attempts = [item for item in _list(contact.get("attempts")) if isinstance(item, dict)]
        if status not in allowed_statuses or not attempts or not _text(contact.get("observed_at")):
            return False
        discovery = [item for item in attempts if _text(item.get("stage")) == "discovery"]
        discovery_providers = [_text(item.get("provider")) for item in discovery]
        expected_prefix = provider_order[:len(discovery_providers)]
        if discovery_providers != expected_prefix:
            return False
        found = [item for item in discovery if _text(item.get("status")) == "found"]
        verification = [item for item in attempts if _text(item.get("stage")) == "verification"]
        email = _text(contact.get("email"))
        if status == "verified":
            if (
                not email
                or _text(contact.get("verification_status")) != "valid"
                or not found
                or _text(found[-1].get("detail")) not in exact_methods
                or not any(_text(item.get("status")) == "valid" for item in verification)
            ):
                return False
        elif status == "unavailable":
            if email or contact.get("exhausted") is not True or found:
                return False
        elif status == "invalid":
            if email or _text(contact.get("verification_status")) != "invalid":
                return False
        elif not email:
            return False
    return True


def _contact_summary_complete(scenario: Mapping[str, Any]) -> bool:
    if not _declares(scenario, "G3"):
        return True
    summary = _dict(_dict(scenario.get("contact_action")).get("summary"))
    statuses = ("verified", "risky", "catch_all", "invalid", "unavailable")
    if not all(isinstance(summary.get(status), int) and summary[status] >= 0 for status in statuses):
        return False
    return sum(summary[status] for status in statuses) == len(_contact_people(scenario))


def _workbook_persisted(scenario: Mapping[str, Any]) -> bool:
    if _declares(scenario, "G1") and not _declares(scenario, "G5"):
        return _account_action_persisted(scenario)
    action = _dict(scenario.get("workbook_action"))
    return (
        _text(action.get("status")) == "succeeded"
        and action.get("persisted") is True
        and bool(_text(action.get("workbook_id")))
    )


def _exact_workbook_rows(scenario: Mapping[str, Any]) -> bool:
    if _declares(scenario, "G1") and not _declares(scenario, "G5"):
        return _account_run_exact(scenario)
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
    if _declares(scenario, "G1") and not _declares(scenario, "G2"):
        return _account_rows_match_brief(scenario)
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
    if _declares(scenario, "G1") and not _declares(scenario, "G5"):
        action = _dict(scenario.get("account_action"))
        workbook_id = _text(action.get("workbook_id"))
        return (
            _account_action_persisted(scenario)
            and bool(_text(action.get("action_id")))
            and action.get("source_job_id") is not None
            and isinstance(action.get("row_count"), int)
            and _text(action.get("url")) == f"/workbooks/{workbook_id}"
        )
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
    if _declares(scenario, "G1") and not _declares(scenario, "G5"):
        return _dict(scenario.get("account_action")).get("approved") is True
    workbook_approved = _dict(scenario.get("workbook_action")).get("approved") is True
    if not workbook_approved or not _declares(scenario, "G3"):
        return workbook_approved
    return _dict(scenario.get("contact_action")).get("approved") is True


def _has_idempotency_key(scenario: Mapping[str, Any]) -> bool:
    if _declares(scenario, "G1") and not _declares(scenario, "G5"):
        action = _dict(scenario.get("account_action"))
        retry = _dict(scenario.get("account_retry_action"))
        key = _text(action.get("idempotency_key"))
        return bool(key) and _text(retry.get("idempotency_key")) == key
    action = _dict(scenario.get("workbook_action"))
    retry = _dict(scenario.get("retry_action"))
    key = _text(action.get("idempotency_key"))
    workbook_keyed = bool(key) and _text(retry.get("idempotency_key")) == key
    if not workbook_keyed or not _declares(scenario, "G3"):
        return workbook_keyed
    contact = _dict(scenario.get("contact_action"))
    contact_retry = _dict(scenario.get("contact_retry_action"))
    contact_key = _text(contact.get("idempotency_key"))
    return bool(contact_key) and _text(contact_retry.get("idempotency_key")) == contact_key


def _retry_reused(scenario: Mapping[str, Any]) -> bool:
    if _declares(scenario, "G1") and not _declares(scenario, "G5"):
        action = _dict(scenario.get("account_action"))
        retry = _dict(scenario.get("account_retry_action"))
        return (
            _text(action.get("status")) == "succeeded"
            and _text(retry.get("status")) == "succeeded"
            and retry.get("reused") is True
            and bool(_text(action.get("workbook_id")))
            and _text(retry.get("workbook_id")) == _text(action.get("workbook_id"))
            and _text(retry.get("action_id")) == _text(action.get("action_id"))
            and retry.get("row_count") == action.get("row_count")
        )
    action = _dict(scenario.get("workbook_action"))
    retry = _dict(scenario.get("retry_action"))
    workbook_reused = (
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
    if not workbook_reused or not _declares(scenario, "G3"):
        return workbook_reused
    contact = _dict(scenario.get("contact_action"))
    contact_retry = _dict(scenario.get("contact_retry_action"))
    return (
        _text(contact.get("status")) == "succeeded"
        and _text(contact_retry.get("status")) == "succeeded"
        and contact_retry.get("reused") is True
        and bool(_text(contact.get("action_id")))
        and _text(contact_retry.get("action_id")) == _text(contact.get("action_id"))
        and _list(contact_retry.get("selected_person_ids")) == _list(contact.get("selected_person_ids"))
        and _list(contact_retry.get("people")) == _list(contact.get("people"))
    )


def _no_duplicate_rows(scenario: Mapping[str, Any]) -> bool:
    if _declares(scenario, "G1") and not _declares(scenario, "G5"):
        domains = [_domain(account.get("canonical_domain")) for account in _account_rows(scenario)]
        return bool(domains) and all(domains) and len(domains) == len(set(domains))
    action = _dict(scenario.get("workbook_action"))
    persisted = [_text(value) for value in _list(action.get("persisted_person_ids"))]
    return bool(persisted) and len(persisted) == len(set(persisted))


def _terminal_states(scenario: Mapping[str, Any]) -> bool:
    if _declares(scenario, "G1") and not _declares(scenario, "G2"):
        return (
            _text(scenario.get("status")) in _TERMINAL_SCENARIO_STATES
            and _text(_dict(scenario.get("account_action")).get("status")) in _TERMINAL_ACTION_STATES
            and _text(_dict(scenario.get("account_retry_action")).get("status")) in _TERMINAL_ACTION_STATES
            and _text(_dict(scenario.get("source_run")).get("status")) in {"complete", "partial", "failed"}
        )
    terminal = (
        _text(scenario.get("status")) in _TERMINAL_SCENARIO_STATES
        and _text(_dict(scenario.get("research")).get("status")) in _TERMINAL_SCENARIO_STATES
        and _text(_dict(scenario.get("verification")).get("status")) in _TERMINAL_SCENARIO_STATES
        and _text(_dict(scenario.get("workbook_action")).get("status")) in _TERMINAL_ACTION_STATES
        and _text(_dict(scenario.get("retry_action")).get("status")) in _TERMINAL_ACTION_STATES
    )
    if not terminal or not _declares(scenario, "G3"):
        return terminal
    return (
        _text(_dict(scenario.get("contact_action")).get("status")) in _TERMINAL_ACTION_STATES
        and _text(_dict(scenario.get("contact_retry_action")).get("status")) in _TERMINAL_ACTION_STATES
    )


def _within_timing(scenario: Mapping[str, Any], name: str, maximum_ms: int) -> bool:
    value = _dict(scenario.get("timings_ms")).get(name)
    return isinstance(value, (int, float)) and not isinstance(value, bool) and 0 <= value <= maximum_ms


def _verification_summary_complete(scenario: Mapping[str, Any]) -> bool:
    if _declares(scenario, "G1") and not _declares(scenario, "G4"):
        run = _dict(scenario.get("source_run"))
        requested = run.get("requested_count")
        delivered = run.get("delivered_count")
        shortfall = run.get("shortfall")
        status = _text(run.get("status"))
        if (
            status not in {"complete", "partial"}
            or not isinstance(requested, int)
            or not isinstance(delivered, int)
            or not isinstance(shortfall, int)
            or shortfall != max(0, requested - delivered)
            or not isinstance(run.get("rejected_by_reason"), dict)
            or not isinstance(run.get("exhausted_sources"), list)
            or not isinstance(run.get("retry_options"), list)
        ):
            return False
        if status == "complete":
            return delivered == requested and shortfall == 0
        return (
            delivered < requested
            and shortfall > 0
            and bool(run.get("exhausted_sources"))
            and bool(run.get("retry_options"))
        )
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
    claims_summarized = summary["passed"] + summary["failed"] + summary["uncertain"] == claim_count
    return claims_summarized and _contact_summary_complete(scenario)


def _can_continue_enrichment(scenario: Mapping[str, Any]) -> bool:
    if scenario.get("can_continue_enrichment") is not True:
        return False
    if _declares(scenario, "G1") and not _declares(scenario, "G3"):
        return _account_rows_match_brief(scenario)
    return _contact_attempt_contract(scenario)


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


def _g6_contract_failures(scenario: Mapping[str, Any]) -> list[str]:
    if not _declares(scenario, "G6"):
        return []
    failed: list[str] = []
    requested = _dict(scenario.get("requested_tracking"))
    selection = _dict(scenario.get("selection"))
    action = _dict(scenario.get("schedule_action"))
    retry = _dict(scenario.get("schedule_retry_action"))
    saved = _dict(scenario.get("schedule_readback"))
    selected_ids = [_text(value) for value in _list(selection.get("account_ids"))]
    saved_ids = [
        _text(value) for value in _list(_dict(saved.get("scope")).get("account_ids"))
    ]
    required_signals = {
        "partnership_hiring",
        "leadership_change",
        "funding",
        "pricing_page_change",
    }

    if _text(scenario.get("status")) != "completed":
        failed.append("g6_scenario_completed")
    if (
        not selected_ids
        or len(selected_ids) != len(set(selected_ids))
        or selected_ids != saved_ids
        or selected_ids != [_text(value) for value in _list(requested.get("account_ids"))]
        or _dict(saved.get("scope")).get("account_count") != len(selected_ids)
    ):
        failed.append("g6_exact_account_scope")
    if (
        _text(requested.get("cadence")) != "weekly"
        or _text(saved.get("cadence")) != "weekly"
        or set(_list(requested.get("signal_types"))) != required_signals
        or set(_list(saved.get("signal_types"))) != required_signals
    ):
        failed.append("g6_exact_cadence_and_signals")
    if not (
        _text(action.get("status")) == "succeeded"
        and action.get("persisted") is True
        and action.get("readback_confirmed") is True
        and saved.get("exists") is True
        and saved.get("readback_confirmed") is True
        and bool(_text(saved.get("schedule_id")))
    ):
        failed.append("g6_persisted_readback")
    if not (
        action.get("approved") is True
        and retry.get("approved") is True
        and bool(_text(action.get("idempotency_key")))
        and _text(retry.get("idempotency_key")) == _text(action.get("idempotency_key"))
        and retry.get("reused") is True
        and _text(retry.get("schedule_id")) == _text(action.get("schedule_id"))
        and saved.get("schedule_count_for_scope") == 1
    ):
        failed.append("g6_idempotent_schedule")
    manual = _dict(saved.get("manual_retry_action"))
    if not (
        _text(saved.get("state")) in {"active", "degraded", "paused"}
        and bool(_text(saved.get("next_run_at")))
        and _text(saved.get("url")) == f"/watches?id={_text(saved.get('schedule_id'))}"
        and _text(manual.get("method")) == "POST"
        and _text(manual.get("url")) == f"/api/watches/{_text(saved.get('schedule_id'))}/poll"
    ):
        failed.append("g6_actionable_receipt")

    health = _dict(saved.get("collector_health"))
    failed_collectors = [
        _dict(value) for value in health.values()
        if isinstance(value, dict) and _text(value.get("state")) == "failed"
    ]
    if failed_collectors and not (
        all(
            isinstance(item.get("attempt_count"), int)
            and item["attempt_count"] > 0
            and bool(_text(item.get("last_error_class")))
            for item in failed_collectors
        )
        and bool(_text(saved.get("last_error_class")))
        and bool(_text(saved.get("next_retry_at")))
        and bool(_text(manual.get("url")))
    ):
        failed.append("g6_failed_collector_recovery")
    if not (
        _within_timing(scenario, "acknowledgement", 1_000)
        and _within_timing(scenario, "schedule_creation", 5_000)
    ):
        failed.append("g6_bounded_timing")
    return failed


def _g7_contract_failures(scenario: Mapping[str, Any]) -> list[str]:
    if not _declares(scenario, "G7"):
        return []
    failed: list[str] = []
    source = _dict(scenario.get("source_contact"))
    selection = _dict(scenario.get("selection"))
    action = _dict(scenario.get("draft_action"))
    retry = _dict(scenario.get("draft_retry_action"))
    saved = _dict(scenario.get("draft_readback"))
    contact_status = _text(saved.get("contact_status"))

    if _text(scenario.get("status")) != "completed":
        failed.append("g7_scenario_completed")
    if not (
        bool(_text(selection.get("person_id")))
        and _text(selection.get("person_id")) == _text(source.get("person_id"))
        and _text(saved.get("person_id")) == _text(source.get("person_id"))
        and _text(saved.get("to_email")).lower() == _text(source.get("email")).lower()
    ):
        failed.append("g7_exact_saved_contact")
    if not (
        (
            contact_status == "verified"
            and source.get("valid_verifier_attempt") is True
        )
        or (
            contact_status == "risky"
            and saved.get("risky_approved") is True
            and source.get("risky_approved") is True
        )
    ):
        failed.append("g7_eligible_contact")
    if saved.get("generic_inbox") is True or saved.get("is_role_address") is True:
        failed.append("g7_non_generic_contact")
    if not (
        _text(action.get("status")) == "succeeded"
        and action.get("approved") is True
        and action.get("persisted") is True
        and action.get("readback_confirmed") is True
        and saved.get("exists") is True
        and saved.get("readback_confirmed") is True
        and _text(saved.get("state")) == "draft"
    ):
        failed.append("g7_persisted_readback")

    subject = _text(saved.get("subject"))
    body = _text(saved.get("body_text"))
    evidence_rows = [
        item for item in _list(saved.get("sentence_evidence")) if isinstance(item, dict)
    ]
    personalized = [item for item in evidence_rows if item.get("personalized") is True]
    grounded = bool(personalized)
    for item in personalized:
        sentence = _text(item.get("text"))
        evidence = [value for value in _list(item.get("evidence")) if isinstance(value, dict)]
        if (
            not sentence
            or (sentence != subject and sentence not in body)
            or not _list(item.get("claim_ids"))
            or not evidence
            or any(
                not _is_http_url(value.get("source_url"))
                or not _text(value.get("observed_at"))
                or not _is_confidence(value.get("confidence"))
                for value in evidence
            )
        ):
            grounded = False
            break
    if not grounded:
        failed.append("g7_sentence_grounding")
    if not (
        saved.get("send_performed") is False
        and saved.get("sent_at") is None
        and saved.get("send_rows_after_draft") == 0
        and not any(
            item.get("performed") is True
            or _text(item.get("status")) in {"sent", "succeeded", "applied"}
            for item in _list(scenario.get("external_writes"))
            if isinstance(item, dict)
        )
    ):
        failed.append("g7_draft_only")
    if not (
        retry.get("approved") is True
        and retry.get("reused") is True
        and bool(_text(action.get("idempotency_key")))
        and _text(retry.get("idempotency_key")) == _text(action.get("idempotency_key"))
        and _text(retry.get("draft_id")) == _text(action.get("draft_id"))
        and saved.get("draft_count_for_action") == 1
    ):
        failed.append("g7_idempotent_draft")
    if not (
        _text(saved.get("url")) == f"/outreach?draft={_text(saved.get('draft_id'))}"
        and bool(subject)
        and bool(body)
    ):
        failed.append("g7_actionable_receipt")
    if not (
        _within_timing(scenario, "acknowledgement", 1_000)
        and _within_timing(scenario, "draft_creation", 5_000)
    ):
        failed.append("g7_bounded_timing")
    return failed


def _workflow_contract_failures(
    scenarios: Sequence[dict[str, Any]], workflow_id: str,
) -> list[str]:
    failures: list[str] = []
    for scenario in scenarios:
        if workflow_id == "G6":
            for failure in _g6_contract_failures(scenario):
                if failure not in failures:
                    failures.append(failure)
        if workflow_id == "G7":
            for failure in _g7_contract_failures(scenario):
                if failure not in failures:
                    failures.append(failure)
    return failures


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

        if _declares(scenario, "G6"):
            selection_ids = [
                _text(value)
                for value in _list(_dict(scenario.get("selection")).get("account_ids"))
            ]
            action = _dict(scenario.get("schedule_action"))
            retry = _dict(scenario.get("schedule_retry_action"))
            saved = _dict(scenario.get("schedule_readback"))
            saved_ids = [
                _text(value)
                for value in _list(_dict(saved.get("scope")).get("account_ids"))
            ]
            if action.get("persisted") is True and not (
                action.get("readback_confirmed") is True
                and saved.get("exists") is True
                and saved.get("readback_confirmed") is True
            ):
                add(
                    "false_schedule_persistence",
                    scenario,
                    "Tracking success was claimed without a saved readback.",
                    ("G6",),
                )
            if selection_ids != saved_ids:
                add(
                    "tracking_scope_drift",
                    scenario,
                    "The saved tracking scope does not match the exact selected account IDs.",
                    ("G6",),
                )
            if (
                retry.get("reused") is not True
                or _text(retry.get("schedule_id")) != _text(action.get("schedule_id"))
                or saved.get("schedule_count_for_scope") != 1
            ):
                add(
                    "duplicate_tracking_schedule",
                    scenario,
                    "A repeated tracking request did not reuse one schedule for the scope.",
                    ("G6",),
                )
            if _text(saved.get("workspace_id")) != _text(scenario.get("workspace_id")):
                add(
                    "cross_workspace_schedule",
                    scenario,
                    "The tracking schedule readback belongs to a different workspace.",
                    ("G6",),
                )
            failed_health = [
                _dict(value)
                for value in _dict(saved.get("collector_health")).values()
                if isinstance(value, dict) and _text(value.get("state")) == "failed"
            ]
            if failed_health and not (
                bool(_text(saved.get("last_error_class")))
                and bool(_text(saved.get("next_retry_at")))
                and bool(_text(_dict(saved.get("manual_retry_action")).get("url")))
            ):
                add(
                    "unrecoverable_signal_collector",
                    scenario,
                    "A failed signal collector lacks error, retry, or manual recovery metadata.",
                    ("G6",),
                )

        if _declares(scenario, "G7"):
            source = _dict(scenario.get("source_contact"))
            action = _dict(scenario.get("draft_action"))
            retry = _dict(scenario.get("draft_retry_action"))
            saved = _dict(scenario.get("draft_readback"))
            if action.get("persisted") is True and not (
                action.get("readback_confirmed") is True
                and saved.get("exists") is True
                and saved.get("readback_confirmed") is True
            ):
                add(
                    "false_draft_persistence",
                    scenario,
                    "Draft success was claimed without a saved readback.",
                    ("G7",),
                )
            if (
                _text(saved.get("person_id")) != _text(source.get("person_id"))
                or _text(saved.get("to_email")).lower() != _text(source.get("email")).lower()
            ):
                add(
                    "draft_contact_drift",
                    scenario,
                    "The saved draft recipient does not match the selected saved contact.",
                    ("G7",),
                )
            if saved.get("generic_inbox") is True or saved.get("is_role_address") is True:
                add(
                    "generic_outreach_recipient",
                    scenario,
                    "A generic or role inbox was selected for the outreach draft.",
                    ("G7",),
                    entity_id=_text(saved.get("person_id")),
                )
            if (
                _text(saved.get("contact_status")) == "verified"
                and source.get("valid_verifier_attempt") is not True
            ):
                add(
                    "draft_verified_without_verifier",
                    scenario,
                    "The chosen address is labeled verified without a valid verifier attempt.",
                    ("G7",),
                    entity_id=_text(saved.get("person_id")),
                )
            if (
                _text(saved.get("contact_status")) == "risky"
                and not (
                    saved.get("risky_approved") is True
                    and source.get("risky_approved") is True
                )
            ):
                add(
                    "unapproved_risky_draft",
                    scenario,
                    "A risky address was used without separate explicit approval.",
                    ("G7",),
                    entity_id=_text(saved.get("person_id")),
                )
            for sentence in _list(saved.get("sentence_evidence")):
                if not isinstance(sentence, dict) or sentence.get("personalized") is not True:
                    continue
                evidence = [
                    item for item in _list(sentence.get("evidence"))
                    if isinstance(item, dict)
                ]
                if not _list(sentence.get("claim_ids")) or not evidence or any(
                    not _is_http_url(item.get("source_url"))
                    or not _text(item.get("observed_at"))
                    or not _is_confidence(item.get("confidence"))
                    for item in evidence
                ):
                    add(
                        "ungrounded_personalized_sentence",
                        scenario,
                        "A personalized draft sentence lacks saved claim evidence.",
                        ("G7",),
                        entity_id=_text(sentence.get("sentence_id")),
                    )
            performed_send = (
                saved.get("send_performed") is True
                or saved.get("sent_at") is not None
                or int(saved.get("send_rows_after_draft") or 0) > 0
                or any(
                    item.get("performed") is True
                    or _text(item.get("status")) in {"sent", "succeeded", "applied"}
                    for item in _list(scenario.get("external_writes"))
                    if isinstance(item, dict)
                )
            )
            if performed_send:
                add(
                    "draft_request_sent_message",
                    scenario,
                    "A draft-only request caused or coincided with an outreach send.",
                    ("G7",),
                )
            if (
                retry.get("reused") is not True
                or _text(retry.get("draft_id")) != _text(action.get("draft_id"))
                or saved.get("draft_count_for_action") != 1
            ):
                add(
                    "duplicate_outreach_draft",
                    scenario,
                    "A repeated draft request did not reuse one persisted draft.",
                    ("G7",),
                )
            if _text(saved.get("workspace_id")) != _text(scenario.get("workspace_id")):
                add(
                    "cross_workspace_draft",
                    scenario,
                    "The draft readback belongs to a different workspace.",
                    ("G7",),
                )

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

        account_action = _dict(scenario.get("account_action"))
        account_retry = _dict(scenario.get("account_retry_action"))
        if _declares(scenario, "G1"):
            domains = []
            for account in _account_rows(scenario):
                account_id = _text(account.get("account_id"))
                domain = _domain(account.get("canonical_domain"))
                domains.append(domain)
                criteria = _dict(account.get("criteria_evidence"))
                if (
                    not account_id
                    or not domain
                    or not criteria
                    or not all(_dict(item).get("matched") is True for item in criteria.values())
                    or not _list(account.get("evidence_urls"))
                ):
                    add(
                        "unsupported_account_fit",
                        scenario,
                        f"Accepted account {account_id or '<missing>'} lacks complete criterion evidence.",
                        ("G1",),
                        entity_id=account_id,
                    )
            if domains and len(domains) != len(set(domains)):
                add(
                    "duplicate_account_domain",
                    scenario,
                    "Account discovery persisted duplicate canonical domains.",
                    ("G1",),
                )

            account_run = _dict(scenario.get("source_run"))
            requested = _dict(scenario.get("brief")).get("requested_count")
            delivered = account_run.get("delivered_count")
            if _text(account_run.get("status")) == "complete" and (
                not isinstance(requested, int)
                or delivered != requested
                or len(_account_rows(scenario)) != requested
                or account_run.get("shortfall") != 0
            ):
                add(
                    "false_complete_account_run",
                    scenario,
                    "Account sourcing reports complete without the requested evidence-matched row count.",
                    ("G1",),
                )
            if _text(account_action.get("status")) == "succeeded" and account_action.get("persisted") is not True:
                add(
                    "success_without_persistence",
                    scenario,
                    "Account workbook action reports success without persisted state.",
                    ("G1",),
                )
            if _text(account_action.get("status")) == "succeeded" and account_action.get("persisted") is True:
                workbook_id = _text(account_action.get("workbook_id"))
                if _text(account_action.get("url")) != f"/workbooks/{workbook_id}":
                    add(
                        "wrong_result_link",
                        scenario,
                        "Account workbook receipt URL does not open the persisted workbook.",
                        ("G1",),
                    )
            if _text(account_retry.get("status")) == "succeeded" and (
                account_retry.get("reused") is not True
                or _text(account_retry.get("workbook_id")) != _text(account_action.get("workbook_id"))
                or _text(account_retry.get("idempotency_key")) != _text(account_action.get("idempotency_key"))
            ):
                add(
                    "duplicate_retry_write",
                    scenario,
                    "Account retry did not reuse the original workbook and action key.",
                    ("G1",),
                )

        contact_action = _dict(scenario.get("contact_action"))
        contact_retry = _dict(scenario.get("contact_retry_action"))
        if _declares(scenario, "G3"):
            selected = _selection_ids(scenario)
            action_selected = [
                _text(value) for value in _list(contact_action.get("selected_person_ids"))
            ]
            returned = [
                _text(value) for value in _list(contact_action.get("returned_person_ids"))
            ]
            if (
                _text(contact_action.get("status")) == "succeeded"
                and (action_selected != selected or returned != selected)
            ):
                add(
                    "contact_selection_mismatch",
                    scenario,
                    "Contact enrichment did not return the exact selected person IDs.",
                    ("G3",),
                )

            exact_methods = {"linkedin", "exact_name"}
            for person in _contact_people(scenario):
                person_id = _text(person.get("person_id"))
                contact = _dict(person.get("contactability"))
                attempts = [
                    item for item in _list(contact.get("attempts"))
                    if isinstance(item, dict)
                ]
                discovery_found = [
                    item for item in attempts
                    if _text(item.get("stage")) == "discovery"
                    and _text(item.get("status")) == "found"
                ]
                verification_valid = any(
                    _text(item.get("stage")) == "verification"
                    and _text(item.get("status")) == "valid"
                    for item in attempts
                )
                status = _text(contact.get("status"))
                if status == "verified" and (
                    not verification_valid
                    or _text(contact.get("verification_status")) != "valid"
                ):
                    add(
                        "contact_verified_without_verifier",
                        scenario,
                        f"Contact for {person_id} is verified without a valid verifier attempt.",
                        ("G3", "G4"),
                        entity_id=person_id,
                        claim="contactability",
                    )
                if status == "verified" and (
                    not discovery_found
                    or _text(discovery_found[-1].get("detail")) not in exact_methods
                ):
                    add(
                        "non_exact_email_verified",
                        scenario,
                        f"Contact for {person_id} is verified without exact-person discovery evidence.",
                        ("G3", "G4"),
                        entity_id=person_id,
                        claim="contactability",
                    )
                if status == "unavailable" and contact.get("exhausted") is not True:
                    add(
                        "unexplained_contact_unavailable",
                        scenario,
                        f"Unavailable contact for {person_id} lacks provider exhaustion evidence.",
                        ("G3", "G4"),
                        entity_id=person_id,
                        claim="contactability",
                    )

            if _text(contact_retry.get("status")) == "succeeded" and (
                contact_retry.get("reused") is not True
                or _text(contact_retry.get("action_id")) != _text(contact_action.get("action_id"))
                or _list(contact_retry.get("people")) != _list(contact_action.get("people"))
            ):
                add(
                    "duplicate_contact_retry",
                    scenario,
                    "Contact retry did not reuse the original exact-person result.",
                    ("G3",),
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
        for candidate in (
            action, retry, contact_action, contact_retry,
            account_action, account_retry,
        ):
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
        "The scenario reached completed state.", ("G1", "G2", "G3", "G4", "G5"))
    add("accepted_people_found", "outcome_completion", 5,
        lambda s: (
            bool(_account_rows(s))
            if _declares(s, "G1") and not _declares(s, "G2")
            else bool(_list(_dict(s.get("research")).get("people")))
        ),
        "The research produced an accepted entity result set.", ("G1", "G2"))
    add("verification_covers_selection", "outcome_completion", 5,
        _verification_covers_selection,
        "Verification and contact enrichment cover the exact selected people.", ("G3", "G4"))
    add("workbook_persisted", "outcome_completion", 8, _workbook_persisted,
        "The requested workbook exists in persisted state.", ("G1", "G5"))
    add("exact_workbook_rows", "outcome_completion", 7, _exact_workbook_rows,
        "Workbook rows exactly match the requested entities.", ("G1", "G5"))

    add("target_company_resolved", "accuracy_and_evidence", 5, _target_resolved,
        "The target company or account brief is explicitly resolved.", ("G1", "G2"))
    add("accepted_people_match_target", "accuracy_and_evidence", 5,
        _accepted_people_match_target,
        "Every accepted entity matches the target and its evidence criteria.", ("G1", "G2"))
    add("stable_person_ids", "accuracy_and_evidence", 4, _stable_person_ids,
        "Stable entity IDs survive the workflow.", ("G1", "G2", "G3", "G4"))
    add("employment_evidence", "accuracy_and_evidence", 4,
        lambda s: _claim_verified_with_evidence(s, "current_employment"),
        "Current employment is a separately evidenced verified claim.", ("G2", "G4"))
    add("function_evidence", "accuracy_and_evidence", 4,
        lambda s: _claim_verified_with_evidence(s, "partnership_function"),
        "Partnership function is a separately evidenced verified claim.", ("G2", "G4"))
    add("independent_claim_contract", "accuracy_and_evidence", 3, _claim_set_complete,
        "All required claims carry status, evidence, time, confidence, and contradictions.", ("G3", "G4"))

    add("explicit_selection", "actionability", 4,
        lambda s: (
            _brief_complete(s)
            if _declares(s, "G1") and not _declares(s, "G2")
            else bool(_selection_ids(s))
        ),
        "The action targets an explicit brief or stable entity IDs.", ("G1", "G2", "G3", "G5"))
    add("usable_person_rows", "actionability", 3, _usable_person_rows,
        "Entity rows contain the fields required for inspection and action.", ("G1", "G2"))
    add("complete_action_receipt", "actionability", 5, _receipt_complete,
        "The write receipt contains identity, counts, state, and a correct link.", ("G1", "G5"))
    add("can_continue_enrichment", "actionability", 3,
        _can_continue_enrichment,
        "The saved selection can continue without rediscovery.", ("G1", "G3", "G5"))

    add("explicit_write_approval", "reliability", 3, _write_approved,
        "Provider spend and workbook writes record explicit approval.", ("G1", "G3", "G5"))
    add("idempotency_key_recorded", "reliability", 3, _has_idempotency_key,
        "Each original action and retry share an idempotency key.", ("G1", "G3", "G5"))
    add("retry_reuses_workbook", "reliability", 5, _retry_reused,
        "Retries reuse the exact result, workbook, and rows.", ("G1", "G3", "G5"))
    add("no_duplicate_rows", "reliability", 2, _no_duplicate_rows,
        "Persisted entity IDs and domains are unique.", ("G1", "G5"))
    add("terminal_honest_states", "reliability", 2, _terminal_states,
        "Scenario, research, contact enrichment, verification, and writes expose terminal states.", ("G1", "G2", "G3", "G4", "G5"))

    add("fast_acknowledgement", "speed", 3,
        lambda s: _within_timing(s, "acknowledgement", 1_000),
        "The request is acknowledged within one second.", ("G1", "G2"))
    add("bounded_people_research", "speed", 3,
        lambda s: (
            _within_timing(s, "account_sourcing", 1_800_000)
            if _declares(s, "G1") and not _declares(s, "G2")
            else _within_timing(s, "research", 45_000)
        ),
        "Recorded sourcing completes within its safety limit.", ("G1", "G2"))
    add("bounded_verification", "speed", 2,
        lambda s: (
            _within_timing(s, "verification", 40_000)
            and (
                not _declares(s, "G3")
                or _within_timing(s, "contact_enrichment", 90_000)
            )
        ),
        "Recorded claim verification and contact enrichment finish within their safety limits.", ("G3", "G4"))
    add("bounded_workbook_write", "speed", 2,
        lambda s: _within_timing(s, "workbook_creation", 5_000),
        "Recorded workbook creation completes within five seconds.", ("G1", "G5"))

    add("verification_summary", "ux_clarity", 2, _verification_summary_complete,
        "Source, verification, and contact summaries report each normalized outcome.", ("G1", "G3", "G4"))
    add("receipt_clarity", "ux_clarity", 3, _receipt_complete,
        "The user receives a concrete workbook state, counts, ID, and link.", ("G1", "G5"))

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
        failed_checks.extend(
            failure
            for failure in _workflow_contract_failures(scenarios, workflow)
            if failure not in failed_checks
        )
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
