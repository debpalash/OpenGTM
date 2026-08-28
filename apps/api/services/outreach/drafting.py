"""Evidence-grounded, draft-only outreach for exact saved contacts."""

from __future__ import annotations

import hashlib
import re
import uuid
from typing import Any, Iterable, Optional
from urllib.parse import urlsplit

from sqlalchemy.exc import IntegrityError

from apps.api.services.outreach.orm_models import OutreachDraft


_EMAIL_RE = re.compile(r"^[^\s@]+@[^\s@]+\.[^\s@]+$")
_FUNCTION_TERMS = (
    "partnership",
    "alliances",
    "alliance",
    "partner development",
    "partner ecosystem",
    "channel partner",
    "business development",
)
_GENERIC_LOCAL_PARTS = {
    "admin", "contact", "hello", "info", "office", "partners", "partnerships",
    "sales", "support", "team",
}
_SENIORITY = (
    ("chief", 5), ("vice president", 5), ("vp", 5), ("head", 4),
    ("director", 3), ("lead", 2), ("manager", 1),
)


class DraftingError(ValueError):
    def __init__(self, message: str, *, flags: Optional[list[dict[str, Any]]] = None):
        super().__init__(message)
        self.flags = flags or []


def _text(value: Any) -> str:
    return str(value).strip() if value is not None else ""


def _list(value: Any) -> list[Any]:
    return value if isinstance(value, list) else []


def _dict(value: Any) -> dict[str, Any]:
    return value if isinstance(value, dict) else {}


def _is_http_url(value: Any) -> bool:
    parsed = urlsplit(_text(value))
    return parsed.scheme in {"http", "https"} and bool(parsed.netloc)


def _generic_email(email: str) -> bool:
    local = email.rsplit("@", 1)[0].lower()
    return local in _GENERIC_LOCAL_PARTS or any(
        local.startswith(f"{prefix}+") for prefix in _GENERIC_LOCAL_PARTS
    )


def _has_valid_verifier(contact: dict[str, Any]) -> bool:
    return any(
        _text(attempt.get("stage")) == "verification"
        and _text(attempt.get("status")).lower() == "valid"
        for attempt in _list(contact.get("attempts"))
        if isinstance(attempt, dict)
    )


def _evidence_sources(person: dict[str, Any]) -> list[dict[str, Any]]:
    observed_at = _text(
        person.get("checked_at")
        or person.get("retrieved_at")
        or _dict(person.get("contactability")).get("observed_at")
    )
    confidence = person.get("verification_confidence")
    if not isinstance(confidence, (int, float)):
        confidence = person.get("confidence")
    if not isinstance(confidence, (int, float)):
        confidence = 0.0
    candidates: list[tuple[str, str]] = []
    for source in _list(person.get("verification_sources")):
        if isinstance(source, dict):
            candidates.append((_text(source.get("url")), _text(source.get("label") or source.get("title"))))
    candidates.extend([
        (_text(person.get("evidence_url")), "Saved role evidence"),
        (_text(person.get("linkedin_url")), "Saved public profile"),
    ])
    seen: set[str] = set()
    if not observed_at:
        return []
    result = []
    for url, label in candidates:
        if not _is_http_url(url) or url in seen:
            continue
        seen.add(url)
        result.append({
            "source_url": url,
            "label": label or "Saved public evidence",
            "observed_at": observed_at,
            "confidence": round(max(0.0, min(float(confidence), 1.0)), 4),
        })
    return result


def _candidate(person: dict[str, Any], *, allow_risky: bool) -> tuple[Optional[dict[str, Any]], dict[str, Any]]:
    person_id = _text(person.get("person_id"))
    name = _text(person.get("name") or person.get("full_name"))
    title = _text(person.get("title") or person.get("contact_title"))
    function = _text(person.get("function"))
    contact = _dict(person.get("contactability"))
    email = _text(contact.get("email") or person.get("email")).lower()
    status = _text(contact.get("status")).lower()
    role_address = bool(contact.get("is_role"))
    generic = _generic_email(email) if email else False
    flag = {
        "person_id": person_id,
        "person_name": name,
        "email": email or None,
        "contact_status": status or "unavailable",
        "generic_inbox": generic,
        "is_role_address": role_address,
        "reason": "",
    }
    if not person_id or not name:
        flag["reason"] = "missing_person_identity"
        return None, flag
    if not _EMAIL_RE.fullmatch(email):
        flag["reason"] = "missing_valid_email"
        return None, flag
    if generic or role_address:
        flag["reason"] = "generic_or_role_address"
        return None, flag
    if status == "verified" and not _has_valid_verifier(contact):
        flag["reason"] = "verified_without_valid_verifier"
        return None, flag
    if status not in {"verified", "risky"}:
        flag["reason"] = "contact_not_eligible"
        return None, flag
    if status == "risky" and not allow_risky:
        flag["reason"] = "risky_address_requires_approval"
        return None, flag
    if not any(term in f"{title} {function}".lower() for term in _FUNCTION_TERMS):
        flag["reason"] = "partnership_function_not_supported"
        return None, flag
    evidence = _evidence_sources(person)
    if not evidence:
        flag["reason"] = "personalization_evidence_missing"
        return None, flag

    seniority = next((points for term, points in _SENIORITY if term in title.lower()), 0)
    verification_confidence = contact.get("verification_confidence")
    if not isinstance(verification_confidence, (int, float)):
        verification_confidence = 0.0
    score = (
        (100 if status == "verified" else 50)
        + seniority * 5
        + round(float(verification_confidence) * 10)
        + min(len(evidence), 3)
    )
    return {
        "person": person,
        "person_id": person_id,
        "name": name,
        "title": title,
        "function": function or "partnerships",
        "email": email,
        "contact_status": status,
        "generic_inbox": generic,
        "is_role_address": role_address,
        "evidence": evidence,
        "score": score,
    }, flag


def select_best_contact(
    source_result: dict[str, Any],
    *,
    requested_person_id: str = "",
    allow_risky: bool = False,
) -> dict[str, Any]:
    """Select one exact eligible person and return flags for every rejection."""
    people = [item for item in _list(source_result.get("people")) if isinstance(item, dict)]
    requested = _text(requested_person_id)
    if requested:
        people = [item for item in people if _text(item.get("person_id")) == requested]
        if not people:
            raise DraftingError("Selected person is not in the saved contact result")

    eligible: list[dict[str, Any]] = []
    flags: list[dict[str, Any]] = []
    for person in people:
        candidate, flag = _candidate(person, allow_risky=allow_risky)
        flags.append(flag)
        if candidate:
            eligible.append(candidate)
    if not eligible:
        raise DraftingError(
            "No saved contact has an eligible evidence-backed address for drafting",
            flags=flags,
        )
    eligible.sort(key=lambda item: (-item["score"], item["person_id"]))
    return {"selected": eligible[0], "contact_flags": flags}


def _claim_id(person_id: str, claim: str) -> str:
    digest = hashlib.sha256(f"{person_id}|{claim}".encode()).hexdigest()[:16]
    return f"claim_{digest}"


def _contract_hash(source_action_id: str, selected: dict[str, Any], allow_risky: bool) -> str:
    raw = "|".join([
        source_action_id,
        selected["person_id"],
        selected["email"],
        selected["contact_status"],
        str(bool(allow_risky)),
    ])
    return hashlib.sha256(raw.encode()).hexdigest()


def draft_receipt(draft: OutreachDraft, *, readback_confirmed: bool, reused: bool) -> dict[str, Any]:
    evidence = list(draft.sentence_evidence or [])
    return {
        "ok": True,
        "persisted": True,
        "reused": reused,
        "readback_confirmed": readback_confirmed,
        "action_id": draft.action_idempotency_key,
        "draft_id": draft.id,
        "workspace_id": draft.workspace_id,
        "state": draft.state,
        "person_id": draft.person_id,
        "person_name": draft.person_name,
        "company": draft.company,
        "title": draft.title or "",
        "to_email": draft.to_email,
        "contact_status": draft.contact_status,
        "risky_approved": bool(draft.risky_approved),
        "generic_inbox": bool(draft.generic_inbox),
        "is_role_address": bool(draft.is_role_address),
        "subject": draft.subject,
        "body_text": draft.body_text,
        "sentence_evidence": evidence,
        "personalized_sentence_count": sum(
            1 for item in evidence
            if isinstance(item, dict) and item.get("personalized") is True
        ),
        "send_performed": draft.sent_at is not None or draft.state == "sent",
        "sent_at": draft.sent_at.isoformat() if draft.sent_at else None,
        "url": f"/outreach?draft={draft.id}",
    }


def create_grounded_draft(
    db: Any,
    *,
    workspace_id: str,
    conversation_id: str,
    source_result: dict[str, Any],
    source_action_id: str,
    requested_person_id: str,
    allow_risky: bool,
    idempotency_key: str,
) -> dict[str, Any]:
    action_key = _text(idempotency_key)
    if not action_key or len(action_key) > 255:
        raise DraftingError("A valid idempotency key is required")
    selection = select_best_contact(
        source_result,
        requested_person_id=requested_person_id,
        allow_risky=allow_risky,
    )
    selected = selection["selected"]
    company = _text(source_result.get("company") or selected["person"].get("company"))
    if not company:
        raise DraftingError("Saved company identity is missing")
    contract_hash = _contract_hash(source_action_id, selected, allow_risky)
    existing = db.query(OutreachDraft).filter(
        OutreachDraft.workspace_id == workspace_id,
        OutreachDraft.action_idempotency_key == action_key,
    ).one_or_none()
    if existing:
        snapshot = _dict(existing.source_snapshot)
        if snapshot.get("contract_hash") != contract_hash:
            raise DraftingError("Idempotency key already belongs to a different draft request")
        return draft_receipt(existing, readback_confirmed=True, reused=True)

    first_name = selected["name"].split()[0]
    role_phrase = selected["title"] or selected["function"]
    subject = f"Partnership idea for {company}"
    sentences = [
        f"Hi {first_name},",
        f"I came across your work in {role_phrase} at {company} while researching partnership teams.",
        "I would like to compare notes on whether OpenGTM's evidence-backed account research and workflow automation could support your team.",
        "Would you be open to a brief conversation next week?",
        "Best,",
    ]
    evidence = selected["evidence"]
    identity_claim = _claim_id(selected["person_id"], "person_identity")
    company_claim = _claim_id(selected["person_id"], "current_employment")
    function_claim = _claim_id(selected["person_id"], "partnership_function")
    sentence_evidence = [
        {
            "sentence_id": "subject",
            "text": subject,
            "personalized": True,
            "claim_ids": [company_claim],
            "evidence": evidence,
        },
        {
            "sentence_id": "s1",
            "text": sentences[0],
            "personalized": True,
            "claim_ids": [identity_claim],
            "evidence": evidence,
        },
        {
            "sentence_id": "s2",
            "text": sentences[1],
            "personalized": True,
            "claim_ids": [identity_claim, company_claim, function_claim],
            "evidence": evidence,
        },
        *[
            {
                "sentence_id": f"s{index}",
                "text": sentence,
                "personalized": False,
                "claim_ids": [],
                "evidence": [],
            }
            for index, sentence in enumerate(sentences[2:], 3)
        ],
    ]
    person = selected["person"]
    contact = _dict(person.get("contactability"))
    draft = OutreachDraft(
        id=str(uuid.uuid4()),
        workspace_id=workspace_id,
        action_idempotency_key=action_key,
        conversation_id=conversation_id,
        source_action_id=source_action_id,
        person_id=selected["person_id"],
        person_name=selected["name"],
        company=company,
        title=selected["title"],
        to_email=selected["email"],
        contact_status=selected["contact_status"],
        risky_approved=selected["contact_status"] == "risky" and bool(allow_risky),
        generic_inbox=selected["generic_inbox"],
        is_role_address=selected["is_role_address"],
        subject=subject,
        body_text="\n\n".join(sentences),
        sentence_evidence=sentence_evidence,
        source_snapshot={
            "contract_hash": contract_hash,
            "company": company,
            "function": _text(source_result.get("function")),
            "person_id": selected["person_id"],
            "contact_status": selected["contact_status"],
            "contact_observed_at": _text(contact.get("observed_at")),
            "contact_attempts": list(_list(contact.get("attempts"))),
            "public_evidence": evidence,
            "contact_flags": selection["contact_flags"],
        },
        state="draft",
        sent_at=None,
    )
    db.add(draft)
    try:
        db.commit()
    except IntegrityError:
        # A concurrent retry can race past the first lookup. The unique
        # workspace/action key is authoritative, so reload and apply the same
        # contract check as the ordinary replay path.
        db.rollback()
        raced = db.query(OutreachDraft).filter(
            OutreachDraft.workspace_id == workspace_id,
            OutreachDraft.action_idempotency_key == action_key,
        ).one_or_none()
        if raced is None:
            raise DraftingError("Draft write conflicted and could not be recovered")
        if _dict(raced.source_snapshot).get("contract_hash") != contract_hash:
            raise DraftingError("Idempotency key already belongs to a different draft request")
        return draft_receipt(raced, readback_confirmed=True, reused=True)
    saved = db.query(OutreachDraft).filter(
        OutreachDraft.id == draft.id,
        OutreachDraft.workspace_id == workspace_id,
    ).one_or_none()
    if saved is None:
        raise DraftingError("Draft write could not be confirmed")
    return draft_receipt(saved, readback_confirmed=True, reused=False)
