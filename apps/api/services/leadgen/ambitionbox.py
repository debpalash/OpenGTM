"""
AmbitionBox API Client — Company search and job listings.

Reverse-engineered from AmbitionBox's internal service gateway.
No authentication required — uses public appId/systemId headers.

Endpoints:
  - Company search/listing with filters (industry, location, size, rating)
  - Company detail (reviews, ratings, benefits)
  - Job listings by company
  - Company comparison data
"""

import asyncio
import re
from typing import Optional
from dataclasses import dataclass, field

import aiohttp

BASE = "https://www.ambitionbox.com/servicegateway-ambitionbox"

HEADERS = {
    "appid": "125",
    "systemid": "local",
    "content-type": "application/json",
    "accept": "application/json",
    "user-agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36",
    "origin": "https://www.ambitionbox.com",
    "referer": "https://www.ambitionbox.com/list-of-companies",
}


@dataclass
class ABCompany:
    """Parsed company from AmbitionBox API."""
    company_id: int = 0
    name: str = ""
    short_name: str = ""
    url_name: str = ""
    logo_url: str = ""
    industry: str = ""
    # None = AmbitionBox has no rating for this company (unrated), which is NOT
    # the same as "rated 0.0". Coercing the API's null to 0 made unrated
    # companies sort below every genuinely bad one -- e.g. "SBI Kiosk Banking"
    # came back as rating 0 with 0 reviews and read as the worst bank in India,
    # when the truth is simply that nobody has reviewed it. A data gap is not a
    # zero.
    rating: Optional[float] = None
    review_count: Optional[int] = None
    jobs_count: int = 0
    salaries_count: int = 0
    interviews_count: int = 0
    employee_count: str = ""
    top_location: str = ""
    total_locations: int = 0
    is_verified: bool = False
    company_type: str = ""  # Public, Private, etc.
    highly_rated_for: list = field(default_factory=list)
    critically_rated_for: list = field(default_factory=list)

    @classmethod
    def from_api(cls, card: dict) -> "ABCompany":
        loc = card.get("topLocationDetail") or {}
        tag = card.get("tag") or {}
        return cls(
            company_id=card.get("companyId", 0),
            name=card.get("name", ""),
            short_name=card.get("shortName", ""),
            url_name=card.get("urlName", ""),
            logo_url=card.get("logoUrl", ""),
            industry=card.get("primaryIndustry", ""),
            # Preserve null. `or 0` would turn "unrated" into "rated 0.0".
            rating=(round(card["companyRating"], 1)
                    if card.get("companyRating") is not None else None),
            review_count=(card["reviewCount"]
                          if card.get("reviewCount") is not None else None),
            jobs_count=card.get("jobsCount", 0) or 0,
            salaries_count=card.get("salariesCount", 0) or 0,
            interviews_count=card.get("interviewsCount", 0) or 0,
            employee_count=card.get("totalEmployeesIndia", "") or card.get("totalEmployees", ""),
            top_location=loc.get("name", ""),
            total_locations=loc.get("totalLocationsCount", 0) or 0,
            is_verified=card.get("isVerifiedEmployer", False),
            company_type=tag.get("name", ""),
            highly_rated_for=[
                {"name": r["name"], "rating": r["ratings"]}
                for r in (card.get("highlyRatedFor") or [])
            ],
            critically_rated_for=[
                {"name": r["name"], "rating": r["ratings"]}
                for r in (card.get("criticallyRatedFor") or [])
            ],
        )

    def to_dict(self) -> dict:
        return {
            "company_id": self.company_id,
            "name": self.name,
            "short_name": self.short_name,
            "url_name": self.url_name,
            "logo_url": self.logo_url,
            "industry": self.industry,
            "rating": self.rating,
            "review_count": self.review_count,
            "jobs_count": self.jobs_count,
            "salaries_count": self.salaries_count,
            "interviews_count": self.interviews_count,
            "employee_count": self.employee_count,
            "top_location": self.top_location,
            "total_locations": self.total_locations,
            "is_verified": self.is_verified,
            "company_type": self.company_type,
            "highly_rated_for": self.highly_rated_for,
            "critically_rated_for": self.critically_rated_for,
            "profile_url": f"https://www.ambitionbox.com/overview/{self.url_name}-overview",
        }


@dataclass
class ABJob:
    """Parsed job from AmbitionBox API."""
    job_id: str = ""
    title: str = ""
    company: str = ""
    company_id: int = 0
    company_rating: float = 0.0
    job_profile: str = ""
    locations: list = field(default_factory=list)
    min_exp: int = 0
    max_exp: int = 0
    skills: list = field(default_factory=list)
    posted_on: str = ""
    portal: str = ""
    jdp_url: str = ""

    @classmethod
    def from_api(cls, j: dict) -> "ABJob":
        return cls(
            job_id=j.get("jobId", ""),
            title=j.get("title", ""),
            company=j.get("company", ""),
            company_id=j.get("companyId", 0),
            company_rating=round(j.get("companyRating", 0) or 0, 1),
            job_profile=j.get("jobProfile", ""),
            locations=j.get("locations", []),
            min_exp=j.get("minExp", 0) or 0,
            max_exp=j.get("maxExp", 0) or 0,
            skills=j.get("skills", []),
            posted_on=j.get("postedOn", ""),
            portal=j.get("portal", ""),
            jdp_url=f"https://www.ambitionbox.com{j['jdpUrl']}" if j.get("jdpUrl") else "",
        )

    def to_dict(self) -> dict:
        return {
            "job_id": self.job_id,
            "title": self.title,
            "company": self.company,
            "company_id": self.company_id,
            "company_rating": self.company_rating,
            "job_profile": self.job_profile,
            "locations": self.locations,
            "experience": f"{self.min_exp}-{self.max_exp} yrs" if self.max_exp else f"{self.min_exp}+ yrs",
            "skills": self.skills,
            "posted_on": self.posted_on,
            "portal": self.portal,
            "url": self.jdp_url,
        }


# ── API Client ──────────────────────────────────────────────────────


def _slugify(value: str) -> str:
    """Display name -> AmbitionBox filter slug.

    The gateway matches slugs, not display names: "IT Services & Consulting"
    must be sent as "it-services-and-consulting". A display name is silently
    ignored (no error, full unfiltered list back), which is why this is done
    for the caller rather than trusted to them.
    """
    s = value.strip().lower().replace("&", "and")
    s = re.sub(r"[^a-z0-9]+", "-", s)
    return s.strip("-")


class AmbitionBoxClient:
    """Async client for AmbitionBox's internal APIs."""

    async def _request(self, method: str, path: str, json_body: dict = None) -> dict:
        """Make a request to the AmbitionBox service gateway."""
        url = f"{BASE}/{path}"
        async with aiohttp.ClientSession() as session:
            async with session.request(
                method, url,
                headers=HEADERS,
                json=json_body,
                timeout=aiohttp.ClientTimeout(total=15),
            ) as resp:
                if resp.status != 200:
                    text = await resp.text()
                    raise RuntimeError(f"AmbitionBox API {resp.status}: {text[:200]}")
                return await resp.json()

    async def search_companies(
        self,
        page: int = 1,
        limit: int = 20,
        sort_by: str = "popular",
        industry: Optional[list[str]] = None,
        location: Optional[list[str]] = None,
        company_type: Optional[list[str]] = None,
        rating: Optional[str] = None,
    ) -> dict:
        """Search companies with optional filters.

        Args:
            page: Page number (1-indexed)
            limit: Results per page. NOTE: the gateway ignores this and always
                returns 20; kept for signature stability, applied client-side.
            sort_by: "popular", "rating", "reviews"
            industry: display names OR slugs, e.g. ["IT Services & Consulting"]
            rating: minimum rating as a string, e.g. "4.5" for 4.5+
            location: NOT SUPPORTED by this endpoint -- raises. See below.
            company_type: NOT SUPPORTED by this endpoint -- raises. See below.

        Returns:
            Dict with "companies" list and "total" count.

        Gateway filter contract (reverse-engineered 2026-07-16, verified by
        differential testing -- see the note below before changing any of it):

          * ``industries``: list[str] of SLUGS.  "IT Services & Consulting"
            must go over the wire as "it-services-and-consulting".
          * ``ratings``: a STRING, not a list.  ``"4.5"`` -> 4.5+.  Sending
            ``["4.5"]`` returns HTTP 422.
          * ``locations`` / ``companyTypes``: no working form found.  As a list
            they are silently dropped; as a string they 422.  AmbitionBox
            filters these by SEO path instead
            (/it-services-and-consulting-companies-in-pune), which this client
            does not implement.

        WHY THIS IS SO EXPLICIT: the previous implementation sent ``Industry``,
        ``Location``, ``CompanyType`` and ``Rating``.  The gateway accepts the
        request, ignores every one of those keys, and returns HTTP 200 with the
        full unfiltered popular list.  So a caller asking for "IT companies in
        Pune rated 4.5+" got TCS (3.3), HDFC Bank, Reliance Jio and Tata Steel
        -- 20 plausible rows, silently wrong, no error to notice.  Unsupported
        filters now raise instead of lying.
        """
        body: dict = {
            "isFilterApplied": True,
            "page": str(page),
            "sortBy": sort_by,
            "limit": limit,
        }
        if industry:
            body["industries"] = [_slugify(i) for i in industry]
        if rating:
            # String, not list -- a list is a 422.
            body["ratings"] = str(rating)
        if location:
            raise NotImplementedError(
                "AmbitionBox's gateway ignores location filters -- it filters "
                "location by SEO path (e.g. /it-services-and-consulting-"
                "companies-in-pune), which this client does not implement. "
                "Passing location would silently return unfiltered results, so "
                "it is refused rather than dropped."
            )
        if company_type:
            raise NotImplementedError(
                "AmbitionBox's gateway ignores company_type filters. See the "
                "location note above -- refused rather than silently dropped."
            )

        data = await self._request(
            "POST",
            "company-services/v0/listing/dream/companies/search",
            body,
        )

        cards = data.get("cards", [])
        companies = [ABCompany.from_api(c).to_dict() for c in cards]
        # The gateway ignores `limit` and always returns 20. Honor it here so
        # the caller gets what it asked for rather than a silent 20.
        if limit and len(companies) > limit:
            companies = companies[:limit]

        return {
            "companies": companies,
            "total": len(companies),
            "page": page,
        }

    async def get_company_jobs(
        self,
        company_id: int,
        page: int = 1,
    ) -> dict:
        """Get job listings for a specific company.

        Args:
            company_id: AmbitionBox company ID
            page: Page number

        Returns:
            Dict with "jobs" list and "pagination" info.
        """
        data = await self._request(
            "GET",
            f"jobs-services/v0/jobs/company/{company_id}?page={page}",
        )

        jobs = [ABJob.from_api(j).to_dict() for j in data.get("jobs", [])]
        pagination = data.get("pagination", {})

        return {
            "jobs": jobs,
            "total": pagination.get("count", 0),
            "page": pagination.get("currentPage", 1),
            "total_pages": pagination.get("totalPages", 1),
        }

    async def get_company_detail(self, company_id: int) -> dict:
        """Get detailed company info including ratings breakdown."""
        data = await self._request(
            "GET",
            f"company-services/v0/company/{company_id}/sectionalDetails",
        )
        return data

    async def get_similar_companies(self, company_id: int) -> list:
        """Get companies similar to the given one."""
        data = await self._request(
            "GET",
            f"insights-services/v0/company/{company_id}/similar-companies-v3",
        )
        return data

    async def search_and_collect(
        self,
        pages: int = 5,
        industry: Optional[list[str]] = None,
        location: Optional[list[str]] = None,
    ) -> list[dict]:
        """Collect companies across multiple pages.

        Returns flat list of company dicts.
        """
        all_companies = []
        seen = set()

        for page in range(1, pages + 1):
            try:
                result = await self.search_companies(
                    page=page,
                    industry=industry,
                    location=location,
                )
                for c in result["companies"]:
                    if c["company_id"] not in seen:
                        seen.add(c["company_id"])
                        all_companies.append(c)

                if len(result["companies"]) < 20:
                    break  # No more pages

                await asyncio.sleep(0.5)  # Rate limit
            except Exception:
                break

        return all_companies


# Module-level singleton
ambitionbox = AmbitionBoxClient()
