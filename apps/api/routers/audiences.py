"""Workspace-scoped dynamic audiences over the lead store."""

from typing import Optional

from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel, Field, field_validator
from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import Session
from sqlalchemy import func

from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role
from apps.api.database import get_db
from apps.api.services.audiences.models import Audience, AudienceMember, AudienceMembershipEvent
from apps.api.services.audiences.refresh import refresh_audience_observed as materialize_audience

router = APIRouter(prefix="/api/audiences", tags=["audiences"])
require_editor = require_workspace_role("editor", "admin", permission="audiences.write")

FILTER_KEYS = {
    "city", "state", "score_tier", "status", "source", "company_size",
    "specialization", "has_email", "has_phone", "has_website",
    "min_score", "max_score", "search", "job_ids",
}


def _validate_filters(value: dict) -> dict:
    unknown = set(value) - FILTER_KEYS
    if unknown:
        raise ValueError(f"unsupported audience filters: {', '.join(sorted(unknown))}")
    for key in ("has_email", "has_phone", "has_website"):
        if key in value and not isinstance(value[key], bool):
            raise ValueError(f"{key} must be a boolean")
    for key in ("min_score", "max_score"):
        if key in value and (not isinstance(value[key], int) or isinstance(value[key], bool)):
            raise ValueError(f"{key} must be an integer")
    if value.get("min_score") is not None and value.get("max_score") is not None:
        if value["min_score"] > value["max_score"]:
            raise ValueError("min_score cannot exceed max_score")
    return value


class AudienceCreate(BaseModel):
    name: str = Field(min_length=1, max_length=200)
    description: str = Field(default="", max_length=2000)
    filters: dict = Field(default_factory=dict)
    refresh_enabled: bool = True
    refresh_interval_minutes: int = Field(default=60, ge=15, le=10080)

    @field_validator("name")
    @classmethod
    def clean_name(cls, value: str) -> str:
        value = value.strip()
        if not value:
            raise ValueError("name cannot be blank")
        return value

    @field_validator("filters")
    @classmethod
    def valid_filters(cls, value: dict) -> dict:
        return _validate_filters(value)


class AudiencePatch(BaseModel):
    name: Optional[str] = Field(default=None, min_length=1, max_length=200)
    description: Optional[str] = Field(default=None, max_length=2000)
    filters: Optional[dict] = None
    refresh_enabled: Optional[bool] = None
    refresh_interval_minutes: Optional[int] = Field(default=None, ge=15, le=10080)

    @field_validator("name")
    @classmethod
    def clean_name(cls, value: Optional[str]) -> Optional[str]:
        if value is None:
            return value
        value = value.strip()
        if not value:
            raise ValueError("name cannot be blank")
        return value

    @field_validator("filters")
    @classmethod
    def valid_filters(cls, value: Optional[dict]) -> Optional[dict]:
        return _validate_filters(value) if value is not None else value


def _get(db: Session, workspace_id: str, audience_id: str) -> Audience:
    audience = db.query(Audience).filter(
        Audience.workspace_id == workspace_id, Audience.id == audience_id,
    ).first()
    if audience is None:
        raise HTTPException(status_code=404, detail="Audience not found")
    return audience


@router.get("")
def list_audiences(db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(current_workspace)):
    return [a.to_api() for a in db.query(Audience).filter(
        Audience.workspace_id == ctx.workspace_id,
    ).order_by(Audience.updated_at.desc()).all()]


@router.post("", status_code=201)
def create_audience(body: AudienceCreate, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    audience = Audience(
        workspace_id=ctx.workspace_id, name=body.name, description=body.description,
        filters=body.filters, refresh_enabled=body.refresh_enabled,
        refresh_interval_minutes=body.refresh_interval_minutes,
    )
    db.add(audience)
    try:
        db.commit()
    except IntegrityError:
        db.rollback()
        raise HTTPException(status_code=409, detail="An audience with this name already exists")
    db.refresh(audience)
    materialize_audience(db, ctx, audience)
    from apps.api.services.audiences.scheduler import schedule_next
    schedule_next(db, audience)
    return audience.to_api()


@router.patch("/{audience_id}")
def update_audience(audience_id: str, body: AudiencePatch, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    audience = _get(db, ctx.workspace_id, audience_id)
    changes = body.model_dump(exclude_unset=True)
    for key, value in changes.items():
        setattr(audience, key, value)
    try:
        db.commit()
    except IntegrityError:
        db.rollback()
        raise HTTPException(status_code=409, detail="An audience with this name already exists")
    db.refresh(audience)
    if "filters" in changes:
        materialize_audience(db, ctx, audience)
    if {"refresh_enabled", "refresh_interval_minutes"} & set(changes):
        from apps.api.services.audiences.scheduler import schedule_next
        schedule_next(db, audience)
    return audience.to_api()


@router.post("/{audience_id}/refresh")
def refresh_audience(audience_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    audience = _get(db, ctx.workspace_id, audience_id)
    return materialize_audience(db, ctx, audience)


@router.get("/{audience_id}/members")
def list_audience_members(
    audience_id: str,
    limit: int = Query(200, ge=1, le=1000),
    before_id: Optional[int] = Query(None, ge=1),
    offset: int = Query(0, ge=0, le=100000),
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    _get(db, ctx.workspace_id, audience_id)
    query = db.query(AudienceMember).filter(
        AudienceMember.workspace_id == ctx.workspace_id,
        AudienceMember.audience_id == audience_id,
    )
    if before_id is not None:
        query = query.filter(AudienceMember.id < before_id)
    members = query.order_by(AudienceMember.id.desc()).offset(offset).limit(limit).all()
    return [member.to_api() for member in members]


def _account_key(snapshot: dict) -> tuple[str, str]:
    from urllib.parse import urlparse
    website = str(snapshot.get("website") or "").strip().lower()
    if website:
        parsed = urlparse(website if "://" in website else f"https://{website}")
        domain = (parsed.hostname or "").removeprefix("www.")
        if domain:
            return domain, str(snapshot.get("company") or domain)
    company = " ".join(str(snapshot.get("company") or "Unknown account").lower().split())
    return f"company:{company}", str(snapshot.get("company") or "Unknown account")


@router.get("/{audience_id}/accounts")
def list_audience_accounts(audience_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(current_workspace)):
    """Roll person-level membership into company coverage and intent summaries."""
    _get(db, ctx.workspace_id, audience_id)
    members = db.query(AudienceMember).filter(AudienceMember.workspace_id == ctx.workspace_id, AudienceMember.audience_id == audience_id).all()
    from apps.api.services.leadgen.orm_models import SignalRow
    signal_rows = db.query(SignalRow.lead_id, func.count(SignalRow.id), func.coalesce(func.sum(SignalRow.weight), 0)).join(AudienceMember, (AudienceMember.lead_id == SignalRow.lead_id) & (AudienceMember.workspace_id == SignalRow.workspace_id)).filter(AudienceMember.workspace_id == ctx.workspace_id, AudienceMember.audience_id == audience_id).group_by(SignalRow.lead_id).all()
    signals = {lead_id: (int(count), int(weight)) for lead_id, count, weight in signal_rows}
    accounts: dict[str, dict] = {}
    for member in members:
        snapshot = dict(member.snapshot or {})
        key, company = _account_key(snapshot)
        account = accounts.setdefault(key, {"key": key, "company": company, "website": snapshot.get("website") or "", "contacts": 0, "with_email": 0, "with_phone": 0, "decision_makers": 0, "score_total": 0.0, "signal_count": 0, "signal_weight": 0, "profiles": []})
        account["contacts"] += 1
        account["with_email"] += bool(snapshot.get("email"))
        account["with_phone"] += bool(snapshot.get("phone"))
        title = str(snapshot.get("contact_title") or "").lower()
        account["decision_makers"] += any(term in title for term in ("chief", "ceo", "cto", "cmo", "vp", "vice president", "head", "director", "owner", "founder"))
        account["score_total"] += float(snapshot.get("score") or 0)
        count, weight = signals.get(member.lead_id, (0, 0))
        account["signal_count"] += count; account["signal_weight"] += weight
        account["profiles"].append({"lead_id": member.lead_id, "name": snapshot.get("contact_person") or f"Lead {member.lead_id}", "title": snapshot.get("contact_title") or "", "email": snapshot.get("email") or ""})
    result = []
    for account in accounts.values():
        contacts = account["contacts"]
        account["avg_score"] = round(account.pop("score_total") / contacts, 1) if contacts else 0
        account["email_coverage_pct"] = round(account["with_email"] / contacts * 100) if contacts else 0
        account["phone_coverage_pct"] = round(account["with_phone"] / contacts * 100) if contacts else 0
        account["profiles"] = account["profiles"][:10]
        result.append(account)
    result.sort(key=lambda row: (-row["signal_weight"], -row["avg_score"], -row["contacts"], row["company"].lower()))
    return {"accounts": result, "summary": {"account_count": len(result), "contact_count": len(members), "accounts_with_signals": sum(row["signal_count"] > 0 for row in result), "accounts_with_decision_makers": sum(row["decision_makers"] > 0 for row in result)}}


@router.get("/{audience_id}/events")
def list_audience_events(
    audience_id: str,
    limit: int = Query(100, ge=1, le=500),
    before_id: Optional[int] = Query(None, ge=1),
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    _get(db, ctx.workspace_id, audience_id)
    query = db.query(AudienceMembershipEvent).filter(
        AudienceMembershipEvent.workspace_id == ctx.workspace_id,
        AudienceMembershipEvent.audience_id == audience_id,
    )
    if before_id is not None:
        query = query.filter(AudienceMembershipEvent.id < before_id)
    events = query.order_by(AudienceMembershipEvent.id.desc()).limit(limit).all()
    return [event.to_api() for event in events]


@router.delete("/{audience_id}", status_code=204)
def delete_audience(audience_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    audience = _get(db, ctx.workspace_id, audience_id)
    from apps.api.services.audiences.scheduler import remove_schedule
    remove_schedule(db, audience.id)
    db.delete(audience)
    db.commit()
