"""Audience activation destination definitions and durable sync runs."""

from typing import Optional
from urllib.parse import urlparse

from fastapi import APIRouter, Depends, Header, HTTPException
from pydantic import BaseModel, Field, field_validator
from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import Session

from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role, workspace_scope
from apps.api.database import SessionLocal, get_db
from apps.api.services.audiences.models import Audience
from apps.api.services.destinations.models import AudienceDestination, DestinationDelivery, DestinationInboundReceipt, DestinationInboundToken, DestinationRun

router = APIRouter(prefix="/api/audience-destinations", tags=["audience-destinations"])
require_editor = require_workspace_role("editor", "admin", permission="activation.write")
require_admin = require_workspace_role("admin", permission="secrets.manage")
TYPES = {
    "webhook", "hubspot", "salesforce", "warehouse_http",
    "meta_ads", "google_ads", "linkedin_ads", "instantly", "smartlead",
    "google_sheets",
}


@router.get("/types")
def list_destination_types(ctx: WorkspaceCtx = Depends(current_workspace)):
    """Expose fail-closed support maturity backed by live certifications."""
    from apps.api.services.integrations.certification import integration_catalog

    return {"types": integration_catalog()}


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
    if dtype == "warehouse_http":
        allowed = {"url", "header_secret_ref", "header_name", "dataset", "mode"}
        unknown = set(config) - allowed
        if unknown:
            raise ValueError(f"unsupported warehouse config: {', '.join(sorted(unknown))}")
        parsed = urlparse(str(config.get("url") or ""))
        if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password:
            raise ValueError("warehouse HTTP destination requires an HTTPS URL without embedded credentials")
        if not str(config.get("header_secret_ref") or "").strip():
            raise ValueError("warehouse HTTP destination requires header_secret_ref")
        if str(config.get("mode") or "snapshot") not in {"snapshot", "upsert"}:
            raise ValueError("warehouse mode must be snapshot or upsert")
    if dtype in {"meta_ads", "google_ads", "linkedin_ads"}:
        if config.get("consent_attested") is not True or not str(config.get("consent_source") or "").strip():
            raise ValueError("ad destinations require consent_attested=true and a consent_source")
        required = {
            "meta_ads": {"custom_audience_id"},
            "google_ads": {"customer_id", "user_list_id"},
            "linkedin_ads": {"segment_id"},
        }[dtype]
        missing = required - {key for key, value in config.items() if str(value or "").strip()}
        if missing:
            raise ValueError(f"missing ad destination config: {', '.join(sorted(missing))}")
        allowed = required | {"consent_attested", "consent_source", "api_version", "login_customer_id"}
        unknown = set(config) - allowed
        if unknown:
            raise ValueError(f"unsupported ad destination config: {', '.join(sorted(unknown))}")
    if dtype in {"instantly", "smartlead"}:
        allowed = {"campaign_id"}
        if dtype == "instantly":
            allowed.add("skip_if_in_campaign")
        else:
            allowed.add("settings")
        unknown = set(config) - allowed
        if unknown:
            raise ValueError(
                f"unsupported {dtype} config: {', '.join(sorted(unknown))}"
            )
        if not str(config.get("campaign_id") or "").strip():
            raise ValueError(f"{dtype} campaign_id is required")
        if dtype == "smartlead" and config.get("settings") is not None and not isinstance(config["settings"], dict):
            raise ValueError("smartlead settings must be an object")
    if dtype == "google_sheets":
        allowed = {"spreadsheet_id", "range", "columns"}
        unknown = set(config) - allowed
        if unknown:
            raise ValueError(f"unsupported google_sheets config: {', '.join(sorted(unknown))}")
        if not str(config.get("spreadsheet_id") or "").strip():
            raise ValueError("google_sheets spreadsheet_id is required")
        columns = config.get("columns")
        if not isinstance(columns, list) or not 1 <= len(columns) <= 100 or not all(isinstance(item, str) and item.strip() for item in columns):
            raise ValueError("google_sheets columns must contain 1 to 100 field names")
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


class InboundEvent(BaseModel):
    external_event_id: str = Field(min_length=1, max_length=255)
    external_record_id: Optional[str] = Field(default=None, max_length=255)
    lead_id: Optional[int] = Field(default=None, ge=1)
    fields: dict = Field(default_factory=dict)

    @field_validator("fields")
    @classmethod
    def bounded_fields(cls, value: dict) -> dict:
        if len(value) > 50:
            raise ValueError("fields may contain at most 50 entries")
        if any(isinstance(item, (dict, list)) for item in value.values()):
            raise ValueError("field values must be scalar")
        if any(len(str(item)) > 10_000 for item in value.values() if item is not None):
            raise ValueError("field values may not exceed 10000 characters")
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
    if body.destination_type == "warehouse_http":
        from apps.api.services.workspace.secrets import get_secret
        if not get_secret(ctx.workspace_id, str(config["header_secret_ref"]), ""):
            raise HTTPException(status_code=422, detail="warehouse header_secret_ref was not found")
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
        if destination.destination_type == "warehouse_http":
            from apps.api.services.workspace.secrets import get_secret
            if not get_secret(ctx.workspace_id, str(changes["config"]["header_secret_ref"]), ""):
                raise HTTPException(status_code=422, detail="warehouse header_secret_ref was not found")
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


@router.post("/{destination_id}/inbound-token")
def rotate_inbound_token(destination_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_admin)):
    destination = _get(db, ctx.workspace_id, destination_id)
    if destination.destination_type not in {"hubspot", "salesforce"}:
        raise HTTPException(status_code=409, detail="Inbound sync tokens are only available for CRM destinations")
    from datetime import datetime, timezone
    from apps.api.services.destinations.inbound import generate_token
    db.query(DestinationInboundToken).filter(DestinationInboundToken.workspace_id == ctx.workspace_id, DestinationInboundToken.destination_id == destination.id, DestinationInboundToken.revoked_at.is_(None)).update({DestinationInboundToken.revoked_at: datetime.now(timezone.utc)}, synchronize_session=False)
    raw, digest, prefix = generate_token()
    token = DestinationInboundToken(workspace_id=ctx.workspace_id, destination_id=destination.id, token_hash=digest, prefix=prefix, created_by=ctx.user.id)
    db.add(token); db.commit(); db.refresh(token)
    return {"token": raw, "prefix": prefix, "created_at": token.created_at, "warning": "Copy this token now; it will not be shown again."}


@router.delete("/{destination_id}/inbound-token", status_code=204)
def revoke_inbound_token(destination_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_admin)):
    _get(db, ctx.workspace_id, destination_id)
    from datetime import datetime, timezone
    db.query(DestinationInboundToken).filter(DestinationInboundToken.workspace_id == ctx.workspace_id, DestinationInboundToken.destination_id == destination_id, DestinationInboundToken.revoked_at.is_(None)).update({DestinationInboundToken.revoked_at: datetime.now(timezone.utc)}, synchronize_session=False)
    db.commit()


@router.get("/{destination_id}/inbound-receipts")
def list_inbound_receipts(destination_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(current_workspace)):
    _get(db, ctx.workspace_id, destination_id)
    return [row.to_api() for row in db.query(DestinationInboundReceipt).filter(DestinationInboundReceipt.workspace_id == ctx.workspace_id, DestinationInboundReceipt.destination_id == destination_id).order_by(DestinationInboundReceipt.created_at.desc()).limit(100).all()]


@router.post("/inbound/{destination_id}")
def receive_inbound_event(destination_id: str, body: InboundEvent, authorization: Optional[str] = Header(default=None)):
    raw = authorization[7:].strip() if authorization and authorization.lower().startswith("bearer ") else ""
    from apps.api.services.destinations.inbound import DestinationAuthError, reconcile, resolve_token
    try:
        workspace_id = resolve_token(raw, destination_id)
    except DestinationAuthError as exc:
        raise HTTPException(status_code=401, detail=str(exc), headers={"WWW-Authenticate": "Bearer"})
    with workspace_scope(workspace_id):
        with SessionLocal() as db:
            destination = db.query(AudienceDestination).filter(AudienceDestination.id == destination_id, AudienceDestination.workspace_id == workspace_id).first()
            if destination is None or destination.destination_type not in {"hubspot", "salesforce"} or not destination.enabled:
                raise HTTPException(status_code=404, detail="Active CRM destination not found")
            try:
                receipt, replay = reconcile(db, destination, body.model_dump())
            except ValueError as exc:
                raise HTTPException(status_code=422, detail=str(exc))
            return {**receipt, "replay": replay}


@router.delete("/{destination_id}", status_code=204)
def delete_destination(destination_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    destination = _get(db, ctx.workspace_id, destination_id)
    from datetime import datetime, timezone
    db.query(DestinationInboundToken).filter(DestinationInboundToken.workspace_id == ctx.workspace_id, DestinationInboundToken.destination_id == destination_id, DestinationInboundToken.revoked_at.is_(None)).update({DestinationInboundToken.revoked_at: datetime.now(timezone.utc)}, synchronize_session=False)
    db.delete(destination)
    db.commit()
