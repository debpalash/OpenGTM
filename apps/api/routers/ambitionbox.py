"""
AmbitionBox Router — Search companies and jobs via AmbitionBox data.
"""

from typing import Optional
from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel, Field

from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role

router = APIRouter(prefix="/api/ambitionbox", tags=["AmbitionBox"])


@router.get("/companies")
async def search_companies(
    page: int = Query(1, ge=1),
    limit: int = Query(20, ge=1, le=20),
    sort_by: str = "popular",
    industry: Optional[str] = None,
    location: Optional[str] = None,
    company_type: Optional[str] = None,
    rating: Optional[str] = None,
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Search AmbitionBox companies with filters.

    Query params:
        page: Page number (1-indexed)
        limit: Results per page
        sort_by: popular | rating | reviews
        industry: Comma-separated. e.g. "IT Services & Consulting,Banking"
        location: Comma-separated. e.g. "Bangalore/Bengaluru,Mumbai"
        company_type: Comma-separated. e.g. "Public,Private"
        rating: Min rating. e.g. "3.5"
    """
    from apps.api.services.leadgen.ambitionbox import ambitionbox

    return await ambitionbox.search_companies(
        page=page,
        limit=limit,
        sort_by=sort_by,
        industry=industry.split(",") if industry else None,
        location=location.split(",") if location else None,
        company_type=company_type.split(",") if company_type else None,
        rating=rating,
    )


@router.get("/companies/{company_id}/jobs")
async def get_company_jobs(company_id: int, page: int = 1, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get job listings for a specific company."""
    from apps.api.services.leadgen.ambitionbox import ambitionbox
    return await ambitionbox.get_company_jobs(company_id, page)


@router.get("/companies/{company_id}/detail")
async def get_company_detail(company_id: int, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get detailed company info (ratings, benefits, etc.)."""
    from apps.api.services.leadgen.ambitionbox import ambitionbox
    return await ambitionbox.get_company_detail(company_id)


@router.get("/companies/{company_id}/similar")
async def get_similar_companies(company_id: int, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get companies similar to the given one."""
    from apps.api.services.leadgen.ambitionbox import ambitionbox
    return await ambitionbox.get_similar_companies(company_id)


@router.get("/collect")
async def collect_companies(
    pages: int = Query(5, ge=1, le=25),
    limit: int = Query(100, ge=1, le=500),
    industry: Optional[str] = None,
    location: Optional[str] = None,
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Collect companies across multiple pages.

    Query params:
        pages: Number of pages to scrape (each ~20 companies)
        industry: Comma-separated filter
        location: Comma-separated filter
    """
    from apps.api.services.leadgen.ambitionbox import ambitionbox

    collection = await ambitionbox.collect_companies(
        requested_count=limit,
        max_pages=pages,
        industry=industry.split(",") if industry else None,
        location=location.split(",") if location else None,
    )
    result = collection.summary(include_records=False)
    result["companies"] = [record.as_dict() for record in collection.records]
    result["total"] = len(collection.records)
    return result


class AmbitionBoxImportRequest(BaseModel):
    name: str = "AmbitionBox Companies"
    industry: Optional[str] = None
    rating: Optional[str] = None
    sort_by: str = Field("popular", pattern="^(popular|rating|reviews)$")
    limit: int = Field(100, ge=1, le=500)
    workbook_id: Optional[str] = None
    replace_existing: bool = False


@router.post("/imports", status_code=202)
async def start_import(
    body: AmbitionBoxImportRequest,
    ctx: WorkspaceCtx = Depends(require_workspace_role("owner", "admin", "editor")),
):
    """Start a durable, checkpointed AmbitionBox-to-workbook import."""
    from apps.api.services.workbook.ambitionbox_import import (
        AmbitionBoxImportAlreadyRunning,
        start_ambitionbox_import,
    )

    try:
        return start_ambitionbox_import(
            workspace_id=ctx.workspace_id,
            name=body.name,
            industry=body.industry,
            rating=body.rating,
            sort_by=body.sort_by,
            requested_limit=body.limit,
            workbook_id=body.workbook_id,
            replace_existing=body.replace_existing,
        )
    except AmbitionBoxImportAlreadyRunning as exc:
        raise HTTPException(
            status_code=409,
            detail={
                "code": "connector_run_active",
                "message": str(exc),
                "run_id": exc.run_id,
            },
        ) from exc
