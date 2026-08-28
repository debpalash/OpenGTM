"""Adapt native Chat tool traces and database state into gauntlet artifacts.

The adapter is deliberately conservative. It only marks a claim verified when
the native verification result contains supporting sources. It does not infer a
canonical company domain or contactability from names, titles, or assistant
text. Workbook state is read from the database instead of trusting the receipt.
"""

from __future__ import annotations

from typing import Any, Mapping, Sequence

from apps.api.services.workbook.models import Workbook, WorkbookRow


_ROLE_VERIFIED = {"independent_role_evidence", "profile_reconfirmed"}


def _dict(value: Any) -> dict[str, Any]:
    return value if isinstance(value, dict) else {}


def _list(value: Any) -> list[Any]:
    return value if isinstance(value, list) else []


def _text(value: Any) -> str:
    return str(value).strip() if value is not None else ""


def _confidence(value: Any) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return 0.0
    return round(max(0.0, min(float(value), 1.0)), 4)


def _steps(trace: Mapping[str, Any], tool_name: str) -> list[dict[str, Any]]:
    return [
        step
        for step in _list(trace.get("steps"))
        if isinstance(step, dict) and _text(step.get("tool_name")) == tool_name
    ]


def _normalized_sources(person: Mapping[str, Any]) -> list[dict[str, str]]:
    sources = []
    for source in _list(person.get("verification_sources")):
        if not isinstance(source, dict) or not _text(source.get("url")):
            continue
        sources.append(
            {
                "url": _text(source.get("url")),
                "source": _text(source.get("source_type")) or "public_web",
            }
        )
    return sources


def _contact_evidence(contact: Mapping[str, Any]) -> list[dict[str, Any]]:
    observed_at = _text(contact.get("observed_at"))
    evidence = []
    for attempt in _list(contact.get("attempts")):
        if not isinstance(attempt, dict) or not _text(attempt.get("provider")):
            continue
        evidence.append({
            "kind": "provider_attempt",
            "source": _text(attempt.get("provider")),
            "stage": _text(attempt.get("stage")),
            "status": _text(attempt.get("status")),
            "detail": _text(attempt.get("detail")),
            "source_license": _text(attempt.get("source_license")) or "unknown",
            "observed_at": observed_at,
        })
    return evidence


def _contact_claim(contact: Mapping[str, Any], fallback_observed_at: str) -> dict[str, Any]:
    contact_status = _text(contact.get("status")) or "unavailable"
    claim_status = {
        "verified": "verified",
        "risky": "uncertain",
        "catch_all": "uncertain",
        "invalid": "rejected",
        "unavailable": "unavailable",
    }.get(contact_status, "failed")
    email = _text(contact.get("email")) or None
    confidence = _confidence(contact.get("verification_confidence"))
    if not confidence:
        confidence = _confidence(contact.get("discovery_confidence"))
    return _claim(
        status=claim_status,
        value={"email": email, "contact_status": contact_status},
        confidence=confidence,
        observed_at=_text(contact.get("observed_at")) or fallback_observed_at,
        evidence=_contact_evidence(contact),
        contradictions=(),
    )


def _claim(
    *,
    status: str,
    value: Any,
    confidence: float,
    observed_at: str,
    evidence: Sequence[Mapping[str, Any]],
    contradictions: Sequence[Mapping[str, Any]] = (),
) -> dict[str, Any]:
    return {
        "status": status,
        "value": value,
        "confidence": confidence,
        "observed_at": observed_at,
        "evidence": [dict(item) for item in evidence],
        "contradictions": [dict(item) for item in contradictions],
    }


def _person_claims(
    person: Mapping[str, Any],
    *,
    company: str,
    canonical_domain: str,
    company_resolution: Mapping[str, Any],
    contact: Mapping[str, Any],
) -> dict[str, Any]:
    native_status = _text(person.get("verification_status"))
    evidence = _normalized_sources(person)
    role_verified = native_status in _ROLE_VERIFIED and bool(evidence)
    role_status = "verified" if role_verified else "uncertain"
    confidence = _confidence(
        person.get("verification_confidence", person.get("confidence"))
    )
    observed_at = _text(person.get("checked_at") or person.get("retrieved_at"))
    contradictions = _dict(person.get("claim_contradictions"))
    resolution_evidence = []
    if _text(company_resolution.get("evidence_url")):
        resolution_evidence.append(
            {
                "url": _text(company_resolution.get("evidence_url")),
                "source": _text(company_resolution.get("source"))
                or "company_identity_provider",
            }
        )
    company_verified = bool(canonical_domain and resolution_evidence)
    contact_claim = _contact_claim(contact, observed_at) if contact else None

    return {
        "company_identity": _claim(
            status="verified" if company_verified else "uncertain",
            value={"company": company, "canonical_domain": canonical_domain},
            confidence=_confidence(company_resolution.get("confidence")),
            observed_at=_text(company_resolution.get("observed_at")) or observed_at,
            evidence=resolution_evidence,
            contradictions=_list(contradictions.get("company_identity")),
        ),
        "current_employment": _claim(
            status=role_status,
            value=True if role_verified else None,
            confidence=confidence,
            observed_at=observed_at,
            evidence=evidence,
            contradictions=_list(contradictions.get("current_employment")),
        ),
        "title": _claim(
            status="uncertain",
            value=_text(person.get("title")) or None,
            confidence=confidence,
            observed_at=observed_at,
            evidence=evidence,
            contradictions=_list(contradictions.get("title")),
        ),
        "partnership_function": _claim(
            status=role_status,
            value=True if role_verified else None,
            confidence=confidence,
            observed_at=observed_at,
            evidence=evidence,
            contradictions=_list(contradictions.get("partnership_function")),
        ),
        "contactability": _claim(
            status=contact_claim["status"],
            value=contact_claim["value"],
            confidence=contact_claim["confidence"],
            observed_at=contact_claim["observed_at"],
            evidence=contact_claim["evidence"],
            contradictions=_list(contradictions.get("contactability")),
        ) if contact_claim else _claim(
            status="unavailable",
            value=None,
            confidence=0.0,
            observed_at=observed_at,
            evidence=(),
            contradictions=_list(contradictions.get("contactability")),
        ),
    }


def _verification_people(
    result: Mapping[str, Any],
    *,
    company: str,
    canonical_domain: str,
    company_resolution: Mapping[str, Any],
    contact_people: Mapping[str, Mapping[str, Any]],
) -> list[dict[str, Any]]:
    people = []
    for person in _list(result.get("people")):
        if not isinstance(person, dict) or not _text(person.get("person_id")):
            continue
        people.append(
            {
                "person_id": _text(person.get("person_id")),
                "claims": _person_claims(
                    person,
                    company=company,
                    canonical_domain=canonical_domain,
                    company_resolution=company_resolution,
                    contact=_dict(contact_people.get(_text(person.get("person_id")))),
                ),
            }
        )
    return people


def _verification_summary(people: Sequence[Mapping[str, Any]]) -> dict[str, int]:
    summary = {"passed": 0, "failed": 0, "uncertain": 0, "changed": 0}
    for person in people:
        for claim in _dict(person.get("claims")).values():
            status = _text(_dict(claim).get("status"))
            if status == "verified":
                summary["passed"] += 1
            elif status in {"rejected", "failed", "contradicted"}:
                summary["failed"] += 1
            else:
                summary["uncertain"] += 1
            if _dict(claim).get("supersedes"):
                summary["changed"] += 1
    return summary


def _research_people(
    result: Mapping[str, Any],
    *,
    canonical_domain: str,
) -> list[dict[str, Any]]:
    people = []
    for person in _list(result.get("people")):
        if not isinstance(person, dict) or not _text(person.get("person_id")):
            continue
        people.append(
            {
                "person_id": _text(person.get("person_id")),
                "name": _text(person.get("name") or person.get("full_name")),
                "company": _text(person.get("company") or result.get("company")),
                "canonical_company_domain": canonical_domain,
                "title": _text(person.get("title")),
                "location": _text(person.get("location")) or None,
                "public_profile_url": _text(
                    person.get("linkedin_url") or person.get("evidence_url")
                ),
                "retrieved_at": _text(person.get("retrieved_at")),
                "confidence": _confidence(person.get("confidence")),
            }
        )
    return people


def _rejections(result: Mapping[str, Any]) -> list[dict[str, Any]]:
    rejected = []
    for reason, count in _dict(result.get("candidates_rejected")).items():
        if isinstance(count, int) and count > 0:
            rejected.append({"reason": _text(reason), "count": count})
    return rejected


def _workbook_snapshot(db: Any, receipt: Mapping[str, Any]) -> dict[str, Any]:
    workbook_id = _text(receipt.get("workbook_id"))
    workbook = (
        db.query(Workbook).filter(Workbook.id == workbook_id).one_or_none()
        if workbook_id
        else None
    )
    if workbook is None:
        return {
            "exists": False,
            "workbook_id": workbook_id,
            "workspace_id": "",
            "row_count": 0,
            "person_ids": [],
            "selected_person_ids": [],
            "can_continue_enrichment": False,
        }

    rows = (
        db.query(WorkbookRow)
        .filter(WorkbookRow.workbook_id == workbook.id)
        .order_by(WorkbookRow.position.asc(), WorkbookRow.id.asc())
        .all()
    )
    person_ids = [
        _text(row.source_record_id or _dict(row.data).get("person_id")) for row in rows
    ]
    config = _dict(workbook.source_config)
    column_ids = {
        _text(column.get("id"))
        for column in _list(workbook.columns_config)
        if isinstance(column, dict)
    }
    can_continue = (
        bool(rows)
        and all(person_ids)
        and "email" in column_ids
        and all(_text(row.source_provider) for row in rows)
    )
    return {
        "exists": True,
        "workbook_id": _text(workbook.id),
        "workspace_id": _text(workbook.workspace_id),
        "row_count": len(rows),
        "person_ids": person_ids,
        "selected_person_ids": [
            _text(person_id)
            for person_id in _list(config.get("selected_person_ids"))
        ],
        "total_rows": int(workbook.total_rows or 0),
        "can_continue_enrichment": can_continue,
    }


def _normalized_action(
    step: Mapping[str, Any],
    *,
    snapshot: Mapping[str, Any],
) -> dict[str, Any]:
    args = _dict(step.get("args"))
    receipt = _dict(step.get("result"))
    approval = _dict(step.get("approval"))
    receipt_succeeded = receipt.get("ok") is True
    persisted = (
        snapshot.get("exists") is True
        and snapshot.get("row_count") == snapshot.get("total_rows")
    )
    selected = [_text(person_id) for person_id in _list(args.get("person_ids"))]
    action_id = _text(receipt.get("action_id") or args.get("idempotency_key"))
    return {
        "action_id": action_id,
        "idempotency_key": action_id,
        "workspace_id": _text(snapshot.get("workspace_id")),
        "approved": _text(approval.get("decision")) in {"approve", "approved"},
        "status": "succeeded" if receipt_succeeded else "failed",
        "persisted": persisted,
        "reused": receipt.get("reused") is True,
        "workbook_id": _text(receipt.get("workbook_id")),
        "selected_person_ids": selected,
        "persisted_person_ids": list(snapshot.get("person_ids") or []),
        "row_count": int(snapshot.get("row_count") or 0),
        "skipped_count": int(receipt.get("skipped_count") or 0),
        "url": _text(receipt.get("url")),
    }


def _normalized_contact_action(step: Mapping[str, Any]) -> dict[str, Any]:
    args = _dict(step.get("args"))
    result = _dict(step.get("result"))
    approval = _dict(step.get("approval"))
    people = [
        {
            "person_id": _text(person.get("person_id")),
            "contactability": dict(_dict(person.get("contactability"))),
        }
        for person in _list(result.get("people"))
        if isinstance(person, dict) and _text(person.get("person_id"))
    ]
    return {
        "action_id": _text(result.get("action_id") or args.get("idempotency_key")),
        "idempotency_key": _text(result.get("action_id") or args.get("idempotency_key")),
        "workspace_id": _text(result.get("workspace_id")),
        "approved": _text(approval.get("decision")) in {"approve", "approved"},
        "status": "succeeded" if result.get("ok") is True else "failed",
        "reused": result.get("reused") is True,
        "selected_person_ids": [
            _text(person_id) for person_id in _list(args.get("person_ids"))
        ],
        "returned_person_ids": [person["person_id"] for person in people],
        "provider_order": list(_list(result.get("provider_order"))),
        "people": people,
        "summary": dict(_dict(result.get("summary"))),
    }


def build_people_workflow_artifact(
    trace: Mapping[str, Any],
    db: Any,
) -> dict[str, Any]:
    """Build one G2/G3/G4/G5 gauntlet artifact from native action evidence."""
    research_steps = _steps(trace, "find_people_at_company")
    verification_steps = _steps(trace, "verify_people_at_company")
    contact_steps = _steps(trace, "enrich_people_contacts")
    workbook_steps = _steps(trace, "create_people_workbook")

    research_step = research_steps[-1] if research_steps else {}
    verification_step = verification_steps[-1] if verification_steps else {}
    contact_step = contact_steps[0] if contact_steps else {}
    contact_retry_step = contact_steps[1] if len(contact_steps) > 1 else {}
    create_step = workbook_steps[0] if workbook_steps else {}
    retry_step = workbook_steps[1] if len(workbook_steps) > 1 else {}
    research_result = _dict(research_step.get("result"))
    verification_result = _dict(verification_step.get("result"))
    contact_result = _dict(contact_step.get("result"))

    resolution = _dict(trace.get("company_resolution"))
    if not _text(resolution.get("status")):
        resolution = _dict(
            verification_result.get("company_resolution")
            or research_result.get("company_resolution")
        )
    company = _text(
        resolution.get("company")
        or verification_result.get("company")
        or research_result.get("company")
    )
    canonical_domain = (
        _text(resolution.get("canonical_domain"))
        if _text(resolution.get("status")) == "resolved"
        else ""
    )
    resolution_status = (
        "resolved"
        if _text(resolution.get("status")) == "resolved" and canonical_domain
        else "unresolved"
    )
    function = _text(
        verification_result.get("function") or research_result.get("function")
    )
    selected = [
        _text(person_id)
        for person_id in _list(_dict(trace.get("selection")).get("person_ids"))
    ]
    if not selected:
        selected = [
            _text(person.get("person_id"))
            for person in _list(verification_result.get("people"))
            if isinstance(person, dict) and _text(person.get("person_id"))
        ]

    contact_people = {
        _text(person.get("person_id")): _dict(person.get("contactability"))
        for person in _list(contact_result.get("people"))
        if isinstance(person, dict) and _text(person.get("person_id"))
    }
    verification_people = _verification_people(
        verification_result,
        company=company,
        canonical_domain=canonical_domain,
        company_resolution=resolution,
        contact_people=contact_people,
    )
    contact_action = _normalized_contact_action(contact_step)
    contact_retry_action = _normalized_contact_action(contact_retry_step)
    create_snapshot = _workbook_snapshot(db, _dict(create_step.get("result")))
    retry_snapshot = _workbook_snapshot(db, _dict(retry_step.get("result")))
    create_action = _normalized_action(create_step, snapshot=create_snapshot)
    retry_action = _normalized_action(retry_step, snapshot=retry_snapshot)

    research_ok = research_result.get("ok") is True
    verification_ok = verification_result.get("ok") is True
    contacts_required = bool(contact_steps)
    contact_ok = contact_result.get("ok") is True if contacts_required else True
    contact_retry_ok = (
        _dict(contact_retry_step.get("result")).get("ok") is True
        if contacts_required else True
    )
    create_ok = _dict(create_step.get("result")).get("ok") is True
    retry_ok = _dict(retry_step.get("result")).get("ok") is True
    scenario_status = (
        "completed"
        if research_ok and verification_ok and contact_ok and contact_retry_ok and create_ok and retry_ok
        else "partial"
    )

    scenario = {
        "id": "partnership_people_to_workbook",
        "status": scenario_status,
        "workflow_ids": [
            workflow
            for workflow in ("G2", "G3", "G4", "G5")
            if workflow != "G3" or contacts_required
        ],
        "workspace_id": _text(trace.get("workspace_id")),
        "conversation_id": _text(trace.get("conversation_id")),
        "prompt_steps": list(_list(trace.get("prompts"))),
        "target": {
            "company": company,
            "canonical_domain": canonical_domain,
            "function": function,
            "resolution_status": resolution_status,
        },
        "selection": {"person_ids": selected},
        "research": {
            "status": "completed" if research_ok else "failed",
            "people": _research_people(
                research_result,
                canonical_domain=canonical_domain,
            ),
            "rejected": _rejections(research_result),
            "exhausted_sources": list(_list(research_result.get("exhausted_sources"))),
        },
        "verification": {
            "status": "completed" if verification_ok else "failed",
            "people": verification_people,
            "summary": _verification_summary(verification_people),
        },
        "contact_action": contact_action,
        "contact_retry_action": contact_retry_action,
        "workbook_action": create_action,
        "retry_action": retry_action,
        "can_continue_enrichment": create_snapshot.get("can_continue_enrichment") is True,
        "timings_ms": {
            "acknowledgement": trace.get("acknowledgement_ms"),
            "research": research_step.get("latency_ms"),
            "verification": verification_step.get("latency_ms"),
            "contact_enrichment": contact_step.get("latency_ms"),
            "workbook_creation": create_step.get("latency_ms"),
        },
        "jobs": list(_list(trace.get("jobs"))),
        "external_writes": list(_list(trace.get("external_writes"))),
    }
    return {
        "schema_version": "1.0",
        "fixture_kind": _text(trace.get("fixture_kind")),
        "run_id": _text(trace.get("run_id")),
        "mode": _text(trace.get("mode")),
        "build_sha": _text(trace.get("build_sha")),
        "started_at": _text(trace.get("started_at")),
        "finished_at": _text(trace.get("finished_at")),
        "unresolved_issues": list(_list(trace.get("unresolved_issues"))),
        "production_run_history": list(_list(trace.get("production_run_history"))),
        "scenarios": [scenario],
    }
