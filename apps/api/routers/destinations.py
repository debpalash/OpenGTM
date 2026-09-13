"""Audience activation destination definitions and durable sync runs."""

from typing import Optional
from urllib.parse import urlparse

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field, field_validator
from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import Session

from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role
from apps.api.database import get_db
from apps.api.services.audiences.models import Audience
from apps.api.services.destinations.models import AudienceDestination, DestinationDelivery, DestinationRun

router = APIRouter(prefix="/api/audience-destinations", tags=["audience-destinations"])
require_editor = require_workspace_role("editor", "admin")
TYPES = {"webhook", "hubspot", "salesforce"}


def _validate_config(dtype: str, config: dict) -> dict:
    def keys(value):
        if isinstance(value, dict):
            for key, nested in value.items():
                yield str(key).lower()
                yield from keys(nested)
        elif isinstance(value, list):
            for nested in value:
                yield from keys(nested)

    forbidden = {"token", "api_key", "secret", "password"} & set(keys(config))
    if forbidden:
        raise ValueError("credentials must use workspace secrets, not destination config")
    if dtype == "webhook":
        allowed = {"url", "method", "header_secret_ref", "header_name"}
        unknown = set(config) - allowed
        if unknown:
            raise ValueError(f"unsupported webhook config: {', '.join(sorted(unknown))}")
        parsed = urlparse(str(config.get("url") or ""))
        if parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username or parsed.password:
            raise ValueError("webhook requires an http(s) URL without embedded credentials")
        if str(config.get("method") or "POST").upper() not in {"POST", "PUT", "PATCH"}:
            raise ValueError("webhook method must be POST, PUT, or PATCH")
    return config


class DestinationCreate(BaseModel):
    audience_id: str
    name: str = Field(min_length=1, max_length=200)
    destination_type: str
    enabled: bool = True
    config: dict = Field(default_factory=dict)
    field_map: dict[str, str] = Field(default_factory=dict)

    @field_validator("name")
    @classmethod
    def clean_name(cls, value: str) -> str:
        value = value.strip()
        if not value:
            raise ValueError("name cannot be blank")
        return value

    @field_validator("destination_type")
    @classmethod
    def valid_type(cls, value: str) -> str:
        value = value.lower()
        if value not in TYPES:
            raise ValueError(f"unsupported destination type '{value}'")
        return value

    def validated_config(self) -> dict:
        return _validate_config(self.destination_type, self.config)


class DestinationPatch(BaseModel):
    name: Optional[str] = Field(default=None, min_length=1, max_length=200)
    enabled: Optional[bool] = None
    config: Optional[dict] = None
    field_map: Optional[dict[str, str]] = None

    @field_validator("name")
    @classmethod
    def clean_name(cls, value: Optional[str]) -> Optional[str]:
        if value is None:
            return value
        value = value.strip()
        if not value:
            raise ValueError("name cannot be blank")
        return value


def _get(db: Session, ws_id: str, destination_id: str) -> AudienceDestination:
    destination = db.query(AudienceDestination).filter(
        AudienceDestination.workspace_id == ws_id, AudienceDestination.id == destination_id,
    ).first()
    if destination is None:
        raise HTTPException(status_code=404, detail="Destination not found")
    return destination


@router.get("")
def list_destinations(audience_id: Optional[str] = None, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(current_workspace)):
    query = db.query(AudienceDestination).filter(AudienceDestination.workspace_id == ctx.workspace_id)
    if audience_id:
        query = query.filter(AudienceDestination.audience_id == audience_id)
    return [destination.to_api() for destination in query.order_by(AudienceDestination.updated_at.desc()).all()]


@router.post("", status_code=201)
def create_destination(body: DestinationCreate, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    if db.query(Audience).filter(Audience.id == body.audience_id, Audience.workspace_id == ctx.workspace_id).first() is None:
        raise HTTPException(status_code=404, detail="Audience not found")
    try:
        config = body.validated_config()
    except ValueError as exc:
        raise HTTPException(status_code=422, detail=str(exc))
    destination = AudienceDestination(
        workspace_id=ctx.workspace_id, audience_id=body.audience_id, name=body.name,
        destination_type=body.destination_type, enabled=body.enabled,
        config=config, field_map=body.field_map,
    )
    db.add(destination)
    try:
        db.commit()
    except IntegrityError:
        db.rollback()
        raise HTTPException(status_code=409, detail="A destination with this name already exists for the audience")
    db.refresh(destination)
    return destination.to_api()


@router.patch("/{destination_id}")
def update_destination(destination_id: str, body: DestinationPatch, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    destination = _get(db, ctx.workspace_id, destination_id)
    changes = body.model_dump(exclude_unset=True)
    if "config" in changes:
        try:
            changes["config"] = _validate_config(destination.destination_type, changes["config"] or {})
        except ValueError as exc:
            raise HTTPException(status_code=422, detail=str(exc))
    for key, value in changes.items():
        setattr(destination, key, value.strip() if key == "name" else value)
    db.commit()
    db.refresh(destination)
    return destination.to_api()


@router.post("/{destination_id}/sync", status_code=202)
def start_sync(destination_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    destination = _get(db, ctx.workspace_id, destination_id)
    if not destination.enabled:
        raise HTTPException(status_code=409, detail="Destination is disabled")
    active = db.query(DestinationRun).filter(
        DestinationRun.workspace_id == ctx.workspace_id,
        DestinationRun.destination_id == destination.id,
        DestinationRun.status.in_(("pending", "running")),
    ).first()
    if active:
        return active.to_api()
    run = DestinationRun(
        workspace_id=ctx.workspace_id, destination_id=destination.id,
        requested_by=str(ctx.user.id),
    )
    db.add(run)
    db.commit()
    db.refresh(run)
    from apps.api.services.queue_service import queue_service
    queue_service.add_job(db, "audience_destination_sync", {
        "workspace_id": ctx.workspace_id, "run_id": run.id,
    }, fire_key=f"destination_sync:{run.id}")
    return run.to_api()


@router.get("/{destination_id}/runs")
def list_runs(destination_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(current_workspace)):
    _get(db, ctx.workspace_id, destination_id)
    return [run.to_api() for run in db.query(DestinationRun).filter(
        DestinationRun.workspace_id == ctx.workspace_id,
        DestinationRun.destination_id == destination_id,
    ).order_by(DestinationRun.created_at.desc()).limit(100).all()]


@router.get("/runs/{run_id}/deliveries")
def list_deliveries(run_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(current_workspace)):
    run = db.query(DestinationRun).filter(DestinationRun.id == run_id, DestinationRun.workspace_id == ctx.workspace_id).first()
    if run is None:
        raise HTTPException(status_code=404, detail="Destination run not found")
    return [delivery.to_api() for delivery in db.query(DestinationDelivery).filter(
        DestinationDelivery.workspace_id == ctx.workspace_id, DestinationDelivery.run_id == run_id,
    ).order_by(DestinationDelivery.id.asc()).all()]


@router.delete("/{destination_id}", status_code=204)
def delete_destination(destination_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    db.delete(_get(db, ctx.workspace_id, destination_id))
    db.commit()
