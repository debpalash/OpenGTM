"""Workspace-scoped dynamic audiences over the lead store."""

from typing import Optional

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field, field_validator
from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import Session

from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role
from apps.api.database import get_db
from apps.api.services.audiences.models import Audience, AudienceMember, AudienceMembershipEvent
from apps.api.services.audiences.refresh import refresh_audience as materialize_audience

router = APIRouter(prefix="/api/audiences", tags=["audiences"])
require_editor = require_workspace_role("editor", "admin")

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
def list_audience_members(audience_id: str, limit: int = 200, offset: int = 0, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(current_workspace)):
    _get(db, ctx.workspace_id, audience_id)
    members = db.query(AudienceMember).filter(
        AudienceMember.workspace_id == ctx.workspace_id,
        AudienceMember.audience_id == audience_id,
    ).order_by(AudienceMember.joined_at.desc()).offset(max(0, offset)).limit(min(max(1, limit), 1000)).all()
    return [member.to_api() for member in members]


@router.get("/{audience_id}/events")
def list_audience_events(audience_id: str, limit: int = 100, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(current_workspace)):
    _get(db, ctx.workspace_id, audience_id)
    events = db.query(AudienceMembershipEvent).filter(
        AudienceMembershipEvent.workspace_id == ctx.workspace_id,
        AudienceMembershipEvent.audience_id == audience_id,
    ).order_by(AudienceMembershipEvent.created_at.desc(), AudienceMembershipEvent.id.desc()).limit(min(max(1, limit), 500)).all()
    return [event.to_api() for event in events]


@router.delete("/{audience_id}", status_code=204)
def delete_audience(audience_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    audience = _get(db, ctx.workspace_id, audience_id)
    from apps.api.services.audiences.scheduler import remove_schedule
    remove_schedule(db, audience.id)
    db.delete(audience)
    db.commit()
