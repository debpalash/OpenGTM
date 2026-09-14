"""Authenticated, idempotent CRM-to-OpenGTM reconciliation."""

import hashlib
import hmac
import json
import os
import secrets
from datetime import datetime, timedelta, timezone

TOKEN_PREFIX = "dsi_"
ALLOWED_FIELDS = {"company", "website", "email", "phone", "contact_person", "contact_title", "city", "state", "address", "specialization", "company_size", "employee_count_exact", "description", "revenue_range", "founded_year", "industry_tags", "technologies", "funding_stage", "linkedin_url", "twitter_url", "facebook_url", "secondary_emails", "secondary_phones", "decision_makers", "status", "notes"}


class DestinationAuthError(Exception):
    pass


def hash_token(raw: str) -> str:
    return hashlib.sha256(raw.encode()).hexdigest()


def generate_token():
    raw = TOKEN_PREFIX + secrets.token_urlsafe(32)
    return raw, hash_token(raw), raw[:12]


def token_ttl_days() -> int:
    try:
        configured = int(os.getenv("OPENGTM_DESTINATION_TOKEN_TTL_DAYS", "90"))
    except ValueError:
        configured = 90
    return max(1, min(configured, 365))


def token_expiry() -> datetime:
    return datetime.now(timezone.utc) + timedelta(days=token_ttl_days())


def _as_utc(value: datetime | None) -> datetime | None:
    if value is None:
        return None
    return value.replace(tzinfo=timezone.utc) if value.tzinfo is None else value.astimezone(timezone.utc)


def resolve_token(raw: str, destination_id: str):
    from apps.api.database import SessionLocal
    from apps.api.services.destinations.models import DestinationInboundToken
    digest = hash_token(raw or "")
    with SessionLocal() as db:
        token = db.query(DestinationInboundToken).filter(DestinationInboundToken.token_hash == digest).first()
        now = datetime.now(timezone.utc)
        expires_at = _as_utc(token.expires_at) if token is not None else None
        if token is None or not hmac.compare_digest(token.token_hash, digest) or token.revoked_at is not None or token.destination_id != destination_id or expires_at is None or expires_at <= now:
            raise DestinationAuthError("invalid or revoked destination token")
        token.last_used_at = now
        workspace_id = token.workspace_id
        db.commit()
        return workspace_id


def reconcile(db, destination, body: dict) -> tuple[dict, bool]:
    from apps.api.services.destinations.models import DestinationDelivery, DestinationInboundReceipt
    from apps.api.services.leadgen.orm_models import LeadRow
    event_id = str(body.get("external_event_id") or "").strip()[:255]
    if not event_id:
        raise ValueError("external_event_id is required")
    prior = db.query(DestinationInboundReceipt).filter(DestinationInboundReceipt.workspace_id == destination.workspace_id, DestinationInboundReceipt.destination_id == destination.id, DestinationInboundReceipt.external_event_id == event_id).first()
    if prior:
        return prior.to_api(), True
    external_id = str(body.get("external_record_id") or "").strip()[:255] or None
    lead_id = body.get("lead_id")
    if lead_id is None and external_id:
        delivery = db.query(DestinationDelivery).filter(DestinationDelivery.workspace_id == destination.workspace_id, DestinationDelivery.destination_id == destination.id, DestinationDelivery.external_id == external_id, DestinationDelivery.status == "success").order_by(DestinationDelivery.delivered_at.desc()).first()
        lead_id = delivery.lead_id if delivery else None
    if lead_id is None and isinstance(body.get("fields"), dict) and body["fields"].get("email"):
        matches = db.query(LeadRow.id).filter(LeadRow.workspace_id == destination.workspace_id, LeadRow.email == str(body["fields"]["email"]).strip()).limit(2).all()
        lead_id = matches[0][0] if len(matches) == 1 else None
    lead = db.query(LeadRow).filter(LeadRow.workspace_id == destination.workspace_id, LeadRow.id == lead_id).first() if lead_id is not None else None
    fields = body.get("fields") if isinstance(body.get("fields"), dict) else {}
    reverse_map = {target: source for source, target in (destination.field_map or {}).items()}
    mapped = {reverse_map.get(str(key), str(key)): value for key, value in fields.items()}
    allowed = {key: value for key, value in mapped.items() if key in ALLOWED_FIELDS}
    if "employee_count_exact" in allowed:
        try:
            allowed["employee_count_exact"] = int(allowed["employee_count_exact"])
        except (TypeError, ValueError):
            allowed.pop("employee_count_exact")
            mapped["employee_count_exact"] = body.get("fields", {}).get("employee_count_exact")
    policy = str((destination.config or {}).get("inbound_conflict_policy") or "fill_missing")
    if policy not in {"fill_missing", "crm_wins"}:
        policy = "fill_missing"
    applied, ignored = [], sorted(set(mapped) - set(allowed))
    status, error = "applied", None
    if lead is None:
        status, error = "unmatched", "No workspace lead matched the callback"
    else:
        for key, value in allowed.items():
            current = getattr(lead, key, None)
            if value is None or value == "" or (policy == "fill_missing" and current not in (None, "", "N/A")):
                ignored.append(key); continue
            if current != value:
                setattr(lead, key, value); applied.append(key)
        if not applied:
            status = "no_change"
    fingerprint = hashlib.sha256(json.dumps(body, sort_keys=True, separators=(",", ":"), default=str).encode()).hexdigest()
    receipt = DestinationInboundReceipt(workspace_id=destination.workspace_id, destination_id=destination.id, provider=destination.destination_type, external_event_id=event_id, external_record_id=external_id, lead_id=lead_id, status=status, conflict_policy=policy, applied_fields=sorted(applied), ignored_fields=sorted(set(ignored)), payload_fingerprint=fingerprint, error=error)
    db.add(receipt)
    try:
        db.commit(); db.refresh(receipt)
    except Exception:
        db.rollback()
        prior = db.query(DestinationInboundReceipt).filter(DestinationInboundReceipt.workspace_id == destination.workspace_id, DestinationInboundReceipt.destination_id == destination.id, DestinationInboundReceipt.external_event_id == event_id).first()
        if prior:
            return prior.to_api(), True
        raise
    return receipt.to_api(), False
