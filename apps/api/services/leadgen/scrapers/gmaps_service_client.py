"""
Google Maps high-throughput client — gosom/google-maps-scraper REST sidecar.

The Python GMaps scraper is fine for small jobs; gosom's Go engine
(research/maps-local/google-maps-scraper) handles volume with a job queue and a
REST API. Run it as a sidecar and this client submits a job, polls for completion,
and returns the CSV rows (50+ fields incl. emails/phones/socials/reviews).

ACTIVATION: run the gosom web service and point this at it, e.g.
    docker run -d -p 8080:8080 gosom/google-maps-scraper -web
    export GMAPS_SCRAPER_URL=http://localhost:8080
Without it, `is_available()` is False and callers fall back to the in-process scraper.

API (verified from the cloned repo's web/static/spec/spec.yaml):
    POST   /api/v1/jobs            {name, keywords[], lang, zoom, depth, max_time} -> {id,...}
    GET    /api/v1/jobs/{id}       -> {status: pending|working|ok|failed, ...}
    GET    /api/v1/jobs/{id}/download -> CSV
"""

import asyncio
import csv
import io
import logging
import os
from typing import Dict, List, Optional

logger = logging.getLogger("leadgen.gmaps_service")


def _endpoint() -> str:
    return (os.getenv("GMAPS_SCRAPER_URL", "") or "").rstrip("/")


def is_available() -> bool:
    return bool(_endpoint())


async def search_places(
    keywords: List[str],
    lang: str = "en",
    zoom: int = 15,
    depth: int = 1,
    max_time: int = 3600,
    poll_interval: float = 5.0,
    overall_timeout: float = 1800.0,
) -> List[Dict[str, str]]:
    """Run a GMaps scrape job to completion and return result rows.

    Returns [] when the service is unset/unreachable or the job fails — callers
    fall back to the in-process scraper.
    """
    base = _endpoint()
    if not base:
        logger.debug("GMAPS_SCRAPER_URL not configured — skipping gosom service")
        return []

    try:
        import httpx
    except Exception:
        return []

    body = {
        "name": " / ".join(keywords)[:120] or "leadgen job",
        "keywords": keywords,
        "lang": lang,
        "zoom": zoom,
        "depth": depth,
        "max_time": max_time,
    }

    try:
        async with httpx.AsyncClient(timeout=30.0) as client:
            # 1) create job
            r = await client.post(f"{base}/api/v1/jobs", json=body)
            if r.status_code not in (200, 201):
                logger.warning("gosom job create failed: HTTP %s %s", r.status_code, r.text[:200])
                return []
            job_id = (r.json() or {}).get("id")
            if not job_id:
                return []

            # 2) poll until terminal
            waited = 0.0
            status = "pending"
            while waited < overall_timeout:
                await asyncio.sleep(poll_interval)
                waited += poll_interval
                jr = await client.get(f"{base}/api/v1/jobs/{job_id}")
                if jr.status_code != 200:
                    continue
                status = (jr.json() or {}).get("status", "")
                if status in ("ok", "failed"):
                    break
            if status != "ok":
                logger.warning("gosom job %s ended status=%s", job_id, status or "timeout")
                return []

            # 3) download CSV results
            dr = await client.get(f"{base}/api/v1/jobs/{job_id}/download")
            if dr.status_code != 200 or not dr.text.strip():
                return []
            rows = list(csv.DictReader(io.StringIO(dr.text)))
            logger.info("gosom job %s returned %d places", job_id, len(rows))
            return rows
    except Exception as e:
        logger.warning("gosom service error: %s", e)
        return []
