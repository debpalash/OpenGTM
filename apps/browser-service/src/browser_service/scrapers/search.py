"""Search-engine sources (ported from legacy leadgen/scrapers: linkedin,
job_boards, crunchbase, review_directories, web_directories, google_search and
proxy_client._ResilientDDGS).

Each source turns its params into the exact legacy search strings, runs them
through ddgs (multi-engine, keyless, browser-impersonating HTTP) via the egress
proxy and returns the raw hits per search. Not here (Go owns them): result
cache, per-host backoff, keyed engine fallback (API keys), proxy-pool rotation,
company-name extraction, lead mapping and dedup.
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass

from ..engines import Deadline, Deps
from ..errors import ServiceError
from ..models import (
    CrunchbaseRequest,
    GoogleSearchRequest,
    JobBoardsRequest,
    LinkedInRequest,
    ReviewDirectoriesRequest,
    SearchHit,
    SearchResult,
    SearchRun,
    WebDirectoriesRequest,
)

logger = logging.getLogger("browser_service.scrapers.search")

MAX_SEARCHES = 30
DEFAULT_PEOPLE_TITLES = ["CEO", "CTO", "CHRO", "HR Director", "Founder", "Managing Director"]


@dataclass(frozen=True)
class Planned:
    variant: str
    query: str
    city: str
    term: str
    search: str
    method: str  # "text" | "news"
    max_results: int


def plan_linkedin(r: LinkedInRequest) -> tuple[list[Planned], float]:
    if r.mode == "people":
        if not r.company_name.strip():
            raise ServiceError("bad_request")
        titles = r.titles or DEFAULT_PEOPLE_TITLES
        n = r.max_results or 2
        return [
            Planned("people", r.company_name, "", t, f'site:linkedin.com/in "{r.company_name}" "{t}" India', "text", n)
            for t in titles
        ], (1.5 if r.delay_s is None else r.delay_s)
    if not r.queries or not r.cities:
        raise ServiceError("bad_request")
    n = r.max_results or 10
    return [
        Planned("companies", q, c, "", f'site:linkedin.com/company "{q}" "{c}" India', "text", n)
        for c in r.cities
        for q in r.queries
    ], (2.0 if r.delay_s is None else r.delay_s)


_JOB_BOARDS = {
    "naukri": ('site:naukri.com "{q}" "{c}" company reviews hiring', 15),
    "indeed": ('site:indeed.co.in/cmp "{q}" "{c}"', 15),
    "foundit": ('site:foundit.in "{q}" "{c}" company', 10),
}


def plan_job_boards(r: JobBoardsRequest) -> tuple[list[Planned], float]:
    tpl, default_n = _JOB_BOARDS[r.board]
    n = r.max_results or default_n
    return [Planned(r.board, q, c, "", tpl.format(q=q, c=c), "text", n) for c in r.cities for q in r.queries], (
        2.0 if r.delay_s is None else r.delay_s
    )


def plan_crunchbase(r: CrunchbaseRequest) -> tuple[list[Planned], float]:
    n = r.max_results or 10
    return [
        Planned("crunchbase", q, c, "", f"{q} {c} site:crunchbase.com/organization", "text", n)
        for c in r.cities
        for q in r.queries
    ], (2.0 if r.delay_s is None else r.delay_s)


_REVIEW_SITES = {
    "clutch": ('site:clutch.co "{q}" India staffing HR', 20, False),
    "goodfirms": ('site:goodfirms.co "{q}" India', 20, False),
    "g2": ('site:g2.com "{q}" India staffing recruitment', 15, False),
    "ambitionbox": ('site:ambitionbox.com "{q}" "{c}" reviews', 15, True),
}


def plan_review_directories(r: ReviewDirectoriesRequest) -> tuple[list[Planned], float]:
    tpl, default_n, per_city = _REVIEW_SITES[r.site]
    n = r.max_results or default_n
    delay = 2.0 if r.delay_s is None else r.delay_s
    if per_city:
        if not r.cities:
            raise ServiceError("bad_request")
        return [Planned(r.site, q, c, "", tpl.format(q=q, c=c), "text", n) for c in r.cities for q in r.queries], delay
    return [Planned(r.site, q, "", "", tpl.format(q=q), "text", n) for q in r.queries], delay


_DIRECTORY_FILTERS = {"justdial": "site:justdial.com", "sulekha": "site:sulekha.com", "indiamart": "site:indiamart.com"}


def plan_web_directories(r: WebDirectoriesRequest) -> tuple[list[Planned], float]:
    n = r.max_results or 10
    sites = [s for s in ("justdial", "sulekha", "indiamart") if s in r.sites]  # legacy order
    return [
        Planned(s, q, c, "", f"{q} {c} {_DIRECTORY_FILTERS[s]}", "text", n)
        for c in r.cities
        for q in r.queries
        for s in sites
    ], (2.0 if r.delay_s is None else r.delay_s)


def plan_google_search(r: GoogleSearchRequest) -> tuple[list[Planned], float]:
    delay = 2.0 if r.delay_s is None else r.delay_s
    if r.mode == "news":
        n = r.max_results or 20
        return [
            Planned(
                "news", q, "", "", f'"{q}" India funding OR expansion OR launched OR partnership 2025 2026', "news", n
            )
            for q in r.queries
        ], delay
    if not r.cities:
        raise ServiceError("bad_request")
    n = r.max_results or 15
    return [
        Planned("web", q, c, "", f'"{q}" "{c}" India contact email phone', "text", n)
        for c in r.cities
        for q in r.queries
    ], delay


def _hit(raw: dict) -> SearchHit:
    def s(key: str, cap: int) -> str:
        v = raw.get(key)
        return str(v)[:cap] if v is not None else ""

    return SearchHit(
        title=s("title", 500),
        url=(s("href", 2048) or s("url", 2048)),
        body=s("body", 2000),
        date=s("date", 64),
        source=s("source", 200),
    )


def _classify(exc: BaseException) -> str:
    name = type(exc).__name__
    if "Ratelimit" in name:
        return "rate_limited"
    if "Timeout" in name:
        return "timeout"
    if "No results" in str(exc):
        return "no_results"
    return "failed"


async def ddgs_search(deps: Deps, method: str, search: str, max_results: int, deadline: Deadline) -> list[dict]:
    """One ddgs query over the egress proxy. Raises on failure (see _classify)."""
    session = deps.proxy.open_session(deps.settings.max_download_bytes)
    proxy_url = deps.proxy.proxy_url(session)
    timeout = max(1, min(deps.settings.search_timeout_s, int(deadline.left())))
    backends = deps.settings.search_backends.strip()

    def run() -> list[dict]:
        from ddgs import DDGS

        kwargs: dict = {"max_results": max_results}
        if backends:
            kwargs["backend"] = backends
        d = DDGS(proxy=proxy_url, timeout=timeout)
        return list(getattr(d, method)(search, **kwargs))

    try:
        # The worker thread cannot be cancelled; ddgs' own timeout bounds it.
        return await asyncio.wait_for(asyncio.to_thread(run), timeout=max(1.0, deadline.left()))
    finally:
        deps.proxy.close_session(session)


async def run_plan(deps: Deps, source: str, plan: list[Planned], delay_s: float, deadline: Deadline) -> SearchResult:
    if len(plan) > MAX_SEARCHES:
        raise ServiceError("bad_request")
    search = deps.search or ddgs_search
    runs: list[SearchRun] = []
    partial = False
    for i, p in enumerate(plan):
        if i and delay_s:
            if deadline.left() <= delay_s + 1:
                partial = True
                break
            await asyncio.sleep(delay_s)
        if deadline.left() < 1:
            partial = True
            break
        hits: list[SearchHit] = []
        err: str | None = None
        try:
            raw = await search(deps, p.method, p.search, p.max_results, deadline)
            hits = [_hit(h) for h in raw[: p.max_results] if isinstance(h, dict)]
            if not hits:
                err = "no_results"
        except TimeoutError:
            err = "timeout"
        except Exception as e:
            err = _classify(e)
            logger.info("search failed source=%s kind=%s", source, err)
        runs.append(
            SearchRun(variant=p.variant, query=p.query, city=p.city, term=p.term, search=p.search, hits=hits, error=err)
        )
    return SearchResult(source=source, runs=runs, partial=partial, elapsed_ms=deadline.elapsed_ms())


PLANNERS = {
    "linkedin": plan_linkedin,
    "job_boards": plan_job_boards,
    "crunchbase": plan_crunchbase,
    "review_directories": plan_review_directories,
    "web_directories": plan_web_directories,
    "google_search": plan_google_search,
}


async def scrape(deps: Deps, source: str, req, deadline: Deadline) -> SearchResult:
    plan, delay = PLANNERS[source](req)
    return await run_plan(deps, source, plan, delay, deadline)
