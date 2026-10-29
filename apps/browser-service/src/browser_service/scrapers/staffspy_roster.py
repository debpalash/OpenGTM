"""POST /v1/staffspy: a company's LinkedIn staff roster (legacy
leadgen/enrichment/providers/staffspy_provider.py ``_scrape``).

Off unless BROWSER_STAFFSPY_ENABLED (LinkedIn ToS; legacy STAFFSPY_ENABLED).
Legacy read a pickled session file from disk; here the caller (Go) sends the
LinkedIn cookies per request and the service builds the requests session
itself (no pickle, nothing written to disk, nothing kept). All LinkedIn traffic
goes through the egress proxy. Seniority ranking and lead mapping stay in Go.
"""

from __future__ import annotations

import asyncio
import logging
from typing import Any

from ..engines import Deadline, Deps
from ..errors import ServiceError
from ..models import StaffRow, StaffSpyRequest, StaffSpyResult

logger = logging.getLogger("browser_service.scrapers.staffspy")

# staffspy Login.load_session sets these on every session it loads
STAFFSPY_HEADERS = {
    "User-Agent": "Mozilla/5.0 (Linux; U; Android 4.4.2; en-us; SCH-I535 Build/KOT49H) AppleWebKit/534.30 "
    "(KHTML, like Gecko) Version/4.0 Mobile Safari/534.30",
    "X-RestLi-Protocol-Version": "2.0.0",
    "X-Li-Track": '{"clientVersion":"1.13.1665"}',
}


def build_session(req: StaffSpyRequest, proxy_url: str) -> Any:
    import requests

    s = requests.Session()
    s.trust_env = False  # never pick up HTTP(S)_PROXY: the egress proxy is the only way out
    s.proxies = {"http": proxy_url, "https": proxy_url}
    for c in req.cookies:
        s.cookies.set(c.name, c.value, domain=c.domain, path=c.path or "/")
    s.headers.update(STAFFSPY_HEADERS)
    if "JSESSIONID" in s.cookies:
        s.headers["csrf-token"] = s.cookies["JSESSIONID"].replace('"', "")
    return s


def check_logged_in(session: Any) -> bool:
    """staffspy Login.check_logged_in."""
    try:
        res = session.get(
            "https://www.linkedin.com/voyager/api/organization/companies?q=universalName&universalName=amazon",
            timeout=20,
        )
        return res.status_code == 200
    except Exception:
        return False


def scrape_staff(session: Any, company: str, search_term: str, location: str, max_results: int) -> list[dict]:
    from staffspy import LinkedInAccount

    account = LinkedInAccount.__new__(LinkedInAccount)  # skip the constructor's interactive login
    account.session = session
    account.on_block = False
    account.log_level = 0
    df = account.scrape_staff(
        company_name=company,
        search_term=search_term or None,
        location=location or None,
        extra_profile_data=False,
        max_results=max_results,
    )
    if df is None or getattr(df, "empty", True):
        return []
    if account.on_block:
        raise RuntimeError("rate limited")
    return [dict(row) for _, row in df.iterrows()]


def _s(v: Any) -> str:
    if v is None:
        return ""
    try:
        if v != v:  # NaN from pandas
            return ""
    except Exception:  # noqa: S110
        pass
    return str(v).strip()


def to_rows(raw: list[dict]) -> list[StaffRow]:
    """Legacy _scrape row mapping."""
    out = []
    for r in raw:
        name = _s(r.get("name"))
        if not name:
            continue
        title = _s(r.get("current_position")) or _s(r.get("headline"))
        link = _s(r.get("profile_link")) or _s(r.get("profile_id"))
        out.append(
            StaffRow(name=name[:200], title=title[:300], linkedin=link[:500], location=_s(r.get("location"))[:200])
        )
    return out


async def scrape(deps: Deps, req: StaffSpyRequest, deadline: Deadline) -> StaffSpyResult:
    if not deps.settings.staffspy_enabled:
        raise ServiceError("engine_unavailable")
    session = deps.proxy.open_session(deps.settings.max_download_bytes, upstream=req.upstream())
    err = None
    rows: list[StaffRow] = []
    try:
        http = build_session(req, deps.proxy.proxy_url(session))
        try:
            if not await asyncio.to_thread(check_logged_in, http):
                err = "login_failed"
            else:
                raw = await asyncio.to_thread(
                    scrape_staff, http, req.company_name, req.search_term, req.location, req.max_results
                )
                rows = to_rows(raw)
                if not rows:
                    err = "no_staff_found"
        except Exception as e:
            name = type(e).__name__
            err = "rate_limited" if "TooManyRequests" in name or "rate limited" in str(e) else "failed"
            logger.info("staffspy failed kind=%s", err)
        finally:
            http.close()
    finally:
        deps.proxy.close_session(session)
    return StaffSpyResult(company_name=req.company_name, staff=rows, error=err, elapsed_ms=deadline.elapsed_ms())
