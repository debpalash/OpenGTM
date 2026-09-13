"""Deployment-wide operational telemetry for platform administrators."""

from fastapi import APIRouter, Depends
from sqlalchemy.orm import Session

from apps.api.core.security import get_current_admin_user
from apps.api.database import get_db
from apps.api.models import User
from apps.api.services.queue_service import queue_service

router = APIRouter(prefix="/admin/operations", tags=["operations"])


@router.get("/queue")
def queue_metrics(
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_admin_user),
):
    _ = current_user
    return queue_service.metrics(db)
