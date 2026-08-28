"""Exact, idempotent account signal-tracking schedules.

The Chat action binds to persisted workbook account IDs, never to pronouns or
assistant prose. One ``account_group`` watch owns the exact account scope and
all requested collectors, while the existing poller supplies durable cadence,
single-flight jobs, retry backoff, restart bootstrap, and manual polling.
"""

from __future__ import annotations

import hashlib
import re
from datetime import datetime, timezone
from typing import Any, Mapping, Optional
from urllib.parse import urlsplit

from apps.api.services.poller import engine as poller_engine
from apps.api.services.poller.models import WatchSubscription
from apps.api.services.workbook.models import Workbook, WorkbookRow


ACCOUNT_SIGNAL_TYPES = (
    "partnership_hiring",
    "leadership_change",
    "funding",
    "pricing_page_change",
)
ACCOUNT_SIGNAL_CADENCES = ("hourly", "daily", "weekly")

_TRACK_ACCOUNTS_RE = re.compile(
    r"\btrack\s+(?:these|those|the|my)\s+accounts?\b",
    re.IGNORECASE,
)


class SignalTrackingError(ValueError):
    """A fail-closed schedule request that is unsafe or incomplete."""


def _text(value: Any) -> str:
    return str(value).strip() if value is not None else ""


def _canonical_domain(value: Any) -> str:
    raw = _text(value).lower()
    if not raw:
        return ""
    parsed = urlsplit(raw if "://" in raw else f"https://{raw}")
    host = (parsed.hostname or "").lower().rstrip(".")
    return host[4:] if host.startswith("www.") else host


def _scope_key(account_ids: list[str]) -> str:
    digest = hashlib.sha256("|".join(sorted(account_ids)).encode()).hexdigest()
    return f"accounts_{digest[:24]}"


def _contract_hash(
    workbook_id: str,
    account_ids: list[str],
    cadence: str,
    signal_types: list[str],
) -> str:
    raw = "|".join([
        workbook_id,
        *sorted(account_ids),
        cadence,
        *sorted(signal_types),
    ])
    return hashlib.sha256(raw.encode()).hexdigest()


def extract_signal_tracking_request(text: str) -> Optional[dict[str, Any]]:
    """Parse the accepted account-tracking follow-up without an LLM."""
    raw = _text(text)
    lowered = raw.lower()
    if not raw or not _TRACK_ACCOUNTS_RE.search(raw):
        return None

    cadence = next(
        (value for value in ACCOUNT_SIGNAL_CADENCES if re.search(rf"\b{value}\b", lowered)),
        "weekly",
    )
    signals: list[str] = []
    if "partnership" in lowered and any(term in lowered for term in ("hiring", "jobs", "roles")):
        signals.append("partnership_hiring")
    if any(term in lowered for term in ("leadership change", "leadership changes", "executive change", "executive changes")):
        signals.append("leadership_change")
    if any(term in lowered for term in ("funding", "fundraise", "fundraising")):
        signals.append("funding")
    if "pricing" in lowered and any(term in lowered for term in ("change", "changes", "page")):
        signals.append("pricing_page_change")

    if not signals:
        return None
    return {
        "original_query": raw,
        "cadence": cadence,
        "signal_types": [value for value in ACCOUNT_SIGNAL_TYPES if value in signals],
    }


def workbook_account_ids(db: Any, workspace_id: str, workbook_id: str) -> list[str]:
    """Read the exact stable account IDs currently persisted in a workbook."""
    workbook = db.query(Workbook).filter(
        Workbook.id == workbook_id,
        Workbook.workspace_id == workspace_id,
    ).one_or_none()
    if workbook is None:
        return []
    rows = db.query(WorkbookRow).filter(
        WorkbookRow.workbook_id == workbook_id,
        WorkbookRow.workspace_id == workspace_id,
    ).order_by(WorkbookRow.position.asc(), WorkbookRow.id.asc()).all()
    result: list[str] = []
    for row in rows:
        data = row.data if isinstance(row.data, dict) else {}
        account_id = _text(data.get("account_id") or row.canonical_entity_id)
        if account_id and account_id not in result:
            result.append(account_id)
    return result


def _load_accounts(
    db: Any,
    workspace_id: str,
    workbook_id: str,
    account_ids: list[str],
) -> tuple[Workbook, list[dict[str, Any]]]:
    workbook = db.query(Workbook).filter(
        Workbook.id == workbook_id,
        Workbook.workspace_id == workspace_id,
    ).one_or_none()
    if workbook is None:
        raise SignalTrackingError("Workbook not found")
    if not account_ids or any(not _text(value) for value in account_ids):
        raise SignalTrackingError("Select at least one persisted account")
    if len(account_ids) != len(set(account_ids)):
        raise SignalTrackingError("Account selection contains duplicate IDs")

    selected = set(account_ids)
    rows = db.query(WorkbookRow).filter(
        WorkbookRow.workbook_id == workbook_id,
        WorkbookRow.workspace_id == workspace_id,
    ).order_by(WorkbookRow.position.asc(), WorkbookRow.id.asc()).all()
    by_id: dict[str, dict[str, Any]] = {}
    for row in rows:
        data = row.data if isinstance(row.data, dict) else {}
        account_id = _text(data.get("account_id") or row.canonical_entity_id)
        if account_id not in selected:
            continue
        company = _text(data.get("company"))
        domain = _canonical_domain(data.get("canonical_domain") or data.get("website"))
        if not company or not domain:
            raise SignalTrackingError(
                f"Account {account_id or '<missing>'} lacks a company name or canonical domain"
            )
        by_id[account_id] = {
            "account_id": account_id,
            "company": company,
            "canonical_domain": domain,
            "website": f"https://{domain}",
            "lead_id": row.lead_id,
        }
    missing = [value for value in account_ids if value not in by_id]
    if missing:
        raise SignalTrackingError(
            "Account selection drifted; missing persisted IDs: " + ", ".join(missing[:5])
        )
    return workbook, [by_id[value] for value in account_ids]


def _state(watch: WatchSubscription) -> str:
    if not watch.enabled:
        return "paused"
    return "degraded" if watch.last_error else "active"


def schedule_receipt(watch: WatchSubscription, *, readback_confirmed: bool) -> dict[str, Any]:
    """Serialize only saved state; callers must reload before passing True."""
    config = watch.config if isinstance(watch.config, dict) else {}
    cursor = watch.cursor if isinstance(watch.cursor, dict) else {}
    accounts = config.get("accounts") if isinstance(config.get("accounts"), list) else []
    next_run = watch.next_poll_at.isoformat() if watch.next_poll_at else None
    next_retry = next_run if watch.last_error and watch.enabled else None
    return {
        "ok": True,
        "persisted": True,
        "schedule_id": watch.id,
        "workspace_id": watch.workspace_id,
        "workbook_id": _text(config.get("workbook_id")),
        "scope_key": _text(config.get("scope_key")),
        "scope": {
            "account_count": len(accounts),
            "account_ids": [_text(item.get("account_id")) for item in accounts if isinstance(item, dict)],
            "accounts": accounts,
        },
        "cadence": watch.interval,
        "signal_types": list(watch.signal_types or []),
        "next_run_at": next_run,
        "state": _state(watch),
        "attempt_count": int(cursor.get("attempt_count") or 0),
        "last_error_class": watch.last_error,
        "next_retry_at": next_retry,
        "collector_health": dict(cursor.get("collector_health") or {}),
        "manual_retry_action": {
            "method": "POST",
            "url": f"/api/watches/{watch.id}/poll",
            "label": "Retry collectors now",
        },
        "readback_confirmed": readback_confirmed,
        "url": f"/watches?id={watch.id}",
    }


def upsert_account_signal_schedule(
    db: Any,
    *,
    workspace_id: str,
    workbook_id: str,
    account_ids: list[str],
    cadence: str,
    signal_types: list[str],
    idempotency_key: str,
) -> dict[str, Any]:
    """Create or update one account-group watch, then read it back from storage."""
    cadence = _text(cadence).lower()
    if cadence not in ACCOUNT_SIGNAL_CADENCES:
        raise SignalTrackingError(f"Unsupported cadence: {cadence or '<missing>'}")
    normalized_signals = [value for value in ACCOUNT_SIGNAL_TYPES if value in signal_types]
    if (
        not normalized_signals
        or len(normalized_signals) != len(signal_types)
        or len(signal_types) != len(set(signal_types))
    ):
        raise SignalTrackingError("Signal types must be unique supported account signals")
    action_key = _text(idempotency_key)
    if not action_key or len(action_key) > 255:
        raise SignalTrackingError("A valid idempotency key is required")

    workbook, accounts = _load_accounts(
        db, workspace_id, workbook_id, [_text(value) for value in account_ids]
    )
    stable_ids = [item["account_id"] for item in accounts]
    scope_key = _scope_key(stable_ids)
    contract_hash = _contract_hash(workbook_id, stable_ids, cadence, normalized_signals)

    existing = None
    candidates = db.query(WatchSubscription).filter(
        WatchSubscription.workspace_id == workspace_id,
        WatchSubscription.kind == "account_group",
    ).all()
    for candidate in candidates:
        config = candidate.config if isinstance(candidate.config, dict) else {}
        requests = config.get("action_requests") if isinstance(config.get("action_requests"), dict) else {}
        if action_key in requests and requests[action_key] != contract_hash:
            raise SignalTrackingError("Idempotency key already belongs to a different tracking request")
        if config.get("scope_key") == scope_key:
            existing = candidate

    now = datetime.now(timezone.utc)
    reused = existing is not None
    updated = False
    if existing is None:
        watch = WatchSubscription(
            workspace_id=workspace_id,
            kind="account_group",
            target=f"Account set {scope_key.removeprefix('accounts_')[:8]}",
            signal_types=normalized_signals,
            interval=cadence,
            config={},
            schedule_anchor=now,
            enabled=True,
            cursor={"bootstrapped": False, "attempt_count": 0, "collector_health": {}},
            consecutive_failures=0,
        )
        import uuid

        watch.id = str(uuid.uuid4())
        db.add(watch)
        db.flush()
    else:
        watch = existing
        updated = (
            watch.interval != cadence
            or list(watch.signal_types or []) != normalized_signals
            or not watch.enabled
        )
        watch.interval = cadence
        watch.signal_types = normalized_signals
        if not watch.enabled:
            watch.enabled = True
            watch.consecutive_failures = 0
            watch.last_error = None
        if updated:
            watch.schedule_anchor = now

    old_config = watch.config if isinstance(watch.config, dict) else {}
    requests = old_config.get("action_requests") if isinstance(old_config.get("action_requests"), dict) else {}
    requests = {**requests, action_key: contract_hash}
    watch.config = {
        **old_config,
        "scope_key": scope_key,
        "workbook_id": workbook_id,
        "workbook_name": workbook.name,
        "accounts": accounts,
        "requested_signal_types": normalized_signals,
        "action_requests": requests,
        "updated_at": now.isoformat(),
    }
    if updated:
        from apps.api.models import Job

        active_jobs = db.query(Job).filter(
            Job.type == "watch_poll",
            Job.status.in_(("pending", "processing")),
        ).all()
        for job in active_jobs:
            payload = job.payload if isinstance(job.payload, dict) else {}
            if payload.get("watch_id") == watch.id:
                job.status = "cancelled"
                job.completed_at = now
                job.error = "schedule_updated"
    if not reused or updated or watch.next_poll_at is None:
        poller_engine.schedule_bootstrap_for_watch(db, watch)
    db.commit()

    # Read-after-write is part of the product contract. A detached in-memory
    # object is not accepted as evidence that the schedule exists.
    saved = db.query(WatchSubscription).filter(
        WatchSubscription.id == watch.id,
        WatchSubscription.workspace_id == workspace_id,
    ).one_or_none()
    if saved is None:
        raise SignalTrackingError("Schedule write could not be confirmed")
    receipt = schedule_receipt(saved, readback_confirmed=True)
    receipt.update({
        "action_id": action_key,
        "reused": reused,
        "updated": updated,
    })
    return receipt
