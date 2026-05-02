from fastapi import APIRouter, Depends
from fastapi.responses import StreamingResponse
from sqlalchemy.orm import Session
from typing import List, Optional, Dict
from datetime import datetime
from apps.api.database import get_db
from apps.api.models import EmailData, Link, User
from apps.api.core.security import get_current_active_user, get_current_admin_user
from pydantic import BaseModel
from collections import defaultdict, Counter

router = APIRouter(prefix="/api/data", tags=["CRM"])


class MarkUsedRequest(BaseModel):
    ids: List[int] = []
    used: bool = True
    q: Optional[str] = None
    start_date: Optional[str] = None
    end_date: Optional[str] = None
    source_link_id: Optional[int] = None
    is_used_filter: Optional[bool] = None


@router.get("", response_model=dict)
async def get_email_data(
    limit: int = 50,
    offset: int = 0,
    source_link_id: Optional[int] = None,
    q: Optional[str] = None,
    sort_by: Optional[str] = "id",
    order: Optional[str] = "desc",
    start_date: Optional[str] = None,
    end_date: Optional[str] = None,
    is_used: Optional[bool] = None,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_active_user),
):
    query = db.query(EmailData, Link.url).outerjoin(
        Link, EmailData.source_link_id == Link.id
    )

    if source_link_id:
        query = query.filter(EmailData.source_link_id == source_link_id)
    if q:
        search_term = f"%{q}%"
        query = query.filter(
            (EmailData.name.ilike(search_term)) | (EmailData.email.ilike(search_term))
        )
    if start_date:
        query = query.filter(EmailData.created_at >= start_date)
    if end_date:
        query = query.filter(EmailData.created_at <= end_date)
    if is_used is not None:
        query = query.filter(EmailData.is_used == is_used)

    sort_column = getattr(EmailData, sort_by, EmailData.id)
    if order == "asc":
        query = query.order_by(sort_column.asc())
    else:
        query = query.order_by(sort_column.desc())

    total = query.count()
    if limit == 0:
        return {"total": total, "items": []}

    results = query.offset(offset).limit(limit).all()

    items = []
    for row in results:
        email_data, source_url = row
        item = email_data.__dict__.copy()
        if "_sa_instance_state" in item:
            del item["_sa_instance_state"]
        item["source_url"] = source_url
        items.append(item)

    return {"total": total, "items": items}


@router.post("/mark-used")
async def mark_emails_used(
    request: MarkUsedRequest,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_active_user),
):
    if request.ids:
        count = (
            db.query(EmailData)
            .filter(EmailData.id.in_(request.ids))
            .update({EmailData.is_used: request.used}, synchronize_session=False)
        )
        db.commit()
        return {"count": count, "success": True}

    # Bulk Filter
    query = db.query(EmailData)
    if request.q:
        search_term = f"%{request.q}%"
        query = query.filter(
            (EmailData.name.ilike(search_term)) | (EmailData.email.ilike(search_term))
        )
    if request.source_link_id:
        query = query.filter(EmailData.source_link_id == request.source_link_id)
    if request.start_date:
        query = query.filter(EmailData.created_at >= request.start_date)
    if request.end_date:
        query = query.filter(EmailData.created_at <= request.end_date)
    if request.is_used_filter is not None:
        query = query.filter(EmailData.is_used == request.is_used_filter)

    count = query.update({EmailData.is_used: request.used}, synchronize_session=False)
    db.commit()
    return {"count": count, "success": True}


class EmailDataUpdate(BaseModel):
    name: Optional[str] = None
    email: Optional[str] = None
    tags: Optional[str] = None
    notes: Optional[str] = None
    is_used: Optional[bool] = None


@router.patch("/{id}", response_model=dict)
async def update_email_data(
    id: int,
    data: EmailDataUpdate,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_active_user),
):
    record = db.query(EmailData).filter(EmailData.id == id).first()
    if not record:
        return {"status": "not found", "error": f"Record with id {id} not found"}, 404

    update_data = data.dict(exclude_unset=True)
    for key, value in update_data.items():
        setattr(record, key, value)

    db.commit()
    return {"status": "success", "data": record.__dict__}


@router.delete("/{id}")
def delete_data(
    id: int,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_admin_user),
):
    record = db.query(EmailData).filter(EmailData.id == id).first()
    if record:
        db.delete(record)
        db.commit()
        return {"status": "success"}
    return {"status": "not found"}


@router.get("/export")
async def export_email_data(
    q: Optional[str] = None,
    source_link_id: Optional[int] = None,
    sort_by: Optional[str] = "id",  # Added sort/filter params to export
    order: Optional[str] = "desc",
    start_date: Optional[str] = None,
    end_date: Optional[str] = None,
    is_used: Optional[bool] = None,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_active_user),
):
    query = db.query(EmailData)
    # Apply same filters as get_email_data
    if source_link_id:
        query = query.filter(EmailData.source_link_id == source_link_id)
    if q:
        search_term = f"%{q}%"
        query = query.filter(
            (EmailData.name.ilike(search_term)) | (EmailData.email.ilike(search_term))
        )
    if start_date:
        query = query.filter(EmailData.created_at >= start_date)
    if end_date:
        query = query.filter(EmailData.created_at <= end_date)
    if is_used is not None:
        query = query.filter(EmailData.is_used == is_used)

    # Sort (Optional for export but good consistentcy)
    sort_column = getattr(EmailData, sort_by, EmailData.id)
    if order == "asc":
        query = query.order_by(sort_column.asc())
    else:
        query = query.order_by(sort_column.desc())

    async def iter_csv():
        yield "Name,Email,Tags,Notes,Source Task ID,Created At,Used\n"
        BATCH_SIZE = 1000
        offset = 0
        while True:
            params = query.offset(offset).limit(BATCH_SIZE).all()
            if not params:
                break
            for row in params:
                name = (row.name or "").replace('"', '""')
                email = (row.email or "").replace('"', '""')
                tags = (row.tags or "").replace('"', '""')
                notes = (row.notes or "").replace('"', '""').replace("\n", " ")
                source = str(row.source_link_id or "")
                created = str(row.created_at or "")
                used = "Yes" if row.is_used else "No"
                yield f'"{name}","{email}","{tags}","{notes}","{source}","{created}","{used}"\n'
            offset += BATCH_SIZE

    timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    return StreamingResponse(
        iter_csv(),
        media_type="text/csv",
        headers={"Content-Disposition": f"attachment; filename=export_{timestamp}.csv"},
    )


@router.get("/stats/timeline", response_model=List[dict])
def get_timeline_stats(
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_active_user),
):
    """
    Returns daily ingestion counts for the last 30 days.
    """
    # Simple Python aggregation for SQLite compatibility
    # In production with Postgres, use date_trunc/strftime

    # Query all created_at
    dates = db.query(EmailData.created_at).filter(EmailData.created_at != None).all()

    # Bucketize
    daily_counts: Dict[str, int] = defaultdict(int)
    for (d,) in dates:
        if d:
            # d is string isoformat in this legacy DB? Or datetime object?
            # Model says: created_at = Column(String, default=...)
            # So it's a string.
            try:
                # Assume ISO format or similar "YYYY-MM-DD..."
                day = d[:10]
                daily_counts[day] += 1
            except Exception:
                pass

    # Sort by date
    sorted_days = sorted(daily_counts.items())
    return [{"date": day, "count": count} for day, count in sorted_days]


@router.get("/stats/domains", response_model=List[dict])
def get_domain_stats(
    limit: int = 10,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_active_user),
):
    """
    Returns top domains.
    """
    emails = db.query(EmailData.email).filter(EmailData.email != None).all()

    domain_counts: Counter = Counter()

    for (email_addr,) in emails:
        if email_addr and "@" in email_addr:
            try:
                domain = email_addr.split("@")[-1].lower()
                domain_counts[domain] += 1
            except Exception:
                pass

    top_domains = domain_counts.most_common(limit)
    return [{"domain": d, "count": c} for d, c in top_domains]
