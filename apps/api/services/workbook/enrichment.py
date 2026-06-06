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

from apps.api.services.workbook.models import Workbook, WorkbookEnrichment, LEAD_FIELD_MAP
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
    "hiring_signals": ["jobspy"],
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

        # Fields that are structured/JSON — NEVER put in a cell, always write-back only
        STRUCTURED_FIELDS = {"decision_makers", "hiring_signals", "secondary_emails", "secondary_phones"}

        for provider_name in provider_chain:
            provider = get_provider(provider_name)
            if not provider:
                logger.warning(f"Provider '{provider_name}' not found, skipping")
                continue

            try:
                # Per-provider timeout so a slow provider (e.g. holehe's 120-site
                # check, deep_scraper's 8-page crawl) can't stall the waterfall.
                result = await asyncio.wait_for(
                    provider.enrich(lead),
                    timeout=float(os.getenv("WORKBOOK_PROVIDER_TIMEOUT", "30")),
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

                if result_value:
                    break  # Waterfall: stop at first success
            except asyncio.TimeoutError:
                logger.warning(f"Provider {provider_name} timed out for lead {lead_id}")
                result_error = "timeout"
            except Exception as e:
                logger.error(f"Provider {provider_name} failed for lead {lead_id}: {e}")
                result_error = str(e)[:200]

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
    """Upsert a WorkbookEnrichment record."""
    existing = db.query(WorkbookEnrichment).filter(
        WorkbookEnrichment.workbook_id == workbook_id,
        WorkbookEnrichment.lead_id == lead_id,
        WorkbookEnrichment.column_id == column_id,
    ).first()

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
    """Enrich multiple leads in a workbook (inline execution)."""
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


async def _broadcast(redis_client, workbook_id: str, message: dict):
    """Broadcast a message to all WebSocket clients via Redis pub/sub."""
    try:
        await redis_client.publish(
            f"workbook:{workbook_id}",
            json.dumps(message, default=str),
        )
    except Exception as e:
        logger.warning(f"Redis broadcast failed: {e}")
