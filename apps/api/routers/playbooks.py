from typing import Optional

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session
from sqlalchemy.exc import IntegrityError

from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role
from apps.api.database import get_db
from apps.api.services.audiences.models import Audience
from apps.api.services.playbooks.models import PlaybookResult, PlaybookRun, ResearchPlaybook

router = APIRouter(prefix="/api/research-playbooks", tags=["research-playbooks"])
require_editor = require_workspace_role("editor", "admin")


class PlaybookCreate(BaseModel):
    name: str = Field(min_length=1, max_length=200)
    description: str = Field(default="", max_length=2000)
    prompt_template: str = Field(min_length=10, max_length=20000)
    output_format: str = "text"
    max_steps: int = Field(default=4, ge=1, le=8)
    cell_budget_usd: float = Field(default=0.10, gt=0, le=5)


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
    if any(key in changes for key in {"prompt_template", "output_format", "max_steps", "cell_budget_usd"}):
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
    run = PlaybookRun(workspace_id=ctx.workspace_id, playbook_id=playbook.id, audience_id=body.audience_id, prompt_version=playbook.version, prompt_snapshot=playbook.prompt_template, max_members=body.max_members, requested_by=str(ctx.user.id))
    db.add(run); db.commit(); db.refresh(run)
    from apps.api.services.queue_service import queue_service
    queue_service.add_job(db, "research_playbook_run", {"workspace_id": ctx.workspace_id, "run_id": run.id}, fire_key=f"playbook:{run.id}")
    return run.to_api()


@router.get("/{playbook_id}/runs")
def list_runs(playbook_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(current_workspace)):
    _playbook(db, ctx.workspace_id, playbook_id)
    return [x.to_api() for x in db.query(PlaybookRun).filter(PlaybookRun.workspace_id == ctx.workspace_id, PlaybookRun.playbook_id == playbook_id).order_by(PlaybookRun.created_at.desc()).limit(100).all()]


@router.get("/runs/{run_id}/results")
def list_results(run_id: str, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(current_workspace)):
    run = db.query(PlaybookRun).filter(PlaybookRun.id == run_id, PlaybookRun.workspace_id == ctx.workspace_id).first()
    if run is None:
        raise HTTPException(404, "Playbook run not found")
    return [x.to_api() for x in db.query(PlaybookResult).filter(PlaybookResult.workspace_id == ctx.workspace_id, PlaybookResult.run_id == run_id).order_by(PlaybookResult.lead_id).all()]
