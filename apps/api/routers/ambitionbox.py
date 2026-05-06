"""
AmbitionBox Router — Search companies and jobs via AmbitionBox data.
"""

from typing import Optional
from fastapi import APIRouter

router = APIRouter(prefix="/api/ambitionbox", tags=["AmbitionBox"])


@router.get("/companies")
async def search_companies(
    page: int = 1,
    limit: int = 20,
    sort_by: str = "popular",
    industry: Optional[str] = None,
    location: Optional[str] = None,
    company_type: Optional[str] = None,
    rating: Optional[str] = None,
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
async def get_company_jobs(company_id: int, page: int = 1):
    """Get job listings for a specific company."""
    from apps.api.services.leadgen.ambitionbox import ambitionbox
    return await ambitionbox.get_company_jobs(company_id, page)


@router.get("/companies/{company_id}/detail")
async def get_company_detail(company_id: int):
    """Get detailed company info (ratings, benefits, etc.)."""
    from apps.api.services.leadgen.ambitionbox import ambitionbox
    return await ambitionbox.get_company_detail(company_id)


@router.get("/companies/{company_id}/similar")
async def get_similar_companies(company_id: int):
    """Get companies similar to the given one."""
    from apps.api.services.leadgen.ambitionbox import ambitionbox
    return await ambitionbox.get_similar_companies(company_id)


@router.get("/collect")
async def collect_companies(
    pages: int = 5,
    industry: Optional[str] = None,
    location: Optional[str] = None,
):
    """Collect companies across multiple pages.

    Query params:
        pages: Number of pages to scrape (each ~20 companies)
        industry: Comma-separated filter
        location: Comma-separated filter
    """
    from apps.api.services.leadgen.ambitionbox import ambitionbox

    companies = await ambitionbox.search_and_collect(
        pages=min(pages, 25),  # Cap at 500 companies
        industry=industry.split(",") if industry else None,
        location=location.split(",") if location else None,
    )
    return {"companies": companies, "total": len(companies)}
