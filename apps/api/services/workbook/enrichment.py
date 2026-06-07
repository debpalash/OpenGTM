"""
Workbook Enrichment Service — Hybrid model.

Orchestrates enrichment for workbook leads:
  1. For known Lead fields (email, phone, etc.) → writes BACK to the Lead record
  2. For AI/computed columns → stores in WorkbookEnrichment overlay table
  3. Broadcasts updates via Redis pub/sub → WebSocket

Can be called directly (sync) or via the BullMQ worker (async).
"""

import asyncio
import json
import logging
import os
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional

from sqlalchemy.orm import Session

from apps.api.database import SessionLocal
from apps.api.services.workbook.models import Workbook, WorkbookEnrichment, WorkbookRow, LEAD_FIELD_MAP
from apps.api.services.workbook.providers import get_provider, list_providers
from apps.api.services.workbook.ai_column import execute_ai_column
from apps.api.services.workbook.output import execute_output_column
from apps.api.services.workbook.conditions import evaluate_condition
from apps.api.services.leadgen.enrichment.provider import (
    EnrichmentProvider, EnrichmentResult, WaterfallEnricher,
)
from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.db import LeadDB

logger = logging.getLogger("workbook.enrichment")

# ── Default waterfall chains per target field ────────────────────────────
# Free OSS scrapers first, paid APIs as fallback.
# When a workbook column doesn't define a custom waterfall, this is used.
DEFAULT_WATERFALLS = {
    # Email: structured page data → website crawl → search → pattern gen → paid APIs
    "email": [
        "deep_scraper", "jsonld_firmographics", "website_scraper", "email_harvester",
        "ddg_email", "mailscout",
        "hunter_io", "apollo_io", "snovio", "prospeo",
    ],
    # Email verification: SMTP → holehe (120+ site check) → paid verify
    "email_confidence": ["mailscout", "holehe", "abstract_api", "debounce"],
    "email_verify": ["mailscout", "holehe", "abstract_api", "debounce"],
    # Phone: structured page data → website crawl → local dirs → paid APIs
    "phone": [
        "deep_scraper", "jsonld_firmographics", "website_scraper", "local_business",
        "ddg_company", "facebook_pages",
        "apollo_io", "people_data_labs",
    ],
    # Company description / info
    "description": [
        "deep_scraper", "jsonld_firmographics", "website_scraper", "ddg_company",
        "company_intel",
    ],
    # Decision makers / contacts (staffspy = full roster; gated, fails over gracefully)
    "decision_makers": ["staffspy", "deep_scraper", "crosslinked", "decision_maker"],
    "contact_person": ["deep_scraper", "crosslinked", "decision_maker", "staffspy", "people_data_labs"],
    "contact_title": ["deep_scraper", "crosslinked", "decision_maker", "staffspy"],
    # Social links — schema.org sameAs is authoritative, try it first
    "linkedin_url": ["jsonld_firmographics", "deep_scraper", "website_scraper", "social_finder"],
    "twitter_url": ["jsonld_firmographics", "deep_scraper", "website_scraper", "social_finder"],
    "facebook_url": ["jsonld_firmographics", "deep_scraper", "website_scraper", "social_finder"],
    # Company metadata
    "company_size": ["deep_scraper", "website_scraper", "company_intel", "people_data_labs"],
    "industry_tags": ["deep_scraper", "website_scraper", "local_business", "company_intel"],
    # Address — PostalAddress schema is authoritative, try it first
    "address": ["jsonld_firmographics", "deep_scraper", "local_business", "google_maps"],
    "founding_year": ["jsonld_firmographics", "deep_scraper", "company_intel"],
    "founded_year": ["jsonld_firmographics", "deep_scraper", "company_intel"],
    # Funding & intelligence
    "funding_stage": ["company_intel"],
    "last_funding_amount": ["company_intel"],
    "investors": ["company_intel"],
    "recent_news": ["company_intel"],
    # Tech stack
    "technologies": ["tech_stack"],
    # Hiring signals
    # Free, reliable public ATS boards first; jobspy aggregator as fallback.
    "hiring_signals": ["ats_hiring", "jobspy"],
    # Scoring
    "score": ["lead_scorer"],
}


def _lead_dict_to_lead(lead_data: dict) -> Lead:
    """Convert a lead dict from SQLite into a Lead dataclass."""
    return Lead.from_dict(lead_data)


def _get_lead_values(lead_data: dict, columns_config: list) -> Dict[str, str]:
    """Build a flat {column_id: value} dict from lead data for template resolution."""
    values = {}
    for col in columns_config:
        col_id = col.get("id", "")
        lead_field = col.get("lead_field", col_id)

        if col.get("type") == "lead_field" and lead_field in lead_data:
            values[col_id] = str(lead_data.get(lead_field, "") or "")
            values[lead_field] = str(lead_data.get(lead_field, "") or "")
        else:
            values[col_id] = str(lead_data.get(col_id, "") or "")

    # Also add all raw lead fields for flexible template resolution
    for k, v in lead_data.items():
        if k not in values:
            values[k] = str(v or "")

    return values


async def enrich_cell(
    db: Session,
    workbook_id: str,
    lead_id: int,
    col_id: str,
    col_config: dict,
    lead_data: dict,
    columns_config: list,
    redis_client=None,
) -> Dict[str, Any]:
    """Enrich a single cell for a lead in a workbook.

    Routes by column type:
      - enrichment/waterfall → provider chain, writes to Lead if target_field set
      - ai_formula → LLM, stores in WorkbookEnrichment
    """
    col_type = col_config.get("type", "enrichment")

    # ── Conditional execution ─────────────────────────────────────────
    if col_config.get("condition"):
        # Build cells-like dict for condition evaluation
        cells = {k: {"value": v, "status": "complete"} for k, v in lead_data.items()}
        should_run = evaluate_condition(col_config["condition"], cells, columns_config)
        if not should_run:
            _set_enrichment(db, workbook_id, lead_id, col_id, None, "skipped")
            if redis_client:
                await _broadcast(redis_client, workbook_id, {
                    "type": "cell_update", "leadId": lead_id, "colId": col_id,
                    "status": "skipped", "value": None,
                })
            return {"success": False, "value": None, "error": "condition_not_met"}

    # ── Output columns are side-effecting → run-once by default ────────
    # Don't re-push to a webhook/CRM/sequencer on a re-run unless the column
    # explicitly opts out (run_once=False) or the cell isn't already complete.
    if col_type == "output" and col_config.get("run_once", True):
        prior = db.query(WorkbookEnrichment).filter(
            WorkbookEnrichment.workbook_id == workbook_id,
            WorkbookEnrichment.lead_id == lead_id,
            WorkbookEnrichment.column_id == col_id,
        ).first()
        if prior and prior.status == "complete":
            return {"success": True, "value": prior.value, "provider": prior.provider,
                    "error": None, "skipped": True}

    # Mark as running
    _set_enrichment(db, workbook_id, lead_id, col_id, None, "running")
    if redis_client:
        await _broadcast(redis_client, workbook_id, {
            "type": "cell_update", "leadId": lead_id, "colId": col_id,
            "status": "running", "value": None,
        })

    # ── Route by column type ──────────────────────────────────────────
    if col_type == "ai_formula":
        # AI Column → LLM
        prompt = col_config.get("prompt", "")
        if not prompt:
            _set_enrichment(db, workbook_id, lead_id, col_id, None, "error", error="no_prompt")
            return {"success": False, "value": None, "error": "no_prompt"}

        # Build cells dict for AI template resolution
        cells = {k: {"value": v} for k, v in lead_data.items()}
        ai_result = await execute_ai_column(
            prompt_template=prompt,
            row_cells=cells,
            columns_config=columns_config,
        )
        result_value = ai_result.get("value")
        result_provider = "ai"
        result_error = ai_result.get("error")

    elif col_type == "output":
        # Output Column → push the row to an external destination
        out = await execute_output_column(
            col_config=col_config,
            lead_data=lead_data,
            columns_config=columns_config,
            workbook_id=workbook_id,
            lead_id=lead_id,
        )
        result_value = out.get("value")
        result_provider = col_config.get("destination", "output")
        result_error = out.get("error")

    elif col_type == "research":
        # Research Column → bounded web-research agent (Claygent-style).
        # Lazy import to avoid a circular import (research_column imports helpers
        # from this module).
        from apps.api.services.workbook.research_column import execute_research_column
        prompt = col_config.get("prompt", "")
        if not prompt:
            _set_enrichment(db, workbook_id, lead_id, col_id, None, "error", error="no_prompt")
            return {"success": False, "value": None, "error": "no_prompt"}
        res = await execute_research_column(
            prompt_template=prompt,
            lead_data=lead_data,
            columns_config=columns_config,
            max_steps=col_config.get("max_steps", 4),
            output_format=col_config.get("output_format", "text"),
        )
        result_value = res.get("value")
        result_provider = "research"
        result_error = res.get("error")

    elif col_type == "agent":
        # Goal-directed enrichment — agent picks tools dynamically (Pillar 4).
        from apps.api.services.workbook.agent_column import run_agent_cell
        agent_result = await run_agent_cell(db, workbook_id, lead_id, col_config, lead_data)
        result_value = agent_result.get("value")
        result_provider = agent_result.get("provider") or "agent"
        result_error = agent_result.get("error")

    elif col_type == "http":
        # HTTP action column — call an arbitrary API per row, extract via JSONPath.
        from apps.api.services.workbook.http_column import execute_http_column
        http_result = await execute_http_column(col_config, lead_data, columns_config)
        result_value = http_result.get("value")
        result_provider = "http"
        result_error = http_result.get("error")

    else:
        # Enrichment/Waterfall → provider chain
        lead = _lead_dict_to_lead(lead_data)
        explicit_chain = col_config.get("waterfall") or ([col_config.get("provider")] if col_config.get("provider") else [])

        # The target_field tells us which Lead field this column is enriching (e.g. "email")
        # If set, we ONLY extract that specific field from the provider result.
        target_field = col_config.get("target_field") or col_config.get("lead_field") or col_id

        # ── Resolve provider chain with DEFAULT_WATERFALLS ──
        # If the column has an explicit waterfall, use it but prepend any
        # OSS providers from the default chain that are missing.
        # If no explicit chain, use the full default.
        if explicit_chain:
            default_chain = DEFAULT_WATERFALLS.get(target_field, [])
            # Prepend default OSS providers that aren't already in the explicit chain
            oss_additions = [p for p in default_chain if p not in explicit_chain]
            provider_chain = oss_additions + explicit_chain
        else:
            provider_chain = DEFAULT_WATERFALLS.get(target_field, [])

        result_value = None
        result_provider = None
        result_error = None
        result_confidence = 0.0

        # ── Pillar 2: cost-aware ordering + budget ceiling ──
        from apps.api.services.workbook import planner as _planner
        wb_row = db.query(Workbook.budget_max_usd, Workbook.budget_spent_usd).filter(
            Workbook.id == workbook_id
        ).first()
        budget_max = (wb_row[0] or 0.0) if wb_row else 0.0
        budget_spent = (wb_row[1] or 0.0) if wb_row else 0.0
        budget_remaining = (budget_max - budget_spent) if budget_max > 0 else None
        provider_chain = _planner.order_chain(db, target_field, provider_chain, budget_remaining)

        # Fields that are structured/JSON — NEVER put in a cell, always write-back only
        STRUCTURED_FIELDS = {"decision_makers", "hiring_signals", "secondary_emails", "secondary_phones"}

        import time as _time
        for provider_name in provider_chain:
            provider = get_provider(provider_name)
            if not provider:
                logger.warning(f"Provider '{provider_name}' not found, skipping")
                continue

            _t0 = _time.monotonic()
            try:
                # Per-provider timeout so a slow provider (e.g. holehe's 120-site
                # check, deep_scraper's 8-page crawl) can't stall the waterfall.
                result = await asyncio.wait_for(
                    provider.enrich(lead),
                    timeout=float(os.getenv("WORKBOOK_PROVIDER_TIMEOUT", "30")),
                )
                _latency_ms = (_time.monotonic() - _t0) * 1000.0
                _planner.record_attempt(
                    db, provider_name, target_field,
                    success=bool(result.success and result.fields),
                    confidence=(result.confidence or provider.default_confidence),
                    latency_ms=_latency_ms,
                )
                if result.success and result.fields:
                    # ── Write back ALL scalar Lead fields from the result ──
                    for field_name, value in result.fields.items():
                        if not value or value == "" or value == "N/A":
                            continue

                        # Structured fields → write back to Lead only, never to cell
                        if field_name in STRUCTURED_FIELDS:
                            _write_back_to_lead(lead_id, field_name, value, provider_name)
                            continue

                        # Write back any known Lead field
                        if field_name in LEAD_FIELD_MAP and field_name != target_field:
                            _write_back_to_lead(lead_id, field_name, value, provider_name)

                    # ── Extract the TARGETED field for this cell ──
                    if target_field in result.fields:
                        cell_value = result.fields[target_field]
                        if cell_value and cell_value != "" and cell_value != "N/A":
                            # Enforce scalar: reject JSON blobs for cell display
                            sv = str(cell_value)
                            if sv.startswith("[{") or sv.startswith("{\""):
                                # Structured data — write to Lead, show summary in cell
                                _write_back_to_lead(lead_id, target_field, cell_value, provider_name)
                                try:
                                    parsed = json.loads(sv)
                                    if isinstance(parsed, list) and parsed:
                                        first = parsed[0]
                                        name = first.get("name") or first.get("email") or ""
                                        if name:
                                            result_value = f"{name}" + (f" +{len(parsed)-1} more" if len(parsed) > 1 else "")
                                        else:
                                            result_value = f"{len(parsed)} results"
                                    else:
                                        result_value = sv[:80]
                                except (json.JSONDecodeError, TypeError):
                                    result_value = sv[:80]
                            else:
                                result_value = sv
                            result_provider = provider_name
                            result_confidence = result.confidence or provider.default_confidence

                if result_value:
                    # ── Charge budget for a successful PAID provider ──
                    if _planner.is_paid(provider_name):
                        cost = _planner.provider_cost(provider_name)
                        db.query(Workbook).filter(Workbook.id == workbook_id).update(
                            {Workbook.budget_spent_usd: (Workbook.budget_spent_usd + cost)},
                            synchronize_session=False,
                        )
                    break  # Waterfall: stop at first success
            except asyncio.TimeoutError:
                logger.warning(f"Provider {provider_name} timed out for lead {lead_id}")
                result_error = "timeout"
            except Exception as e:
                _latency_ms = (_time.monotonic() - _t0) * 1000.0
                logger.error(f"Provider {provider_name} failed for lead {lead_id}: {e}")
                result_error = str(e)[:200]
                _planner.record_attempt(
                    db, provider_name, target_field,
                    success=False, latency_ms=_latency_ms,
                    rate_limited=_planner.looks_rate_limited(result_error),
                )

    # ── Write results ─────────────────────────────────────────────────
    if result_value:
        # Always store in enrichment overlay (value is already scalar/summary)
        _set_enrichment(db, workbook_id, lead_id, col_id, result_value, "complete", provider=result_provider)
    else:
        _set_enrichment(db, workbook_id, lead_id, col_id, None, "error", error=result_error or "no_data")

    db.commit()

    # Broadcast result
    if redis_client:
        await _broadcast(redis_client, workbook_id, {
            "type": "cell_update",
            "leadId": lead_id,
            "colId": col_id,
            "value": result_value,
            "status": "complete" if result_value else "error",
            "provider": result_provider,
            "error": result_error if not result_value else None,
        })

    return {
        "success": bool(result_value),
        "value": result_value,
        "provider": result_provider,
        "error": result_error,
    }


def _set_enrichment(
    db: Session, workbook_id: str, lead_id: int, column_id: str,
    value: Any, status: str, provider: str = None, error: str = None,
):
    """Upsert a WorkbookEnrichment record.

    enrich_cell calls this twice per cell in one session ("running" then the
    result) and commits once at the end. The session uses autoflush=False, so a
    plain SELECT won't see the still-pending "running" row — we'd insert a second
    row (duplicate / unique-constraint violation). So also check the session's
    pending inserts. We deliberately do NOT flush here: flushing would acquire
    the DB write lock early and hold it across the provider/LLM call.
    """
    existing = db.query(WorkbookEnrichment).filter(
        WorkbookEnrichment.workbook_id == workbook_id,
        WorkbookEnrichment.lead_id == lead_id,
        WorkbookEnrichment.column_id == column_id,
    ).first()

    if existing is None:
        # Match a not-yet-flushed row added earlier in this same session.
        for obj in db.new:
            if (isinstance(obj, WorkbookEnrichment)
                    and obj.workbook_id == workbook_id
                    and obj.lead_id == lead_id
                    and obj.column_id == column_id):
                existing = obj
                break

    if existing:
        existing.value = value
        existing.status = status
        existing.provider = provider
        existing.error = error
    else:
        db.add(WorkbookEnrichment(
            workbook_id=workbook_id,
            lead_id=lead_id,
            column_id=column_id,
            value=value,
            status=status,
            provider=provider,
            error=error,
        ))
    # Note: caller is responsible for db.commit()


def _write_back_to_lead(lead_id: int, field: str, value: str, provider: str = None):
    """Write an enrichment result back to the Lead record (source of truth)."""
    try:
        lead_db = LeadDB()
        updates = {field: value, "updated_at": datetime.now(timezone.utc).isoformat()}
        # Also store provenance
        if provider and field == "email":
            updates["email_provider"] = provider
        elif provider and field == "phone":
            updates["phone_provider"] = provider

        set_clause = ", ".join(f"{k} = ?" for k in updates)
        values = list(updates.values()) + [lead_id]
        lead_db.conn.execute(f"UPDATE leads SET {set_clause} WHERE id = ?", values)
        lead_db.conn.commit()
        lead_db.close()
        logger.info(f"Wrote {field}={value[:50]} back to lead {lead_id}")
    except Exception as e:
        logger.warning(f"Failed to write back to lead {lead_id}: {e}")


async def enrich_workbook_leads(
    db: Session,
    workbook_id: str,
    leads: list[dict],
    columns: list[dict],
    columns_config: list[dict],
    redis_client=None,
) -> Dict[str, Any]:
    """Enrich multiple leads in a workbook (inline execution).

    DEPRECATED for large runs: fully serial, blocks the caller. Kept for small
    ad-hoc callers. Workbook /run now goes through the durable queue handler
    (handle_run_workbook → run_workbook_enrichment) which is concurrent and
    crash-recoverable. See features/workbook-v2-source-engine-spec.md §1.5 (P-1).
    """
    completed = 0
    errors = 0

    for lead_data in leads:
        for col in columns:
            result = await enrich_cell(
                db=db,
                workbook_id=workbook_id,
                lead_id=lead_data["id"],
                col_id=col["id"],
                col_config=col,
                lead_data=lead_data,
                columns_config=columns_config,
                redis_client=redis_client,
            )
            if result.get("success"):
                completed += 1
            else:
                errors += 1

    return {"completed": completed, "errors": errors, "total": completed + errors}


# ── P-1: Durable, concurrent execution substrate ─────────────────────────
# The workbook /run endpoint enqueues a "run_workbook" job on queue_service
# (DB-polling worker w/ heartbeat + dead-job reaper + retry). The handler
# below runs cells in bounded-concurrency batches so a 500-row × 5-col
# workbook completes off the request thread and never sticks in `running`.

REDIS_URL = os.getenv("REDIS_URL", "redis://localhost:6379")
DEFAULT_CONCURRENCY = int(os.getenv("WORKBOOK_RUN_CONCURRENCY", "8"))

ENRICHMENT_COL_TYPES = ("enrichment", "waterfall", "ai_formula", "agent", "http")


def _make_redis():
    """Best-effort async Redis client for cell-update broadcasts. None if unavailable."""
    try:
        import redis.asyncio as aioredis
        return aioredis.from_url(REDIS_URL, decode_responses=True)
    except Exception as e:  # redis lib missing, etc.
        logger.info(f"Redis unavailable for workbook broadcasts: {e}")
        return None


def _load_workbook_leads(
    db: Session, wb: Workbook,
    row_ids: Optional[list] = None, lead_ids: Optional[list] = None,
) -> list[dict]:
    """Load rows to enrich — v2 WorkbookRow, falling back to v1 leads-DB filter."""
    from sqlalchemy import func as sa_func
    from apps.api.services.leadgen.db import LeadDB

    v2_count = db.query(sa_func.count(WorkbookRow.id)).filter(
        WorkbookRow.workbook_id == wb.id
    ).scalar() or 0

    if v2_count > 0:
        query = db.query(WorkbookRow).filter(WorkbookRow.workbook_id == wb.id)
        if row_ids:
            query = query.filter(WorkbookRow.id.in_(row_ids))
        elif lead_ids:
            query = query.filter(WorkbookRow.lead_id.in_(lead_ids))
        return [{"id": r.lead_id or r.id, **(r.data or {})} for r in query.all()]

    # v1 legacy — leads DB filtered by the workbook's saved criteria
    import dataclasses
    fc = wb.filter_criteria or {}
    lead_db = LeadDB()
    try:
        rows = lead_db.get_leads(
            status=fc.get("status"), city=fc.get("city"),
            source=fc.get("source"), score_tier=fc.get("score_tier"),
            limit=10000,
        )
    except Exception as e:
        logger.warning(f"v1 lead load failed for {wb.id}: {e}")
        rows = []
    finally:
        lead_db.close()

    leads = []
    for r in rows:
        d = dataclasses.asdict(r) if dataclasses.is_dataclass(r) else dict(r)
        leads.append(d)
    if lead_ids:
        leads = [l for l in leads if l.get("id") in lead_ids]
    return leads


async def _run_one_cell(workbook_id, lead_data, col, columns_config, redis_client) -> dict:
    """Run a single cell in its own DB session (Session is not concurrency-safe)."""
    try:
        with SessionLocal() as cell_db:
            return await enrich_cell(
                db=cell_db,
                workbook_id=workbook_id,
                lead_id=lead_data["id"],
                col_id=col["id"],
                col_config=col,
                lead_data=lead_data,
                columns_config=columns_config,
                redis_client=redis_client,
            )
    except Exception as e:
        logger.error(f"Cell {col.get('id')} for lead {lead_data.get('id')} crashed: {e}")
        return {"success": False, "error": str(e)[:200]}


async def run_workbook_enrichment(
    workbook_id: str,
    column_ids: Optional[list] = None,
    row_ids: Optional[list] = None,
    lead_ids: Optional[list] = None,
    concurrency: int = DEFAULT_CONCURRENCY,
) -> Dict[str, Any]:
    """Concurrent, pause-aware, crash-recoverable workbook run.

    Self-contained (own sessions) so it can run under the queue worker. Updates
    workbook status/progress and derives `complete` from cell completion, so the
    workbook can never be left stuck in `running` by a dropped request.
    """
    # ── Load config + rows ──
    with SessionLocal() as db:
        wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
        if not wb:
            return {"error": "workbook_not_found", "total": 0}
        columns_config = wb.columns_config or []
        enrichment_cols = [
            c for c in columns_config
            if c.get("type") in ENRICHMENT_COL_TYPES
            and (column_ids is None or c.get("id") in column_ids)
        ]
        leads = _load_workbook_leads(db, wb, row_ids, lead_ids)

    if not enrichment_cols or not leads:
        with SessionLocal() as db:
            wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
            if wb:
                wb.status = "complete"
                db.commit()
        return {"completed": 0, "errors": 0, "total": 0, "rows": len(leads)}

    cells = [(lead, col) for lead in leads for col in enrichment_cols]
    total = len(cells)
    n_cols = len(enrichment_cols)
    completed = errors = done = 0
    stopped = False
    redis_client = _make_redis()

    try:
        for i in range(0, total, concurrency):
            # Cooperative stop: /stop sets status=paused
            with SessionLocal() as sdb:
                cur = sdb.query(Workbook.status).filter(Workbook.id == workbook_id).scalar()
            if cur == "paused":
                stopped = True
                break

            batch = cells[i:i + concurrency]
            results = await asyncio.gather(
                *[_run_one_cell(workbook_id, lead, col, columns_config, redis_client)
                  for lead, col in batch],
                return_exceptions=True,
            )
            for r in results:
                done += 1
                if isinstance(r, dict) and r.get("success"):
                    completed += 1
                else:
                    errors += 1

            # Progress: completed_rows = fully-processed rows so far
            with SessionLocal() as sdb:
                w = sdb.query(Workbook).filter(Workbook.id == workbook_id).first()
                if w:
                    w.total_rows = len(leads)
                    w.completed_rows = min(len(leads), done // max(1, n_cols))
                    sdb.commit()
    finally:
        # ── Finalize status (never leave it stuck in running) ──
        with SessionLocal() as sdb:
            w = sdb.query(Workbook).filter(Workbook.id == workbook_id).first()
            if w and w.status != "paused":
                w.status = "complete"
                w.completed_rows = len(leads)
                sdb.commit()
        if redis_client is not None:
            try:
                await _broadcast(redis_client, workbook_id, {
                    "type": "workbook_status",
                    "status": "paused" if stopped else "complete",
                    "completed": completed, "errors": errors, "total": total,
                })
            finally:
                try:
                    await redis_client.aclose()
                except Exception:
                    pass

    return {
        "completed": completed, "errors": errors, "total": total,
        "rows": len(leads), "stopped": stopped,
    }


async def handle_run_workbook(job_id: int, payload: dict):
    """queue_service handler for the 'run_workbook' job type."""
    logger.info(f"[job {job_id}] run_workbook {payload.get('workbook_id')}")
    result = await run_workbook_enrichment(
        workbook_id=payload["workbook_id"],
        column_ids=payload.get("column_ids"),
        row_ids=payload.get("row_ids"),
        lead_ids=payload.get("lead_ids"),
        concurrency=payload.get("concurrency", DEFAULT_CONCURRENCY),
    )
    logger.info(f"[job {job_id}] run_workbook done: {result}")


async def _broadcast(redis_client, workbook_id: str, message: dict):
    """Broadcast a message to all WebSocket clients via Redis pub/sub."""
    try:
        await redis_client.publish(
            f"workbook:{workbook_id}",
            json.dumps(message, default=str),
        )
    except Exception as e:
        logger.warning(f"Redis broadcast failed: {e}")
