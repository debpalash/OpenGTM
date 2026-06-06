"""
Workbook Source Engine (P0) — sourcing as a first-class workbook primitive.

A `source` column carries an ICP config. When run, it materializes NEW rows
into the workbook by driving the existing `JobRunner` (6 strategies + 91-source
registry + dedup + scoring), then snapshotting the discovered leads into
`WorkbookRow`s — deduped against rows already in the workbook — and publishing a
`row_added` event per new row so the UI streams them live.

This inverts the old flow (source upstream → merge into workbook). Sourcing now
lives in the workbook. See features/workbook-v2-source-engine-spec.md (Pillar 1).

Execution runs on the durable queue_service worker via handle_source_workbook,
the same substrate that P-1 put under enrichment.
"""

import dataclasses
import logging
import os
import re
from typing import Any, Dict, List, Optional

from apps.api.database import SessionLocal
from apps.api.services.workbook.models import Workbook, WorkbookRow
from apps.api.services.workbook.enrichment import _make_redis, _broadcast

logger = logging.getLogger("workbook.source_engine")

# Lead fields worth snapshotting into a workbook row.
_ROW_FIELDS = (
    "company", "website", "email", "phone", "city", "state", "address",
    "contact_person", "contact_title", "specialization", "company_size",
    "description", "linkedin_url", "twitter_url", "facebook_url",
    "score", "score_tier", "source", "industry_tags",
)


# ── ICP → query ──────────────────────────────────────────────────────────

def build_query(icp: dict) -> str:
    """Turn an ICP config into a natural-language query for JobRunner.

    JobRunner does its own region/city detection from the query text, so the
    free-text description (which usually carries the geo) is the best signal;
    structured fields are appended as a fallback.
    """
    icp = icp or {}
    desc = (icp.get("description") or "").strip()
    if desc:
        return desc
    parts: List[str] = []
    if icp.get("industry"):
        parts.append(str(icp["industry"]))
    if icp.get("keywords_any"):
        parts.append(" ".join(str(k) for k in icp["keywords_any"]))
    geo = icp.get("geo") or []
    if geo:
        parts.append("in " + ", ".join(str(g) for g in geo))
    return " ".join(parts).strip()


# ── Dedup helpers (naive for P0; Pillar 1 upgrades to LeadDeduplicator) ───

def _norm_name(s: Any) -> str:
    s = str(s or "").lower()
    s = re.sub(r"\b(pvt|private|ltd|limited|llc|inc|corp|co|company|gmbh|plc|llp)\b", "", s)
    return re.sub(r"[^a-z0-9]", "", s)


def _norm_domain(url: Any) -> str:
    s = str(url or "").lower()
    s = re.sub(r"^https?://", "", s)
    s = re.sub(r"^www\.", "", s)
    return s.split("/")[0].strip()


def _existing_keys(db, workbook_id: str) -> tuple[set, set]:
    """Normalized company-name and domain keys already in the workbook."""
    names, domains = set(), set()
    rows = db.query(WorkbookRow.data).filter(WorkbookRow.workbook_id == workbook_id).all()
    for (data,) in rows:
        data = data or {}
        nk = _norm_name(data.get("company"))
        if nk:
            names.add(nk)
        dk = _norm_domain(data.get("website"))
        if dk:
            domains.add(dk)
    return names, domains


# ── Materialization ──────────────────────────────────────────────────────

async def materialize_source(workbook_id: str, column_id: str) -> Dict[str, Any]:
    """Run a workbook's source column: source leads and append new rows.

    Returns a summary {found, added, skipped, query}.
    """
    # Load workbook + source column config
    with SessionLocal() as db:
        wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
        if not wb:
            return {"error": "workbook_not_found"}
        col = next(
            (c for c in (wb.columns_config or [])
             if c.get("id") == column_id and c.get("type") == "source"),
            None,
        )
        if not col:
            return {"error": "source_column_not_found"}
        icp = col.get("icp") or {}
        target_rows = int(col.get("target_rows") or 0)
        workspace_id = (wb.source_config or {}).get("workspace_id", "") if wb.source_config else ""
        wb.status = "running"
        db.commit()

    query = build_query(icp)
    if not query:
        with SessionLocal() as db:
            wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
            if wb:
                wb.status = "draft"
                db.commit()
        return {"error": "empty_icp", "found": 0, "added": 0}

    # ── Run the existing sourcing pipeline (writes to leads DB) ──
    from apps.api.services.leadgen.job_runner import JobRunner
    from apps.api.services.leadgen.db import LeadDB

    runner = JobRunner()
    try:
        job_id = await runner.submit(query, workspace_id=workspace_id)
    except Exception as e:
        logger.error(f"Source run failed for {workbook_id}/{column_id}: {e}")
        with SessionLocal() as db:
            wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
            if wb:
                wb.status = "draft"
                db.commit()
        return {"error": str(e)[:200], "found": 0, "added": 0, "query": query}

    # ── Fetch the leads this job produced ──
    lead_db = LeadDB()
    try:
        leads = lead_db.get_leads(source=f"job:{job_id}", limit=10000)
    except Exception as e:
        logger.warning(f"Failed to fetch sourced leads: {e}")
        leads = []
    finally:
        lead_db.close()

    found = len(leads)
    redis_client = _make_redis()
    added = skipped = 0

    try:
        with SessionLocal() as db:
            names, domains = _existing_keys(db, workbook_id)
            max_pos = (
                db.query(WorkbookRow.position)
                .filter(WorkbookRow.workbook_id == workbook_id)
                .order_by(WorkbookRow.position.desc())
                .limit(1)
                .scalar()
            ) or 0

            for lead in leads:
                if target_rows and added >= target_rows:
                    break
                d = dataclasses.asdict(lead) if dataclasses.is_dataclass(lead) else dict(lead)

                nk = _norm_name(d.get("company"))
                dk = _norm_domain(d.get("website"))
                # Dedup vs existing rows AND within this batch
                if (nk and nk in names) or (dk and dk in domains):
                    skipped += 1
                    continue
                if nk:
                    names.add(nk)
                if dk:
                    domains.add(dk)

                row_data = {k: d.get(k) for k in _ROW_FIELDS if d.get(k) not in (None, "")}
                if not row_data.get("company"):
                    skipped += 1
                    continue

                max_pos += 1
                row = WorkbookRow(
                    workbook_id=workbook_id,
                    position=max_pos,
                    data=row_data,
                    lead_id=d.get("id"),
                    enrichments={},
                )
                db.add(row)
                db.flush()  # get row.id
                added += 1

                if redis_client is not None:
                    await _broadcast(redis_client, workbook_id, {
                        "type": "row_added",
                        "rowId": row.id,
                        "data": row_data,
                    })

            db.commit()

            wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
            if wb:
                total = db.query(WorkbookRow).filter(
                    WorkbookRow.workbook_id == workbook_id
                ).count()
                wb.total_rows = total
                wb.status = "draft"  # sourcing done; ready to enrich
                db.commit()
    finally:
        if redis_client is not None:
            try:
                await _broadcast(redis_client, workbook_id, {
                    "type": "source_done",
                    "columnId": column_id,
                    "found": found, "added": added, "skipped": skipped,
                })
            finally:
                try:
                    await redis_client.aclose()
                except Exception:
                    pass

    logger.info(
        f"Source {workbook_id}/{column_id}: found={found} added={added} skipped={skipped}"
    )
    return {"found": found, "added": added, "skipped": skipped, "query": query, "job_id": job_id}


async def handle_source_workbook(job_id: int, payload: dict):
    """queue_service handler for the 'source_workbook' job type."""
    logger.info(f"[job {job_id}] source_workbook {payload.get('workbook_id')}")
    result = await materialize_source(
        workbook_id=payload["workbook_id"],
        column_id=payload["column_id"],
    )
    logger.info(f"[job {job_id}] source_workbook done: {result}")


# ── Preview (no write, no sourcing) ──────────────────────────────────────

def preview_source(icp: dict, channels: Optional[dict] = None) -> Dict[str, Any]:
    """Dry-run: show the query that would run and which sources it would hit."""
    from apps.api.services.leadgen.source_registry import get_all_sources

    channels = channels or {}
    regions = channels.get("regions") or [None]
    categories = channels.get("categories") or [None]
    explicit = set(channels.get("explicit_sources") or [])

    matched: Dict[str, dict] = {}
    for region in regions:
        for category in categories:
            for s in get_all_sources(region=region, category=category):
                matched[s["name"]] = {
                    "name": s["name"], "label": s.get("label", s["name"]),
                    "category": s.get("category"), "region": s.get("region"),
                }
    if explicit:
        from apps.api.services.leadgen.source_registry import get_source
        for name in explicit:
            s = get_source(name)
            if s:
                matched[name] = {
                    "name": s["name"], "label": s.get("label", name),
                    "category": s.get("category"), "region": s.get("region"),
                }

    sources = sorted(matched.values(), key=lambda x: x["name"])
    return {
        "query": build_query(icp),
        "source_count": len(sources),
        "sources": sources,
    }
