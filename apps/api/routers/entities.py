"""Entity graph API (Pillar 1) — canonical companies, corroboration, merge/split."""

import logging
from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel
from sqlalchemy.orm import Session

from apps.api.database import get_db
from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role
from apps.api.services.entities.models import (
    CompanyEntity, EntityReviewPair, EntityMergeLog,
)
from apps.api.services.entities.graph import get_entity, merge_entities, split_entity

logger = logging.getLogger("entities.api")
router = APIRouter(prefix="/api/entities", tags=["entities"])
require_editor = require_workspace_role("editor", "admin")


@router.get("/company")
async def list_companies(
    min_corroboration: int = Query(1, ge=1),
    limit: int = Query(100, le=1000),
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """List canonical companies in the authenticated workspace."""
    q = db.query(CompanyEntity).filter(
        CompanyEntity.workspace_id == ctx.workspace_id,
        CompanyEntity.corroboration_count >= min_corroboration,
    )
    q = q.order_by(CompanyEntity.corroboration_count.desc()).limit(limit)
    return {"entities": [e.to_api() for e in q.all()]}


@router.get("/company/{entity_id}")
async def get_company(
    entity_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Canonical record + full per-field provenance (all source candidates)."""
    entity = db.query(CompanyEntity).filter(
        CompanyEntity.id == entity_id,
        CompanyEntity.workspace_id == ctx.workspace_id,
    ).first()
    if entity is None:
        raise HTTPException(status_code=404, detail="Entity not found")
    return entity.to_api()


class MergeRequest(BaseModel):
    kept_id: str
    merged_id: str
    reason: str = "manual"


@router.post("/merge")
async def merge(
    body: MergeRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Merge two canonical entities (e.g. confirming an ambiguous pair)."""
    result = merge_entities(
        db, body.kept_id, body.merged_id, body.reason,
        workspace_id=ctx.workspace_id,
    )
    if "error" in result:
        raise HTTPException(status_code=400, detail=result["error"])
    return result


class SplitRequest(BaseModel):
    merge_log_id: int


@router.post("/split")
async def split(
    body: SplitRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Undo a merge from its audit-log id."""
    result = split_entity(db, body.merge_log_id, workspace_id=ctx.workspace_id)
    if "error" in result:
        raise HTTPException(status_code=400, detail=result["error"])
    return result


@router.get("/review-queue")
async def review_queue(
    status: str = Query("pending"),
    limit: int = Query(100, le=1000),
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Grey-band (0.70–0.85) ambiguous matches awaiting human merge/reject."""
    q = (
        db.query(EntityReviewPair)
        .join(CompanyEntity, CompanyEntity.id == EntityReviewPair.entity_id)
        .filter(CompanyEntity.workspace_id == ctx.workspace_id)
        .filter(EntityReviewPair.status == status)
        .order_by(EntityReviewPair.score.desc())
        .limit(limit)
    )
    return {
        "pairs": [
            {
                "id": p.id, "entity_id": p.entity_id, "candidate": p.candidate,
                "score": p.score, "reason": p.reason, "status": p.status,
            }
            for p in q.all()
        ]
    }


class ReviewDecision(BaseModel):
    pair_id: int
    decision: str  # "merge" | "reject"


@router.post("/review-queue/decide")
async def decide_review(
    body: ReviewDecision,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Resolve a review pair: merge the candidate into the entity, or reject."""
    pair = (
        db.query(EntityReviewPair)
        .join(CompanyEntity, CompanyEntity.id == EntityReviewPair.entity_id)
        .filter(
            EntityReviewPair.id == body.pair_id,
            CompanyEntity.workspace_id == ctx.workspace_id,
        )
        .first()
    )
    if not pair:
        raise HTTPException(status_code=404, detail="Review pair not found")
    if body.decision == "merge":
        new_id = (pair.candidate or {}).get("new_entity_id")
        if not new_id:
            raise HTTPException(status_code=400, detail="No candidate entity to merge")
        result = merge_entities(
            db, pair.entity_id, new_id, reason="review_confirmed",
            workspace_id=ctx.workspace_id,
        )
        if "error" in result:
            raise HTTPException(status_code=400, detail=result["error"])
        pair.status = "merged"
        db.commit()
        return {"status": "merged", **result}
    pair.status = "rejected"
    db.commit()
    return {"status": "rejected"}
