"""Wire models (the OpenAPI schema the Go client is contract-tested against).

Requests forbid unknown fields; every list and string is bounded. Timeouts and
size caps in a request are clamped to the service limits.
"""

from __future__ import annotations

from typing import Annotated, Literal

from pydantic import BaseModel, ConfigDict, Field, StringConstraints

Term = Annotated[str, StringConstraints(strip_whitespace=True, min_length=1, max_length=200)]

Str200 = Field(min_length=1, max_length=200)


class _Req(BaseModel):
    model_config = ConfigDict(extra="forbid")


class ErrorBody(BaseModel):
    code: str
    message: str
    fields: list[str] | None = None


class ErrorEnvelope(BaseModel):
    error: ErrorBody


class Liveness(BaseModel):
    status: Literal["ok"]


class Health(BaseModel):
    status: Literal["ok", "degraded"]
    version: str
    browser: Literal["ready", "down", "disabled"]
    active: int
    capacity: int
    engines: list[str]


# ── render / screenshot ──────────────────────────────────────────────


class Cookie(_Req):
    name: str = Field(min_length=1, max_length=256)
    value: str = Field(max_length=4096)
    domain: str = Field(min_length=1, max_length=253)
    path: str = Field(default="/", max_length=1024)
    secure: bool = True
    http_only: bool = False


WaitUntil = Literal["commit", "domcontentloaded", "load", "networkidle"]


class RenderRequest(_Req):
    url: str = Field(min_length=1, max_length=4096)
    engine: Literal["browser", "impersonate", "flaresolverr"] = "browser"
    wait_until: WaitUntil = "domcontentloaded"
    wait_ms: int = Field(default=0, ge=0, le=15000, description="extra settle time after navigation")
    scroll: bool = Field(default=False, description="scroll to the bottom to trigger lazy loading")
    max_scrolls: int = Field(default=5, ge=0, le=20)
    include_html: bool = True
    include_text: bool = False
    max_bytes: int = Field(default=2 << 20, ge=1024, le=64 << 20, description="cap for html and for text, each")
    timeout_s: float = Field(default=30.0, ge=1.0, le=600.0)
    user_agent: str | None = Field(default=None, max_length=512)
    locale: str = Field(default="en-US", max_length=35)
    cookies: list[Cookie] = Field(default_factory=list, max_length=20)


class RenderResult(BaseModel):
    url: str
    final_url: str
    status: int
    engine: str
    title: str
    description: str
    html: str | None
    text: str | None
    html_truncated: bool
    text_truncated: bool
    challenge: bool = Field(description="the page looks like a bot challenge / JS wall")
    blocked_requests: int = Field(description="subrequests the egress guard refused")
    elapsed_ms: int


class ScreenshotRequest(_Req):
    url: str = Field(min_length=1, max_length=4096)
    wait_until: WaitUntil = "domcontentloaded"
    wait_ms: int = Field(default=0, ge=0, le=15000)
    full_page: bool = False
    format: Literal["jpeg", "png"] = "jpeg"
    quality: int = Field(default=60, ge=1, le=100)
    viewport_width: int = Field(default=1280, ge=320, le=1920)
    viewport_height: int = Field(default=900, ge=240, le=4000)
    timeout_s: float = Field(default=30.0, ge=1.0, le=600.0)


class ScreenshotResult(BaseModel):
    url: str
    final_url: str
    status: int
    content_type: str
    image_base64: str
    bytes: int
    elapsed_ms: int


# ── scrape: browser sources ──────────────────────────────────────────


class GoogleMapsRequest(_Req):
    query: str = Str200
    city: str = Field(min_length=1, max_length=100)
    max_results: int = Field(default=20, ge=1, le=50)
    timeout_s: float = Field(default=120.0, ge=5.0, le=600.0)


class MapPlace(BaseModel):
    name: str
    address: str
    phone: str
    website: str
    rating: str
    category: str


class GoogleMapsResult(BaseModel):
    source: Literal["google_maps"]
    search_url: str
    listings_found: int
    places: list[MapPlace]
    errors: int
    partial: bool
    elapsed_ms: int


class JustDialPageRequest(_Req):
    url: str = Field(min_length=1, max_length=4096, description="a justdial.com listing page")
    timeout_s: float = Field(default=45.0, ge=5.0, le=600.0)


class DirectoryListing(BaseModel):
    name: str
    phone_text: str
    address: str


class JustDialPageResult(BaseModel):
    source: Literal["justdial_page"]
    final_url: str
    status: int
    listings: list[DirectoryListing]
    elapsed_ms: int


# ── scrape: search-engine sources (ddgs) ─────────────────────────────


Queries = Field(min_length=1, max_length=10)
Cities = Field(default_factory=list, max_length=10)


class _SearchReq(_Req):
    max_results: int | None = Field(default=None, ge=1, le=50, description="per search; null = legacy default")
    delay_s: float | None = Field(default=None, ge=0.0, le=10.0, description="pause between searches; null = legacy")
    timeout_s: float = Field(default=120.0, ge=5.0, le=600.0)


class LinkedInRequest(_SearchReq):
    mode: Literal["companies", "people"] = "companies"
    queries: list[Term] = Field(default_factory=list, max_length=10)
    cities: list[Term] = Cities
    company_name: str = Field(default="", max_length=200, description="people mode")
    titles: list[Term] = Field(default_factory=list, max_length=10)


class JobBoardsRequest(_SearchReq):
    board: Literal["naukri", "indeed", "foundit"]
    queries: list[Term] = Queries
    cities: list[Term] = Field(min_length=1, max_length=10)


class CrunchbaseRequest(_SearchReq):
    queries: list[Term] = Queries
    cities: list[Term] = Field(min_length=1, max_length=10)


class ReviewDirectoriesRequest(_SearchReq):
    site: Literal["clutch", "goodfirms", "g2", "ambitionbox"]
    queries: list[Term] = Queries
    cities: list[Term] = Field(default_factory=list, max_length=10, description="ambitionbox only")


class WebDirectoriesRequest(_SearchReq):
    queries: list[Term] = Queries
    cities: list[Term] = Field(min_length=1, max_length=10)
    sites: list[Literal["justdial", "sulekha", "indiamart"]] = Field(
        default_factory=lambda: ["justdial", "sulekha", "indiamart"], min_length=1, max_length=3
    )


class GoogleSearchRequest(_SearchReq):
    mode: Literal["web", "news"] = "web"
    queries: list[Term] = Queries
    cities: list[Term] = Field(default_factory=list, max_length=10, description="web mode only")


class SearchHit(BaseModel):
    title: str
    url: str
    body: str
    date: str
    source: str


class SearchRun(BaseModel):
    variant: str = Field(description="board / site / mode the search ran for")
    query: str = Field(description="the caller's query term (or company name)")
    city: str
    term: str = Field(description="title term for linkedin people searches")
    search: str = Field(description="the exact search string sent to the engines")
    hits: list[SearchHit]
    error: str | None = Field(description="no_results | rate_limited | timeout | failed")


class SearchResult(BaseModel):
    source: str
    runs: list[SearchRun]
    partial: bool = Field(description="the deadline cut the search list short")
    elapsed_ms: int
