import base64
import binascii
import json
from datetime import datetime, timezone
from typing import Optional

from fastapi import APIRouter, Depends, HTTPException, Query, Request
from pydantic import BaseModel, Field, model_validator
from sqlalchemy import and_, or_
from sqlalchemy.orm import Session
from sqlalchemy.exc import IntegrityError

from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role
from apps.api.database import get_db
from apps.api.services.audiences.models import Audience
from apps.api.services.playbooks.models import PlaybookResult, PlaybookRun, ResearchPlaybook

router = APIRouter(prefix="/api/research-playbooks", tags=["research-playbooks"])
require_editor = require_workspace_role("editor", "admin", permission="agents.write")


def _encode_run_cursor(run: PlaybookRun) -> str:
    payload = json.dumps([run.created_at.isoformat(), run.id], separators=(",", ":")).encode()
    return base64.urlsafe_b64encode(payload).decode().rstrip("=")


def _decode_run_cursor(cursor: str) -> tuple[datetime, str]:
    try:
        value = json.loads(base64.urlsafe_b64decode(cursor + "=" * (-len(cursor) % 4)))
        if not isinstance(value, list) or len(value) != 2 or not isinstance(value[1], str):
            raise ValueError
        created_at = datetime.fromisoformat(value[0])
        if created_at.tzinfo is None:
            created_at = created_at.replace(tzinfo=timezone.utc)
        return created_at, value[1]
    except (ValueError, TypeError, UnicodeDecodeError, json.JSONDecodeError, binascii.Error) as exc:
        raise HTTPException(422, "invalid playbook run cursor") from exc


def _decode_result_cursor(cursor: str) -> int:
    try:
        value = int(cursor)
        if value < 0:
            raise ValueError
        return value
    except (ValueError, TypeError) as exc:
        raise HTTPException(422, "invalid playbook result cursor") from exc


@router.get("/capabilities")
def playbook_capabilities(ctx: WorkspaceCtx = Depends(current_workspace)):
    """Expose implemented agent features without overstating live maturity."""
    from apps.api.services.integrations.certification import agent_capability_catalog

    return {"capabilities": agent_capability_catalog()}


class PlaybookStep(BaseModel):
    key: str = Field(pattern=r"^[a-z][a-z0-9_]{0,39}$")
    name: str = Field(min_length=1, max_length=100)
    prompt_template: str = Field(min_length=10, max_length=20000)
    output_format: str = "text"


class PlaybookCreate(BaseModel):
    name: str = Field(min_length=1, max_length=200)
    description: str = Field(default="", max_length=2000)
    prompt_template: str = Field(min_length=10, max_length=20000)
    output_format: str = "text"
    max_steps: int = Field(default=4, ge=1, le=8)
    cell_budget_usd: float = Field(default=0.10, gt=0, le=5)
    steps: list[PlaybookStep] = Field(default_factory=list, max_length=8)

    @model_validator(mode="after")
    def validate_steps(self):
        if len({step.key for step in self.steps}) != len(self.steps):
            raise ValueError("playbook step keys must be unique")
        if any(step.output_format not in {"text", "json"} for step in self.steps):
            raise ValueError("step output_format must be text or json")
        return self


class PlaybookPatch(BaseModel):
    name: Optional[str] = Field(default=None, min_length=1, max_length=200)
    description: Optional[str] = Field(default=None, max_length=2000)
    prompt_template: Optional[str] = Field(default=None, min_length=10, max_length=20000)
    output_format: Optional[str] = None
    max_steps: Optional[int] = Field(default=None, ge=1, le=8)
    cell_budget_usd: Optional[float] = Field(default=None, gt=0, le=5)
    enabled: Optional[bool] = None
    schedule_audience_id: Optional[str] = None
    schedule_interval_minutes: Optional[int] = Field(default=None, ge=15, le=10080)
    steps: Optional[list[PlaybookStep]] = Field(default=None, max_length=8)

    @model_validator(mode="after")
    def validate_steps(self):
        if self.steps is not None and len({step.key for step in self.steps}) != len(self.steps):
            raise ValueError("playbook step keys must be unique")
        if self.steps is not None and any(step.output_format not in {"text", "json"} for step in self.steps):
            raise ValueError("step output_format must be text or json")
        return self


class RunCreate(BaseModel):
    audience_id: str
    max_members: int = Field(default=100, ge=1, le=1000)


def _playbook(db, ws, playbook_id):
    row = db.query(ResearchPlaybook).filter(ResearchPlaybook.id == playbook_id, ResearchPlaybook.workspace_id == ws).first()
    if row is None:
        raise HTTPException(404, "Playbook not found")
    return row


@router.get("")
def list_playbooks(db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(current_workspace)):
    return [x.to_api() for x in db.query(ResearchPlaybook).filter(ResearchPlaybook.workspace_id == ctx.workspace_id).order_by(ResearchPlaybook.updated_at.desc()).all()]


@router.post("", status_code=201)
def create_playbook(body: PlaybookCreate, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    if body.output_format not in {"text", "json"}:
        raise HTTPException(422, "output_format must be text or json")
    row = ResearchPlaybook(workspace_id=ctx.workspace_id, **body.model_dump())
    db.add(row)
    try:
        db.commit()
    except IntegrityError:
        db.rollback()
        raise HTTPException(409, "A playbook with this name already exists")
    db.refresh(row)
    return row.to_api()


@router.delete("/{playbook_id}", status_code=204)
def delete_playbook(playbook_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    row = _playbook(db, ctx.workspace_id, playbook_id)
    active = db.query(PlaybookRun).filter(PlaybookRun.workspace_id == ctx.workspace_id, PlaybookRun.playbook_id == row.id, PlaybookRun.status.in_(("pending", "running"))).first()
    if active:
        raise HTTPException(409, "Cannot delete a playbook with an active run")
    from apps.api.services.playbooks.scheduler import remove_schedule
    remove_schedule(db, row.id); db.delete(row); db.commit()


@router.patch("/{playbook_id}")
def patch_playbook(playbook_id: str, body: PlaybookPatch, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    row = _playbook(db, ctx.workspace_id, playbook_id)
    changes = body.model_dump(exclude_unset=True)
    if changes.get("output_format") not in {None, "text", "json"}:
        raise HTTPException(422, "output_format must be text or json")
    schedule_audience_id = changes.get("schedule_audience_id")
    if schedule_audience_id and db.query(Audience).filter(Audience.id == schedule_audience_id, Audience.workspace_id == ctx.workspace_id).first() is None:
        raise HTTPException(404, "Scheduled audience not found")
    for key, value in changes.items():
        setattr(row, key, value)
    if any(key in changes for key in {"prompt_template", "steps", "output_format", "max_steps", "cell_budget_usd"}):
        row.version += 1
    from apps.api.services.playbooks.scheduler import schedule_next
    schedule_next(db, row); db.refresh(row)
    return row.to_api()


@router.post("/{playbook_id}/runs", status_code=202)
def start_run(playbook_id: str, body: RunCreate, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_editor)):
    playbook = _playbook(db, ctx.workspace_id, playbook_id)
    if not playbook.enabled:
        raise HTTPException(409, "Playbook is disabled")
    if db.query(Audience).filter(Audience.id == body.audience_id, Audience.workspace_id == ctx.workspace_id).first() is None:
        raise HTTPException(404, "Audience not found")
    run = PlaybookRun(workspace_id=ctx.workspace_id, playbook_id=playbook.id, audience_id=body.audience_id, prompt_version=playbook.version, prompt_snapshot=playbook.prompt_template, steps_snapshot=playbook.steps or [], max_members=body.max_members, requested_by=str(ctx.user.id))
    db.add(run); db.commit(); db.refresh(run)
    from apps.api.services.queue_service import queue_service
    queue_service.add_job(db, "research_playbook_run", {"workspace_id": ctx.workspace_id, "run_id": run.id}, fire_key=f"playbook:{run.id}")
    return run.to_api()


@router.get("/{playbook_id}/runs")
def list_runs(
    playbook_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
    limit: int = Query(50, ge=1, le=200),
    offset: int = Query(0, ge=0, le=100000),
    cursor: Optional[str] = Query(None, max_length=1024),
):
    _playbook(db, ctx.workspace_id, playbook_id)
    query = db.query(PlaybookRun).filter(
        PlaybookRun.workspace_id == ctx.workspace_id,
        PlaybookRun.playbook_id == playbook_id,
    )
    if cursor:
        created_at, run_id = _decode_run_cursor(cursor)
        query = query.filter(or_(
            PlaybookRun.created_at < created_at,
            and_(PlaybookRun.created_at == created_at, PlaybookRun.id < run_id),
        ))
    query = query.order_by(PlaybookRun.created_at.desc(), PlaybookRun.id.desc()).limit(limit + 1)
    if offset and not cursor:
        query = query.offset(offset)
    rows = query.all()
    has_more = len(rows) > limit
    page = rows[:limit]
    return {
        "runs": [row.to_api() for row in page],
        "limit": limit,
        "offset": offset if not cursor else None,
        "has_more": has_more,
        "next_cursor": _encode_run_cursor(page[-1]) if has_more else None,
    }


@router.get("/runs/{run_id}/results")
def list_results(
    run_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
    limit: int = Query(100, ge=1, le=500),
    offset: int = Query(0, ge=0, le=100000),
    cursor: Optional[str] = Query(None, max_length=32),
):
    run = db.query(PlaybookRun).filter(PlaybookRun.id == run_id, PlaybookRun.workspace_id == ctx.workspace_id).first()
    if run is None:
        raise HTTPException(404, "Playbook run not found")
    query = db.query(PlaybookResult).filter(
        PlaybookResult.workspace_id == ctx.workspace_id,
        PlaybookResult.run_id == run_id,
    )
    if cursor:
        query = query.filter(PlaybookResult.id > _decode_result_cursor(cursor))
    query = query.order_by(PlaybookResult.id.asc()).limit(limit + 1)
    if offset and not cursor:
        query = query.offset(offset)
    rows = query.all()
    has_more = len(rows) > limit
    page = rows[:limit]
    return {
        "results": [row.to_api() for row in page],
        "limit": limit,
        "offset": offset if not cursor else None,
        "has_more": has_more,
        "next_cursor": str(page[-1].id) if has_more else None,
    }


@router.post("/runs/{run_id}/retry", status_code=202)
def retry_run(
    run_id: str,
    request: Request,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Resume only failed profiles in a terminal playbook run."""
    run = db.query(PlaybookRun).filter(
        PlaybookRun.id == run_id,
        PlaybookRun.workspace_id == ctx.workspace_id,
    ).first()
    if run is None:
        raise HTTPException(404, "Playbook run not found")
    if run.status not in {"failed", "completed_with_errors", "cancelled"}:
        raise HTTPException(409, "Only failed, completed-with-errors, or cancelled runs can be retried")
    run.status = "pending"
    run.error = None
    run.finished_at = None
    from apps.api.services.queue_service import queue_service
    queue_service.add_job(
        db, "research_playbook_run",
        {"workspace_id": ctx.workspace_id, "run_id": run.id},
        fire_key=f"playbook:{run.id}",
    )
    request.state.audit_metadata = {
        "action": "research_playbook.run.retry",
        "playbook_id": run.playbook_id,
        "run_id": run.id,
        "previous_failed": run.failed,
    }
    db.refresh(run)
    return run.to_api()


@router.post("/runs/{run_id}/cancel")
def cancel_run(
    run_id: str,
    request: Request,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Cancel a queued/running playbook; its worker stops at the next safe boundary."""
    run = db.query(PlaybookRun).filter(
        PlaybookRun.id == run_id,
        PlaybookRun.workspace_id == ctx.workspace_id,
    ).first()
    if run is None:
        raise HTTPException(404, "Playbook run not found")
    if run.status not in {"pending", "running"}:
        raise HTTPException(409, "Only pending or running playbook runs can be cancelled")
    from datetime import datetime, timezone
    from apps.api.models import Job

    run.status = "cancelled"
    run.error = "Cancelled by user"
    run.finished_at = datetime.now(timezone.utc)
    db.query(Job).filter(
        Job.workspace_id == ctx.workspace_id,
        Job.type == "research_playbook_run",
        Job.fire_key == f"playbook:{run.id}",
        Job.status.in_(("pending", "processing")),
    ).update({Job.status: "cancelled", Job.error: "Cancelled by user"}, synchronize_session=False)
    db.commit()
    db.refresh(run)
    request.state.audit_metadata = {
        "action": "research_playbook.run.cancel",
        "playbook_id": run.playbook_id,
        "run_id": run.id,
    }
    return run.to_api()
