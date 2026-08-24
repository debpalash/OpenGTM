from fastapi import APIRouter, Depends, HTTPException, BackgroundTasks
from sqlalchemy.orm import Session
from datetime import datetime, timezone
from typing import List
from apps.api.database import get_db
from apps.api.models import Link, User
from apps.api.schemas.tasks import LinkCreate, LinkResponse
from apps.api.core.security import get_current_active_user, get_current_admin_user
from apps.api.services.queue_service import queue_service
# Need to import these to ensure they are registered?
# Actually queue handlers are registered in main startup, so just dispatching here is fine.

router = APIRouter(
    prefix="/api/queue",
    tags=["Queue"],
    dependencies=[Depends(get_current_admin_user)],
)


@router.get("", response_model=List[LinkResponse])
def get_queue(
    db: Session = Depends(get_db),
):
    links = db.query(Link).all()
    links.sort(key=lambda x: x.created_at or "", reverse=True)
    return links


@router.post("", response_model=dict)
async def add_to_queue(
    link: LinkCreate,
    background_tasks: BackgroundTasks,
    db: Session = Depends(get_db),
):
    from apps.api.core.url_guard import check_url, BlockedUrlError
    try:
        check_url(str(link.url), resolve=True)
    except BlockedUrlError as exc:
        raise HTTPException(status_code=400, detail=f"URL not allowed: {exc}") from exc
    # Check if URL already exists
    existing = db.query(Link).filter(Link.url == link.url).first()
    if existing:
        return {
            "status": "exists",
            "message": "URL already in queue",
            "id": existing.id,
        }

    new_link = Link(
        url=link.url,
        status="Pending",
        created_at=datetime.now(timezone.utc).isoformat(),
    )
    db.add(new_link)
    db.commit()
    db.refresh(new_link)

    queue_service.add_job(db, "download_link", {"link_id": new_link.id})

    return {"status": "queued", "message": "URL added to queue", "id": new_link.id}


@router.delete("/{link_id}")
def delete_queue_item(
    link_id: int,
    db: Session = Depends(get_db),
):
    link = db.query(Link).filter(Link.id == link_id).first()
    if link:
        db.delete(link)
        db.commit()
        return {"status": "success", "message": "Queue item deleted"}
    return {"status": "not_found", "message": "Queue item not found"}


@router.post("/{link_id}/start")
@router.post("/{link_id}/retry")
async def start_queue_item(
    link_id: int,
    db: Session = Depends(get_db),
):
    link = db.query(Link).filter(Link.id == link_id).first()
    if not link:
        raise HTTPException(status_code=404, detail="Queue item not found")

    link.status = "Pending"
    queue_service.add_job(db, "download_link", {"link_id": link_id}, priority=2)
    db.commit()

    return {"status": "queued", "message": "Task started", "id": link_id}


@router.post("/{link_id}/stop")
async def stop_queue_item(
    link_id: int,
    db: Session = Depends(get_db),
):
    link = db.query(Link).filter(Link.id == link_id).first()
    if not link:
        raise HTTPException(status_code=404, detail="Queue item not found")

    link.status = "Stopped"
    db.commit()
    return {"status": "stopped", "message": "Task marked as stopped"}


@router.post("/fix")
async def fix_queue_tasks(
    current_user: User = Depends(get_current_admin_user),
):  # Admin only
    queue_service.recover_jobs()
    return {"status": "success", "message": "Triggered recovery"}
