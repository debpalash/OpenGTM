from fastapi import APIRouter, Depends, HTTPException, WebSocket, Query
from sqlalchemy.orm import Session
from datetime import datetime, timezone
from typing import List, Optional
from pydantic import BaseModel
from apps.api.database import get_db
from apps.api.models import User, ScrapeHistory, Link, EmailData
from apps.api.core.security import get_current_active_user
from apps.api.services.scraper import UniversalScraper

router = APIRouter(tags=["Scraper"])
scraper_service = UniversalScraper()


class ScrapeRequest(BaseModel):
    url: str


class ScrapeResponse(BaseModel):
    status: str
    url: str
    title: str = ""
    description: str = ""
    extracted_emails: List[str] = []
    word_count: int = 0
    preview_text: str = ""
    html_content: str = ""
    method: str = ""


@router.websocket("/ws/scraper/preview")
async def websocket_scraper_preview(websocket: WebSocket, token: str = Query(None)):
    await websocket.accept()
    try:
        data = await websocket.receive_json()
        url = data.get("url")
        if not url:
            await websocket.send_json({"error": "No URL provided"})
            return

        async def send_frame(image_bytes):
            try:
                await websocket.send_bytes(image_bytes)
            except Exception:
                pass

        await websocket.send_json({"type": "status", "message": "Starting scrape..."})

        result = await scraper_service.scrape(url, screenshot_callback=send_frame)

        await websocket.send_json({"type": "result", "data": result})

    except Exception as e:
        await websocket.send_json({"type": "error", "message": str(e)})
    finally:
        await websocket.close()


@router.post("/api/v2/scrape", response_model=ScrapeResponse)
async def scrape_url(
    request: ScrapeRequest,
    current_user: User = Depends(get_current_active_user),
    db: Session = Depends(get_db),
):
    try:
        data = await scraper_service.scrape(request.url)

        # Save History
        history = ScrapeHistory(
            url=data.get("url"),
            title=data.get("title"),
            method=data.get("method"),
            word_count=data.get("word_count", 0),
            email_count=len(data.get("extracted_emails", [])),
        )
        db.add(history)
        db.commit()
        db.refresh(history)

        # Merge into Main Database
        existing_link = db.query(Link).filter(Link.url == request.url).first()
        if not existing_link:
            existing_link = Link(
                url=request.url,
                status="Completed",
                created_at=datetime.now(timezone.utc).isoformat(),
                source="Scraper V2",
            )
            db.add(existing_link)
            db.commit()
            db.refresh(existing_link)

        # Add Emails
        emails = data.get("extracted_emails", [])
        for email in emails:
            exists = (
                db.query(EmailData)
                .filter(
                    EmailData.email == email,
                    EmailData.source_link_id == existing_link.id,
                )
                .first()
            )
            if not exists:
                new_email = EmailData(
                    name="", email=email, source_link_id=existing_link.id
                )
                db.add(new_email)
        db.commit()

        return data
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/api/v2/scrapes", response_model=List[dict])
def get_scrape_history(
    skip: int = 0,
    limit: int = 50,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_active_user),
):
    history = (
        db.query(ScrapeHistory)
        .order_by(ScrapeHistory.id.desc())
        .offset(skip)
        .limit(limit)
        .all()
    )
    return [
        {
            "id": h.id,
            "url": h.url,
            "title": h.title,
            "method": h.method,
            "word_count": h.word_count,
            "email_count": h.email_count,
            "created_at": h.created_at,
        }
        for h in history
    ]


@router.get("/api/sources")
def get_sources():
    """Return available document sources (public — no auth required)."""
    from apps.api.sources.registry import SourceRegistry
    registry = SourceRegistry()
    return {"sources": registry.list_sources()}


@router.get("/api/trending")
def get_trending(
    limit: int = 10,
    db: Session = Depends(get_db),
):
    """Return trending/recent scrapes as search results."""
    history = (
        db.query(ScrapeHistory).order_by(ScrapeHistory.id.desc()).limit(limit).all()
    )

    results = []
    for h in history:
        results.append(
            {
                "title": h.title or "Untitled Document",
                "url": h.url,
                "source": h.method or "web",
                "author": "Unknown",
                "year": str(datetime.fromisoformat(h.created_at).year)
                if h.created_at
                else "",
                "snippet": f"Extracted {h.email_count} emails and {h.word_count} words.",
                "file_type": "html",
                "thumbnail": None,
            }
        )

    if not results:
        # Return some mock data if history is empty so frontend doesn't look broken
        return [
            {
                "title": "Welcome to Universal Scraper",
                "url": "https://example.com/welcome",
                "source": "system",
                "author": "Admin",
                "year": "2024",
                "snippet": "Start searching to see results here.",
                "file_type": "guide",
                "thumbnail": None,
            }
        ]

    return results
