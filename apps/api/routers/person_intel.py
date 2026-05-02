"""
Person Intelligence Router
Provides REST + WebSocket endpoints for LinkedIn profile enrichment.
"""
from fastapi import APIRouter, Depends, HTTPException, WebSocket, Query
from sqlalchemy.orm import Session
from datetime import datetime
from typing import List
from pydantic import BaseModel

from apps.api.database import get_db
from apps.api.models import User, PersonIntel
from apps.api.core.security import get_current_active_user
from apps.api.services.person_intel import person_intel_service

router = APIRouter(tags=["Person Intel"])


# --- Schemas ---

class EnrichRequest(BaseModel):
    linkedin_url: str
    deep: bool = False


class PersonIntelResponse(BaseModel):
    id: int
    linkedin_url: str
    username: str
    name: str = ""
    headline: str = ""
    location: str = ""
    summary: str = ""
    emails: list = []
    social_links: dict = {}
    articles: list = []
    mentions: list = []
    companies: list = []
    education: list = []
    skills: list = []
    status: str = "pending"
    created_at: str = ""

    class Config:
        from_attributes = True


# --- WebSocket Endpoint ---

@router.websocket("/ws/person/enrich")
async def websocket_person_enrich(websocket: WebSocket, token: str = Query(None)):
    """
    WebSocket endpoint for real-time person enrichment.
    
    Client sends: {"linkedin_url": "...", "deep": false}
    Server streams: {"step": "...", ...} progress messages
    Final message: {"step": "completed", "profile": {...}}
    """
    await websocket.accept()
    try:
        data = await websocket.receive_json()
        linkedin_url = data.get("linkedin_url")
        deep = data.get("deep", False)

        if not linkedin_url:
            await websocket.send_json({"step": "error", "message": "No LinkedIn URL provided"})
            return

        if "linkedin.com/in/" not in linkedin_url:
            await websocket.send_json({
                "step": "error",
                "message": "Invalid LinkedIn URL. Expected format: https://www.linkedin.com/in/username",
            })
            return

        async def ws_callback(msg: dict):
            try:
                await websocket.send_json(msg)
            except Exception:
                pass

        # Run enrichment pipeline
        profile = await person_intel_service.enrich(
            linkedin_url=linkedin_url,
            deep=deep,
            ws_callback=ws_callback,
        )

        # Save to database
        db = next(get_db())
        try:
            record = PersonIntel(
                linkedin_url=profile.linkedin_url,
                username=profile.username,
                name=profile.name,
                headline=profile.headline,
                location=profile.location,
                summary=profile.summary,
                emails=profile.emails,
                social_links=profile.social_links,
                articles=profile.articles,
                mentions=profile.mentions,
                companies=profile.companies,
                education=profile.education,
                skills=profile.skills,
                raw_sources=profile.raw_sources,
                status=profile.status,
                created_at=datetime.utcnow(),
            )
            db.add(record)
            db.commit()
            db.refresh(record)

            # Send final message with DB id
            await websocket.send_json({
                "step": "saved",
                "id": record.id,
                "profile": profile.model_dump(),
            })
        finally:
            db.close()

    except Exception as e:
        try:
            await websocket.send_json({"step": "error", "message": str(e)})
        except Exception:
            pass
    finally:
        try:
            await websocket.close()
        except Exception:
            pass


# --- REST Endpoints ---

@router.post("/api/person/enrich", response_model=dict)
async def enrich_person(
    request: EnrichRequest,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_active_user),
):
    """Start enrichment for a LinkedIn profile URL (batch/non-streaming)."""
    if "linkedin.com/in/" not in request.linkedin_url:
        raise HTTPException(
            status_code=400,
            detail="Invalid LinkedIn URL. Expected format: https://www.linkedin.com/in/username",
        )

    try:
        profile = await person_intel_service.enrich(
            linkedin_url=request.linkedin_url,
            deep=request.deep,
        )

        # Save to DB
        record = PersonIntel(
            linkedin_url=profile.linkedin_url,
            username=profile.username,
            name=profile.name,
            headline=profile.headline,
            location=profile.location,
            summary=profile.summary,
            emails=profile.emails,
            social_links=profile.social_links,
            articles=profile.articles,
            mentions=profile.mentions,
            companies=profile.companies,
            education=profile.education,
            skills=profile.skills,
            raw_sources=profile.raw_sources,
            status=profile.status,
            created_at=datetime.utcnow(),
        )
        db.add(record)
        db.commit()
        db.refresh(record)

        return {
            "id": record.id,
            "status": "success",
            "profile": profile.model_dump(),
        }

    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e))
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/api/person/history")
def get_person_history(
    skip: int = 0,
    limit: int = 30,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_active_user),
):
    """List previous person enrichment lookups."""
    records = (
        db.query(PersonIntel)
        .order_by(PersonIntel.id.desc())
        .offset(skip)
        .limit(limit)
        .all()
    )

    return [
        {
            "id": r.id,
            "linkedin_url": r.linkedin_url,
            "username": r.username,
            "name": r.name or "",
            "headline": r.headline or "",
            "email_count": len(r.emails or []),
            "status": r.status,
            "created_at": r.created_at.isoformat() if r.created_at else "",
        }
        for r in records
    ]


@router.get("/api/person/{record_id}")
def get_person_detail(
    record_id: int,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_active_user),
):
    """Get full person intel record by ID."""
    record = db.query(PersonIntel).filter(PersonIntel.id == record_id).first()
    if not record:
        raise HTTPException(status_code=404, detail="Record not found")

    return {
        "id": record.id,
        "linkedin_url": record.linkedin_url,
        "username": record.username,
        "name": record.name,
        "headline": record.headline,
        "location": record.location,
        "summary": record.summary,
        "emails": record.emails or [],
        "social_links": record.social_links or {},
        "articles": record.articles or [],
        "mentions": record.mentions or [],
        "companies": record.companies or [],
        "education": record.education or [],
        "skills": record.skills or [],
        "raw_sources": record.raw_sources or [],
        "status": record.status,
        "created_at": record.created_at.isoformat() if record.created_at else "",
    }


@router.delete("/api/person/{record_id}")
def delete_person_record(
    record_id: int,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_active_user),
):
    """Delete a person intel record."""
    record = db.query(PersonIntel).filter(PersonIntel.id == record_id).first()
    if not record:
        raise HTTPException(status_code=404, detail="Record not found")

    db.delete(record)
    db.commit()
    return {"status": "deleted", "id": record_id}
