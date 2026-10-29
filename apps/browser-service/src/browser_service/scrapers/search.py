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
import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass

from ..egress import UpstreamProxy
from ..engines import Deadline, Deps
from ..errors import ServiceError
from ..models import (
    CrunchbaseRequest,
    GoogleSearchRequest,
    JobBoardsRequest,
    LinkedInRequest,
    ReviewDirectoriesRequest,
    SearchHit,
    SearchOut,
    SearchRequest,
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


# legacy leadgen/search_cache._RL_PATTERNS: rate-limit / block signals
_BLOCK_PATTERNS = ("ratelimit", "rate limit", "429", "403", "202", "too many", "blocked", "captcha")
_TIMEOUT_PATTERNS = ("timeout", "timed out")
_UNSET = object()


def classify_search_error(exc: BaseException) -> str:
    """Map a ddgs failure to a code. Block signals (legacy is_block_signal) are
    rate_limited or timeout; the Go side grows its per-host backoff on them."""
    if isinstance(exc, TimeoutError):
        return "timeout"
    text = f"{type(exc).__name__} {exc}".casefold()
    if any(p in text for p in _TIMEOUT_PATTERNS):
        return "timeout"
    if any(p in text for p in _BLOCK_PATTERNS):
        return "rate_limited"
    if "no results" in text:
        return "no_results"
    return "failed"


# ddgs threads cannot be cancelled: after a timeout the worker keeps running until
# ddgs' own socket timeout. A dedicated pool plus a slot counter released only when
# the thread really finishes bounds the total, however many requests time out.
MAX_DDGS_WORKERS = 4
_ddgs_pool = ThreadPoolExecutor(max_workers=MAX_DDGS_WORKERS, thread_name_prefix="ddgs")
_ddgs_slots = threading.BoundedSemaphore(MAX_DDGS_WORKERS)


async def ddgs_search(
    deps: Deps,
    method: str,
    search: str,
    max_results: int,
    deadline: Deadline,
    *,
    upstream: UpstreamProxy | None = None,
    backend: object = _UNSET,
    region: str | None = None,
    safesearch: str | None = None,
    timelimit: str | None = None,
) -> list[dict]:
    """One ddgs query over the egress proxy (chained to ``upstream`` when the
    caller picked one). Raises on failure (see _classify)."""
    if not _ddgs_slots.acquire(blocking=False):
        raise ServiceError("busy")
    try:
        session = deps.proxy.open_session(deps.settings.max_download_bytes, upstream=upstream)
    except BaseException:
        _ddgs_slots.release()
        raise
    proxy_url = deps.proxy.proxy_url(session)
    timeout = max(1, min(deps.settings.search_timeout_s, int(deadline.left())))
    backends = deps.settings.search_backends.strip() if backend is _UNSET else backend

    def run() -> list[dict]:
        try:
            return _run()
        finally:
            _ddgs_slots.release()

    def _run() -> list[dict]:
        from ddgs import DDGS

        kwargs: dict = {"max_results": max_results}
        if backends:
            kwargs["backend"] = backends
        if region:
            kwargs["region"] = region
        if safesearch:
            kwargs["safesearch"] = safesearch
        if timelimit:
            kwargs["timelimit"] = timelimit
        d = DDGS(proxy=proxy_url, timeout=timeout)
        return list(getattr(d, method)(search, **kwargs))

    try:
        # The worker thread cannot be cancelled; ddgs' own timeout bounds it and
        # the slot stays held until it exits.
        fut = asyncio.get_running_loop().run_in_executor(_ddgs_pool, run)
        return await asyncio.wait_for(fut, timeout=max(1.0, deadline.left()))
    finally:
        deps.proxy.close_session(session)


async def run_plan(
    deps: Deps,
    source: str,
    plan: list[Planned],
    delay_s: float,
    deadline: Deadline,
    upstream: UpstreamProxy | None = None,
) -> SearchResult:
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
            raw = await search(deps, p.method, p.search, p.max_results, deadline, upstream=upstream)
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
    return await run_plan(deps, source, plan, delay, deadline, upstream=req.upstream())


async def query(deps: Deps, req: SearchRequest, deadline: Deadline) -> SearchOut:
    """POST /v1/search: one ddgs query with the caller's options. Failures are
    a code in ``error`` (never upstream text); the Go side owns caching,
    backoff, keyed fallbacks and proxy rotation."""
    search = deps.search or ddgs_search
    if req.backends is None:
        backend: str | None = deps.settings.search_backends.strip() or None
    else:
        backend = ", ".join(req.backends) or None
    hits: list[SearchHit] = []
    err: str | None = None
    try:
        raw = await search(
            deps,
            req.method,
            req.query,
            req.max_results,
            deadline,
            upstream=req.upstream(),
            backend=backend,
            region=req.region,
            safesearch=req.safesearch,
            timelimit=req.timelimit,
        )
        hits = [_hit(h) for h in raw[: req.max_results] if isinstance(h, dict)]
        if not hits:
            err = "no_results"
    except Exception as e:
        err = classify_search_error(e)
        logger.info("search failed kind=%s", err)
    if err and deps.stats is not None:
        deps.stats.search_error(err)
    return SearchOut(query=req.query, method=req.method, hits=hits, error=err, elapsed_ms=deadline.elapsed_ms())
