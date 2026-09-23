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
from decimal import Decimal, ROUND_CEILING
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional

from sqlalchemy.orm import Session

from apps.api.database import SessionLocal
from apps.api.services.workbook.models import Workbook, WorkbookEnrichment, WorkbookRow, LEAD_FIELD_MAP
from apps.api.services.workbook.providers import get_provider, list_providers
from apps.api.services.workbook.ai_column import execute_ai_column, cell_text
from apps.api.services.workbook.output import execute_output_column
from apps.api.services.workbook.conditions import evaluate_condition
from apps.api.services.leadgen.enrichment.provider import (
    EnrichmentProvider, EnrichmentResult, WaterfallEnricher,
)
from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.db import LeadDB
from apps.api.services.leadgen.llm import llm  # for the Message Batches pre-pass

logger = logging.getLogger("workbook.enrichment")

# Provider calls run in KILLABLE subprocesses (provider_runner) so a provider
# that blocks past its deadline gets its worker process killed instead of leaking
# an un-killable thread that eventually wedges the run.
from apps.api.services.workbook.provider_runner import run_provider
from apps.api.services.workbook.execution_identity import current_execution
from apps.api.services.workbook.spend_models import WorkbookSpendAttempt
from apps.api.services.workbook.spend_service import execute_reserved_attempt
from apps.api.services.workbook.provider_accounting import accounting_envelope


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
    # Person MOBILE phone (Clay-parity "mobile phone" waterfall): BYOK-only,
    # cost-ordered (leadmagic ~$0.05 < prospeo ~$0.10). Input is the person's
    # LinkedIn URL (and/or work email for leadmagic). Deliberately excludes the
    # company-phone providers above (numverify/google_maps/facebook_pages/
    # local_business find switchboard numbers, not direct dials). Providers are
    # declarative manifests that skip gracefully when their key is missing.
    # numverify is NOT appended as a validation step: the verify-cascade
    # pattern (email_verify_cascade) is email-specific today.
    "mobile_phone": ["leadmagic_mobile", "prospeo_mobile"],
    # Company description / info. mca_registry + wikidata are website-independent:
    # they answer from the company NAME even when the site is dead.
    "description": [
        "deep_scraper", "jsonld_firmographics", "website_scraper", "ddg_company",
        "company_intel", "wikidata", "mca_registry",
    ],
    # Decision makers / contacts (staffspy = full roster; gated, fails over gracefully)
    "decision_makers": ["staffspy", "deep_scraper", "crosslinked", "decision_maker"],
    "contact_person": ["deep_scraper", "crosslinked", "decision_maker", "staffspy", "people_data_labs"],
    "contact_title": ["deep_scraper", "crosslinked", "decision_maker", "staffspy"],
    # Social links — schema.org sameAs is authoritative; wikidata is a keyless,
    # website-independent fallback (official LinkedIn/Twitter/Facebook IDs).
    "linkedin_url": ["jsonld_firmographics", "deep_scraper", "website_scraper", "social_finder", "wikidata"],
    "twitter_url": ["jsonld_firmographics", "deep_scraper", "website_scraper", "social_finder", "wikidata"],
    "facebook_url": ["jsonld_firmographics", "deep_scraper", "website_scraper", "social_finder", "wikidata"],
    # Company metadata
    # company_size_heuristic (keyless, derives from existing signals) is appended
    # LAST below when COMPANY_SIZE_HEURISTIC_ENABLED — it only fills the gap real
    # providers leave (the waterfall stops at the first success).
    "company_size": ["deep_scraper", "website_scraper", "company_intel", "wikidata", "people_data_labs"],
    "industry_tags": ["deep_scraper", "website_scraper", "local_business", "company_intel", "mca_registry", "wikidata"],
    # Address — schema first; mca_registry = authoritative registered office (India).
    "address": ["jsonld_firmographics", "deep_scraper", "local_business", "mca_registry", "wikidata", "google_maps"],
    "founding_year": ["jsonld_firmographics", "deep_scraper", "company_intel", "mca_registry", "wikidata"],
    "founded_year": ["jsonld_firmographics", "deep_scraper", "company_intel", "mca_registry", "wikidata"],
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

# Feature-flagged: append the keyless heuristic as the LAST resort in the
# company_size chain. Off by default → chain is byte-identical to before. Even if
# left appended with the flag off, the provider wouldn't be registered (get_provider
# returns None → skipped), but we gate the append too to avoid log noise.
try:
    from apps.api.core.config import settings as _cfg_settings
    if (getattr(_cfg_settings, "COMPANY_SIZE_HEURISTIC_ENABLED", False)
            and "company_size_heuristic" not in DEFAULT_WATERFALLS["company_size"]):
        DEFAULT_WATERFALLS["company_size"].append("company_size_heuristic")
except Exception:  # pragma: no cover - defensive: never block import on config
    pass


def _lead_dict_to_lead(lead_data: dict) -> Lead:
    """Convert a lead dict from SQLite into a Lead dataclass."""
    return Lead.from_dict(lead_data)


def _get_lead_values(lead_data: dict, columns_config: list) -> Dict[str, str]:
    """Build a flat {column_id: value} dict from lead data for template resolution."""
    values = {}
    aliases = {}
    for col in columns_config:
        col_id = col.get("id", "")
        lead_field = col.get("lead_field", col_id)

        if col.get("type") == "lead_field" and lead_field in lead_data:
            values[col_id] = cell_text(lead_data.get(lead_field))
            values[lead_field] = cell_text(lead_data.get(lead_field))
        else:
            values[col_id] = cell_text(lead_data.get(col_id))
        if col.get("name"):
            aliases[col["name"]] = values[col_id]

    # Current column values win over aliases, which win over stale materialized
    # display-name copies in raw row data. Retain unrelated source fields.
    return {**{key: cell_text(value) for key, value in lead_data.items()}, **aliases, **values}


async def enrich_cell(
    db: Session,
    workbook_id: str,
    lead_id: int,
    col_id: str,
    col_config: dict,
    lead_data: dict,
    columns_config: list,
    redis_client=None,
    force: bool = False,
) -> Dict[str, Any]:
    """Enrich a single cell for a lead in a workbook.

    Routes by column type:
      - enrichment/waterfall → provider chain, writes to Lead if target_field set
      - ai_formula → LLM, stores in WorkbookEnrichment

    ``force=True`` bypasses success-skip gates. For side-effecting ``output``
    columns this explicitly overrides the run-once guard (the row is RE-PUSHED
    to the webhook/CRM/sequencer) — callers must only set it on a deliberate,
    user-confirmed re-run.
    """
    col_type = col_config.get("type", "enrichment")
    # v2 rows may not be backed by a Lead. Keep the legacy integer cell key for
    # WorkbookEnrichment, but never use a WorkbookRow id as a Lead id for
    # write-back. v1 lead dictionaries do not carry __lead_id and remain linked.
    linked_lead_id = (
        lead_data.get("__lead_id")
        if "__lead_id" in lead_data
        else lead_id
    )
    row_id = lead_data.get("__row_id")
    # Provider telemetry is deliberately buffered in memory. Persisting it in
    # the middle of a waterfall used to acquire SQLite's sole writer lock and
    # hold it across later provider network calls, freezing the API and worker
    # heartbeat. It is flushed in one short transaction after the cell commits.
    provider_attempts: list[dict] = []
    budget_charge = 0.0

    from apps.api.services.workbook.column_deps import unavailable_computed_dependencies, cycle_blocked_columns, _refs_in
    dependency_error = ("dependency_cycle" if col_id in cycle_blocked_columns(columns_config) else
                        "upstream_dependency_unavailable" if unavailable_computed_dependencies(col_config, columns_config, lead_data) else None)
    from apps.api.services.workbook.ai_column import validate_template_references
    try:
        validate_template_references(" ".join("{" + ref + "}" for ref in _refs_in(col_config)), columns_config)
    except ValueError:
        dependency_error = "ambiguous_column_reference"
    if dependency_error:
        from apps.api.services.workbook.batch_attempts import fence_workbook_run_state
        fence_workbook_run_state(db, workbook_id)
        _set_enrichment(db, workbook_id, lead_id, col_id, None, "error",
                        error=dependency_error, row_id=row_id)
        db.commit()
        return {"success": False, "value": None, "error": dependency_error}

    # ── Conditional execution ─────────────────────────────────────────
    if col_config.get("condition"):
        # Build cells-like dict for condition evaluation
        cells = {k: {"value": v, "status": "complete"} for k, v in lead_data.items()}
        should_run = evaluate_condition(col_config["condition"], cells, columns_config)
        if not should_run:
            _set_enrichment(db, workbook_id, lead_id, col_id, None, "skipped", row_id=row_id)
            if redis_client:
                await _broadcast(redis_client, workbook_id, {
                    "type": "cell_update", "leadId": lead_id, "rowId": row_id,
                    "colId": col_id,
                    "status": "skipped", "value": None,
                })
            return {"success": False, "value": None, "error": "condition_not_met"}

    # ── Output columns are side-effecting → run-once by default ────────
    # Don't re-push to a webhook/CRM/sequencer on a re-run unless the column
    # explicitly opts out (run_once=False), the cell isn't already complete,
    # or the caller passed an explicit (user-confirmed) force override.
    if col_type == "output" and col_config.get("run_once", True) and not force:
        prior = db.query(WorkbookEnrichment).filter(
            WorkbookEnrichment.workbook_id == workbook_id,
            WorkbookEnrichment.lead_id == lead_id,
            WorkbookEnrichment.column_id == col_id,
        ).first()
        if row_id is not None:
            own_row = db.query(WorkbookRow).filter(
                WorkbookRow.workbook_id == workbook_id, WorkbookRow.id == row_id,
            ).first()
            if own_row is None:
                return {"success": False, "value": None, "error": "output_row_missing"}
            own_cell = (own_row.enrichments or {}).get(col_id)
            if isinstance(own_cell, dict) and own_cell.get("status") == "complete":
                return {"success": True, "value": own_cell.get("value"), "provider": own_cell.get("provider"),
                        "error": None, "skipped": True}
            if own_cell is None and prior and prior.status == "complete":
                # Old lead-keyed receipts cannot prove which v2 row was sent.
                # Do not silently skip this row or risk an automatic resend.
                return {"success": False, "value": None, "error": "output_identity_requires_review"}
        elif prior and prior.status == "complete":
            return {"success": True, "value": prior.value, "provider": prior.provider,
                    "error": None, "skipped": True}

    # Mark as running
    _set_enrichment(db, workbook_id, lead_id, col_id, None, "running", row_id=row_id)
    if redis_client:
        await _broadcast(redis_client, workbook_id, {
            "type": "cell_update", "leadId": lead_id, "rowId": row_id,
            "colId": col_id,
            "status": "running", "value": None,
        })

    # ── Route by column type ──────────────────────────────────────────
    if col_type == "ai_formula":
        # AI Column → LLM
        prompt = col_config.get("prompt", "")
        if not prompt:
            _set_enrichment(db, workbook_id, lead_id, col_id, None, "error", error="no_prompt", row_id=row_id)
            return {"success": False, "value": None, "error": "no_prompt"}

        # Build cells dict for AI template resolution
        cells = {k: {"value": v} for k, v in lead_data.items()}
        # Bound the LLM call — a hung/rate-limited provider must not wedge the row.
        try:
            ai_result = await asyncio.wait_for(
                execute_ai_column(
                    prompt_template=prompt,
                    row_cells=cells,
                    columns_config=columns_config,
                ),
                timeout=float(os.getenv("WORKBOOK_AI_TIMEOUT", "45")),
            )
            result_value = ai_result.get("value")
            result_error = ai_result.get("error")
        except asyncio.TimeoutError:
            result_value = None
            result_error = "ai_timeout"
        result_provider = "ai"

    elif col_type == "output":
        # Output Column → push the row to an external destination.
        # Resolve the workbook's workspace so CRM/SMTP credentials are selected
        # per-workspace (spec WI-6), falling back to global when unset.
        workspace_id = (
            db.query(Workbook.workspace_id)
            .filter(Workbook.id == workbook_id)
            .scalar()
        )
        from apps.api.services.workbook.output_attempts import execute_output_with_journal
        out = await execute_output_with_journal(
            SessionLocal, execute_output_column,
            col_config=col_config,
            lead_data=lead_data,
            columns_config=columns_config,
            workbook_id=workbook_id,
            lead_id=lead_id,
            workspace_id=workspace_id,
        )
        # A diagnostic value such as "POST 500" is not a delivery receipt.
        output_succeeded = ("success" not in out or out["success"] is True) and not bool(out.get("error"))
        result_value = out.get("value") if output_succeeded else None
        result_provider = col_config.get("destination", "output")
        result_error = out.get("error") or (None if output_succeeded else "output_failed")

    elif col_type == "research":
        # Research Column → bounded web-research agent (Claygent-style).
        # Lazy import to avoid a circular import (research_column imports helpers
        # from this module).
        from apps.api.services.workbook.research_column import execute_research_column
        prompt = col_config.get("prompt", "")
        if not prompt:
            _set_enrichment(db, workbook_id, lead_id, col_id, None, "error", error="no_prompt", row_id=row_id)
            return {"success": False, "value": None, "error": "no_prompt"}
        # Resolve the workbook's workspace so the native path is workspace-aware.
        research_ws = (
            db.query(Workbook.workspace_id)
            .filter(Workbook.id == workbook_id)
            .scalar()
        )
        # Per-cell USD budget: column override or the global default.
        research_budget = col_config.get("cell_budget_usd")
        res = await execute_research_column(
            prompt_template=prompt,
            lead_data=lead_data,
            columns_config=columns_config,
            max_steps=col_config.get("max_steps", 4),
            output_format=col_config.get("output_format", "text"),
            workspace_id=research_ws,
            cell_budget_usd=float(research_budget) if research_budget is not None else None,
        )
        result_value = res.get("value")
        result_provider = "research"
        result_error = res.get("error")
        # Native path returns citations/cost/stopped_reason for the cell-metadata
        # channel (same channel verify-status uses); persisted via _set_enrichment.
        _research_metadata = res.get("metadata")

    elif col_type == "agent":
        # Goal-directed enrichment — agent picks tools dynamically (Pillar 4).
        from apps.api.services.workbook.agent_column import run_agent_cell
        agent_result = await run_agent_cell(db, workbook_id, lead_id, col_config, lead_data,
            provider_timeout=_RUN_CONFIG.get("provider_timeout", 10.0))
        provider_attempts.extend(agent_result.pop("_provider_attempts", []))
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

    elif col_type == "formula":
        # Formula action column — safe-evaluated expression over row values.
        from apps.api.services.workbook.formula_column import execute_formula_column
        f_result = await execute_formula_column(col_config, lead_data, columns_config)
        result_value = f_result.get("value")
        result_provider = "formula"
        result_error = f_result.get("error")

    else:
        # Enrichment/Waterfall → provider chain
        lead = _lead_dict_to_lead(lead_data)
        configured_waterfall = col_config.get("waterfall")
        explicit_selection = configured_waterfall is not None or bool(col_config.get("provider"))
        explicit_chain = list(configured_waterfall) if configured_waterfall is not None else ([col_config["provider"]] if col_config.get("provider") else [])

        # The target_field tells us which Lead field this column is enriching (e.g. "email")
        # If set, we ONLY extract that specific field from the provider result.
        target_field = col_config.get("target_field") or col_config.get("lead_field") or col_id

        # ── Resolve provider chain with DEFAULT_WATERFALLS ──
        # Explicit selection is a provider authorization boundary, including its
        # order. Adding defaults here used to call unselected paid providers and
        # made execution disagree with the configured waterfall and cost preview.
        provider_chain = list(explicit_chain if explicit_selection else DEFAULT_WATERFALLS.get(target_field, []))
        authorized_chain = list(provider_chain)
        execution = current_execution.get()
        if execution is not None and execution.workbook_id != workbook_id:
            raise ValueError("Enrichment workbook does not match queue identity")
        row_identity = f"row:{row_id}" if row_id is not None else f"lead:{lead_id}"

        result_value = None
        result_provider = None
        result_error = None
        result_confidence = 0.0
        result_license = None  # provenance: declared license of the winning provider

        # ── Pillar 2: cost-aware ordering + budget ceiling ──
        from apps.api.services.workbook import planner as _planner
        wb_row = db.query(Workbook.budget_max_usd, Workbook.budget_spent_usd).filter(
            Workbook.id == workbook_id
        ).first()
        budget_max = (wb_row[0] or 0.0) if wb_row else 0.0
        budget_spent = (wb_row[1] or 0.0) if wb_row else 0.0
        budget_remaining = (budget_max - budget_spent) if budget_max > 0 else None
        plan_chain = _planner.filter_chain if explicit_selection else _planner.order_chain
        # Selected providers that never ran, as (provider, reason). A cell with
        # no provider call must say why instead of reporting generic no_data.
        skipped_providers: list = []
        provider_chain = plan_chain(db, target_field, provider_chain, budget_remaining,
                                    skipped=skipped_providers)
        chain_exposure = sum(int((Decimal(str(_planner.provider_cost(p))) * 1000000).to_integral_value(rounding=ROUND_CEILING)) for p in dict.fromkeys(authorized_chain))
        if execution is not None:
            prior = [row[0] for row in db.query(WorkbookSpendAttempt.provider).filter_by(
                workspace_id=execution.workspace_id, workbook_id=workbook_id,
                run_id=execution.run_id, row_identity=row_identity, column_id=col_id,
            ).order_by(WorkbookSpendAttempt.created_at, WorkbookSpendAttempt.id).all()]
            provider_chain = list(dict.fromkeys([p for p in prior if p in authorized_chain] + provider_chain))

        # Optional waterfall-depth cap (0 = unlimited). With killable workers a
        # hung provider can't wedge the run, so we default to NO cap — trying the
        # full chain (different sources/strategies) is exactly what lifts the hit
        # rate toward the success target. Set a cap only to trade recall for speed.
        _max_providers = _RUN_CONFIG.get("max_providers", 0)
        if _max_providers and len(provider_chain) > _max_providers:
            provider_chain = provider_chain[:_max_providers]

        # Fields that are structured/JSON — NEVER put in a cell, always write-back only
        STRUCTURED_FIELDS = {"decision_makers", "hiring_signals", "secondary_emails", "secondary_phones", "technographics"}

        import time as _time
        for provider_name in provider_chain:
            from apps.api.services.workbook.batch_attempts import check_workbook_run_owner
            check_workbook_run_owner(SessionLocal, workbook_id)
            provider = get_provider(provider_name)
            if not provider:
                logger.warning(f"Provider '{provider_name}' not found, skipping")
                skipped_providers.append((provider_name, "unknown_provider"))
                continue

            _t0 = _time.monotonic()
            reserved_call = execution is not None and _planner.is_paid(provider_name)
            if execution is not None and not reserved_call:
                from apps.api.services.workbook.vendor_catalog import has_known_cost
                if not has_known_cost(provider_name):
                    result_error = "provider_price_unknown"
                    break
            try:
                # Run the provider in a KILLABLE subprocess (see provider_runner).
                # A provider that blocks past its deadline gets its worker process
                # SIGKILLed and replaced — so a hung provider can never leak a
                # thread and wedge the run. This is what makes the waterfall safe.
                if reserved_call:
                    exposure = int((Decimal(str(_planner.provider_cost(provider_name))) * 1000000).to_integral_value(rounding=ROUND_CEILING))
                    async def operation():
                        response = await run_provider(provider_name, lead, timeout=_RUN_CONFIG.get("provider_timeout", 10.0))
                        return accounting_envelope(provider_name, response, exposure)
                    receipt = await execute_reserved_attempt(operation=operation, reservation=dict(
                        session_factory=SessionLocal, workspace_id=execution.workspace_id, workbook_id=workbook_id,
                        run_id=execution.run_id, row_identity=row_identity, column_id=col_id, provider=provider_name,
                        attempt_key=execution.attempt_key(row_identity=row_identity, column_id=col_id, provider=provider_name),
                        exposure_microusd=exposure, cell_limit_microusd=chain_exposure,
                        cost_basis={"kind": "catalog_estimate", "provider": provider_name},
                        operation_contract={"inputs": lead_data, "column": col_config},
                    ))
                    if not receipt["ok"]:
                        result_error = receipt["reason"]
                        break
                    _rd = receipt["result"]
                else:
                    _rd = await run_provider(provider_name, lead, timeout=_RUN_CONFIG.get("provider_timeout", 10.0))
                # `license` is provenance-only metadata the runner adds; pop it
                # before constructing EnrichmentResult (which has no such field).
                _attempt_license = (_rd or {}).pop("license", None)
                result = (EnrichmentResult(**_rd) if _rd
                          else EnrichmentResult(provider=provider_name, success=False))
                _latency_ms = (_time.monotonic() - _t0) * 1000.0
                provider_attempts.append({
                    "provider": provider_name,
                    "field": target_field,
                    "success": bool(result.success and result.fields),
                    "confidence": result.confidence or provider.default_confidence,
                    "latency_ms": _latency_ms,
                })
                if result.success and result.fields:
                    # ── Write back ALL scalar Lead fields from the result ──
                    for field_name, value in result.fields.items():
                        if not value or value == "" or value == "N/A":
                            continue

                        # Structured fields → write back to Lead only, never to cell
                        if field_name in STRUCTURED_FIELDS:
                            if linked_lead_id is not None:
                                _write_back_to_lead(linked_lead_id, field_name, value, provider_name)
                            continue

                        # Write back any known Lead field
                        if field_name in LEAD_FIELD_MAP and field_name != target_field:
                            if linked_lead_id is not None:
                                _write_back_to_lead(linked_lead_id, field_name, value, provider_name)

                    # ── Extract the TARGETED field for this cell ──
                    if target_field in result.fields:
                        cell_value = result.fields[target_field]
                        if cell_value and cell_value != "" and cell_value != "N/A":
                            # Enforce scalar: reject JSON blobs for cell display
                            sv = str(cell_value)
                            if sv.startswith("[{") or sv.startswith("{\""):
                                # Structured data — write to Lead, show summary in cell
                                if linked_lead_id is not None:
                                    _write_back_to_lead(linked_lead_id, target_field, cell_value, provider_name)
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
                            result_license = _attempt_license

                if result_value:
                    # ── Charge budget for a successful PAID provider ──
                    if _planner.is_paid(provider_name) and not reserved_call:
                        # Defer the write until after all awaited work (including
                        # email verification) so no DB lock spans network I/O.
                        budget_charge = _planner.provider_cost(provider_name)
                    break  # Waterfall: stop at first success
            except asyncio.TimeoutError:
                logger.warning(f"Provider {provider_name} timed out for lead {lead_id}")
                result_error = "timeout"
                # Trip the circuit breaker so the next cell skips this dead/slow
                # provider instead of eating its full timeout again.
                provider_attempts.append({
                    "provider": provider_name,
                    "field": target_field,
                    "success": False,
                    "latency_ms": (_time.monotonic() - _t0) * 1000.0,
                    "timed_out": True,
                })
                if reserved_call:
                    result_error = "accounting_uncertain"
                    break
            except Exception as e:
                _latency_ms = (_time.monotonic() - _t0) * 1000.0
                logger.error(f"Provider {provider_name} failed for lead {lead_id}: {e}")
                result_error = str(e)[:200]
                provider_attempts.append({
                    "provider": provider_name,
                    "field": target_field,
                    "success": False,
                    "latency_ms": _latency_ms,
                    "rate_limited": _planner.looks_rate_limited(result_error),
                })
                if reserved_call:
                    result_error = "accounting_uncertain"
                    break

        if not result_value and not result_error and not provider_attempts:
            if not authorized_chain:
                result_error = "no_providers_selected" if explicit_selection else "no_default_providers"
            elif skipped_providers:
                result_error = ("providers_unavailable: " + ", ".join(
                    f"{p} ({reason})" for p, reason in skipped_providers))[:200]

    # ── Auto-verify email cells ───────────────────────────────────────
    # When an email column produces a value, run the verify cascade and attach
    # the 4-status result as cell metadata (badge in the UI). Best-effort: never
    # fails the cell on a verify error. Opt out with col_config verify=False.
    #
    # The native research path also writes cell_metadata.research (answer,
    # citations, cost_usd, stopped_reason) — persisted on the SAME channel so the
    # UI can render an "n sources" affordance even on a no-answer cell. Persist it
    # even when result_value is empty (so a no_answer cell shows 0 sources).
    cell_metadata = locals().get("_research_metadata") if col_type == "research" else None
    _verify_target = col_config.get("target_field") or col_config.get("lead_field")
    if (result_value and col_type in ("enrichment", "waterfall")
            and _verify_target == "email" and col_config.get("verify", True)):
        try:
            from apps.api.services.leadgen.enrichment.email_verify_cascade import verify_email
            from apps.api.core.tenancy import current_workspace_var
            vr = await verify_email(str(result_value),
                                    workspace_id=current_workspace_var.get() or None)
            cell_metadata = {"verify": {"status": vr.status, "confidence": vr.confidence,
                                        "source": vr.source}}
        except Exception as e:
            logger.debug(f"email verify failed for {result_value}: {e}")

    # Selected providers that never ran (unknown/cooldown/over-budget) are
    # part of this result's attempt history, including when a later provider
    # succeeded. Only the waterfall branch defines the list.
    _skipped = locals().get("skipped_providers")
    if _skipped:
        cell_metadata = {**(cell_metadata or {}), "skipped_providers": [
            {"provider": p, "reason": reason} for p, reason in _skipped]}

    # ── Per-fact provenance (flag-gated) ──────────────────────────────
    # Build {source, license, confidence, fetched_at} for a produced value so it
    # rides into the cell dict + cell_metadata mirror. result_confidence /
    # result_license are only defined on the enrichment/waterfall branch, so read
    # them defensively (None for ai_formula/research/formula/http/agent cells —
    # those record source + a name-resolved license only). When the flag is OFF
    # this stays None and the persisted cell JSON is byte-identical to today.
    provenance = None
    try:
        from apps.api.core.config import settings as _prov_settings
        if (getattr(_prov_settings, "PROVENANCE_TRACKING_ENABLED", False)
                and result_value and result_provider):
            from apps.api.services.leadgen.enrichment.licenses import provenance_for
            _conf = locals().get("result_confidence")
            provenance = provenance_for(
                result_provider,
                confidence=(_conf if _conf else None),
                declared_license=locals().get("result_license"),
            )
    except Exception as e:  # pragma: no cover - provenance must never break a cell
        logger.debug(f"cell provenance build skipped: {e}")
        provenance = None

    # ── Write results ─────────────────────────────────────────────────
    # This is the first write in the cell transaction. Keep it adjacent to the
    # commit: no provider, verifier, or Redis await may happen while SQLite's
    # single writer lock is held.
    from apps.api.services.workbook.batch_attempts import fence_workbook_run_state
    fence_workbook_run_state(db, workbook_id)
    if budget_charge:
        db.query(Workbook).filter(Workbook.id == workbook_id).update(
            {Workbook.budget_spent_usd: (Workbook.budget_spent_usd + budget_charge)},
            synchronize_session=False,
        )
    has_result = result_value is not None and result_value != "" and result_value != [] and result_value != {}
    if has_result:
        # Always store in enrichment overlay (value is already scalar/summary)
        _set_enrichment(db, workbook_id, lead_id, col_id, result_value, "complete",
                        provider=result_provider, metadata=cell_metadata,
                        provenance=provenance, row_id=row_id)
    else:
        # Persist research metadata even on a no-answer cell so the UI can render
        # 0 sources gracefully (cell_metadata is only set for research above).
        _set_enrichment(db, workbook_id, lead_id, col_id, None, "error",
                        error=result_error or "no_data", metadata=cell_metadata, row_id=row_id)

    db.commit()

    # Provider reliability is useful telemetry, not part of the cell's atomic
    # result. Flush it after the result commit in a separate, short transaction
    # so a failed ledger update can never erase a successful enrichment.
    if provider_attempts:
        try:
            from apps.api.services.workbook import planner as _planner
            for attempt in provider_attempts:
                _planner.record_attempt(db, **attempt)
            db.commit()
        except Exception as e:
            db.rollback()
            logger.warning(
                "Provider telemetry write failed for cell %s/%s: %s",
                lead_id, col_id, e,
            )

    # Broadcast result
    if redis_client:
        await _broadcast(redis_client, workbook_id, {
            "type": "cell_update",
            "leadId": lead_id,
            "rowId": row_id,
            "colId": col_id,
            "value": result_value,
            "status": "complete" if has_result else "error",
            "provider": result_provider,
            "error": result_error if not has_result else None,
            "research": (cell_metadata or {}).get("research"),
            "provenance": provenance,
            "verify_status": ((cell_metadata or {}).get("verify") or {}).get("status"),
        })

    return {
        "success": has_result,
        "value": result_value,
        "provider": result_provider,
        "error": result_error,
        "research": (cell_metadata or {}).get("research"),
    }


def _set_enrichment(
    db: Session, workbook_id: str, lead_id: int, column_id: str,
    value: Any, status: str, provider: str = None, error: str = None,
    metadata: dict = None, provenance: dict = None,
    row_id: Optional[int] = None,
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

    # Per-fact provenance rides on the SAME cell_metadata channel as the verify
    # badge (mirror of the inline cell dict). None when the flag is off → the
    # stored metadata is byte-identical to today.
    cell_meta = metadata
    if provenance:
        cell_meta = {**(metadata or {}), "provenance": provenance}

    stored_value = value if value is None or isinstance(value, str) else json.dumps(value, ensure_ascii=False)
    if existing:
        existing.value = stored_value
        existing.status = status
        existing.provider = provider
        existing.error = error
        # Metadata describes this result, not the last successful attempt.
        # Clear it on metadata-free writes (including running/error) so legacy
        # readback cannot resurrect stale citations or verification badges.
        existing.cell_metadata = cell_meta
    else:
        from apps.api.core.tenancy import current_workspace_var
        db.add(WorkbookEnrichment(
            workbook_id=workbook_id,
            # Denormalized tenant from the active workspace scope (set by the
            # request dep / worker workspace_scope). Matches the RLS GUC so the
            # WITH CHECK passes; under SQLite it's the same value, just stored.
            workspace_id=current_workspace_var.get(),
            lead_id=lead_id,
            column_id=column_id,
            value=stored_value,
            status=status,
            provider=provider,
            error=error,
            cell_metadata=cell_meta,
        ))

    # Mirror into the v2 WorkbookRow.enrichments JSON so the value (and verify
    # badge) survive a page reload — the rows endpoint reads that JSON, not the
    # WorkbookEnrichment table. (Without this, enriched cells only showed live
    # via WebSocket and vanished on refresh.)
    try:
        from sqlalchemy.orm.attributes import flag_modified
        if row_id is not None:
            wr = db.query(WorkbookRow).filter(
                WorkbookRow.workbook_id == workbook_id, WorkbookRow.id == row_id
            ).first()
        else:
            wr = db.query(WorkbookRow).filter(
                WorkbookRow.workbook_id == workbook_id, WorkbookRow.lead_id == lead_id
            ).first()
            if wr is None:
                wr = db.query(WorkbookRow).filter(
                    WorkbookRow.workbook_id == workbook_id, WorkbookRow.id == lead_id
                ).first()
        if wr is not None:
            overlay = dict(wr.enrichments or {})
            # ── Automations on_row_changed prior-value capture (§3.6) ──
            # Read the CURRENT cell value before overwrite; if it changes and an
            # enabled on_row_changed rule watches this field, queue a deferred
            # emit (after the caller commits). cell_version derives from the
            # row's updated_at epoch-ms so genuine re-transitions aren't deduped.
            _prev = overlay.get(column_id)
            _old_val = (_prev.get("value") if isinstance(_prev, dict) else _prev)
            cell = {"value": value, "status": status, "provider": provider, "error": error}
            vstatus = (metadata or {}).get("verify", {}).get("status") if metadata else None
            if vstatus:
                cell["verify_status"] = vstatus
            # Per-fact provenance (flag-gated; None → key omitted → byte-identical).
            if provenance:
                cell["provenance"] = provenance
            if isinstance((cell_meta or {}).get("research"), dict):
                cell["research"] = cell_meta["research"]
            if (cell_meta or {}).get("skipped_providers"):
                cell["skipped_providers"] = cell_meta["skipped_providers"]
            overlay[column_id] = cell
            wr.enrichments = overlay
            flag_modified(wr, "enrichments")
            if status == "complete" and str("" if _old_val is None else _old_val) != str("" if value is None else value):
                _queue_row_change_emit(workbook_id, wr.id, column_id, _old_val, value)
    except Exception as e:
        logger.debug(f"row enrichments mirror failed: {e}")
    # Note: caller is responsible for db.commit()


# ── Automations on_row_changed deferred emit (§3.6) ──────────────────────────
# We accumulate (workbook_id, row_id, field, old, new) deltas during a write
# batch and flush them to events.on_rows_changed AFTER the run commits, so a
# rolled-back write never fires a rule. Keyed per-asyncio-task to stay isolated.
import contextvars as _contextvars  # noqa: E402
import time as time  # noqa: E402  (used by flush_row_change_emits cell_version)

_pending_row_changes: _contextvars.ContextVar = _contextvars.ContextVar(
    "_pending_row_changes", default=None
)


def _queue_row_change_emit(workbook_id, row_id, field, old, new):
    from apps.api.core.config import settings as _settings
    if not getattr(_settings, "AUTOMATIONS_ENABLED", False):
        return
    buf = _pending_row_changes.get()
    if buf is None:
        buf = []
        _pending_row_changes.set(buf)
    buf.append({"workbook_id": workbook_id, "row_id": str(row_id), "field": field,
                "old": old, "new": new})


def flush_row_change_emits(workspace_id: str):
    """Emit accumulated on_row_changed deltas (call AFTER the write commits)."""
    buf = _pending_row_changes.get()
    if not buf:
        return
    _pending_row_changes.set([])
    if not workspace_id:
        return
    by_wb: dict = {}
    for ch in buf:
        by_wb.setdefault(ch["workbook_id"], []).append({
            "row_id": ch["row_id"], "field": ch["field"],
            "old": ch["old"], "new": ch["new"],
            "cell_version": int(time.time() * 1000),
        })
    try:
        from apps.api.services.automations import events as _auto_events
        for wb_id, changes in by_wb.items():
            _auto_events.on_rows_changed(workspace_id, wb_id, changes)
    except Exception as e:
        logger.warning("on_rows_changed flush failed: %s", e)


def _write_back_to_lead(lead_id: int, field: str, value: str, provider: str = None):
    """Write an enrichment result back to the Lead record (source of truth).

    Uses the tenant-scoped lead store resolved from the active
    ``workspace_scope`` (set by ``run_workbook_enrichment``), so the write lands
    in the correct per-workspace store and passes RLS ``WITH CHECK`` on Postgres
    — never a bare default-path LeadDB. Falls back to the legacy default path only
    for an unscoped caller (preserves prior behaviour).
    """
    try:
        from apps.api.core.tenancy import current_workspace_var

        updates = {field: value}
        if provider and field == "email":
            updates["email_provider"] = provider
        elif provider and field == "phone":
            updates["phone_provider"] = provider

        # Per-fact provenance (flag-gated): record {source,license,confidence,
        # fetched_at} for this written-back field + refresh last_enriched_at.
        # Built once here; merged read-modify-write into field_provenance below
        # per backend so we never clobber other fields' provenance.
        _prov = None
        try:
            from apps.api.core.config import settings as _prov_settings
            if getattr(_prov_settings, "PROVENANCE_TRACKING_ENABLED", False):
                from apps.api.services.leadgen.enrichment.licenses import provenance_for
                _prov = provenance_for(provider)
        except Exception as e:  # pragma: no cover
            logger.debug(f"write-back provenance build skipped: {e}")
            _prov = None

        ws = current_workspace_var.get()
        if ws:
            from apps.api.services.leadgen.store import get_lead_store
            from apps.api.services.workspace import manager as ws_manager

            slug = ws_manager.workspace_slug(ws) or ""
            store = get_lead_store(ws, slug)
            try:
                if _prov is not None:
                    from apps.api.services.leadgen.enrichment.licenses import (
                        merge_field_provenance,
                    )
                    cur = store.get_lead(lead_id)
                    cur_fp = getattr(cur, "field_provenance", "") if cur else ""
                    updates["field_provenance"] = merge_field_provenance(
                        cur_fp or "", {field: _prov},
                    )
                    updates["last_enriched_at"] = datetime.now(timezone.utc).isoformat()
                store.update_lead_fields(lead_id, updates)  # adds updated_at
            finally:
                store.close()
        else:
            # Unscoped legacy caller — preserve prior default-path behaviour.
            lead_db = LeadDB()
            try:
                if _prov is not None:
                    from apps.api.services.leadgen.enrichment.licenses import (
                        merge_field_provenance,
                    )
                    try:
                        row = lead_db.conn.execute(
                            "SELECT field_provenance FROM leads WHERE id = ?", (lead_id,)
                        ).fetchone()
                        cur_fp = row[0] if row else ""
                    except Exception:
                        cur_fp = ""
                    updates["field_provenance"] = merge_field_provenance(
                        cur_fp or "", {field: _prov},
                    )
                    updates["last_enriched_at"] = datetime.now(timezone.utc).isoformat()
                updates["updated_at"] = datetime.now(timezone.utc).isoformat()
                set_clause = ", ".join(f"{k} = ?" for k in updates)
                lead_db.conn.execute(
                    f"UPDATE leads SET {set_clause} WHERE id = ?",
                    list(updates.values()) + [lead_id],
                )
                lead_db.conn.commit()
            finally:
                lead_db.close()
        logger.info(f"Wrote {field}={str(value)[:50]} back to lead {lead_id}")
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
    crash-recoverable. See docs/specs/workbook-v2-source-engine-spec.md §1.5 (P-1).
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
DEFAULT_CONCURRENCY = int(os.getenv("WORKBOOK_RUN_CONCURRENCY", "12"))

# Per-run knobs read by enrich_cell (set by run_workbook_enrichment at run start;
# runs are sequential so a module-level dict is safe). max_providers: 0 = no cap.
_RUN_CONFIG = {
    "max_providers": int(os.getenv("WORKBOOK_MAX_PROVIDERS_PER_CELL", "0")),
    "provider_timeout": float(os.getenv("WORKBOOK_PROVIDER_TIMEOUT", "10")),
}

ENRICHMENT_COL_TYPES = (
    "enrichment", "waterfall", "ai_formula", "research", "agent", "http",
    "formula", "output",
)


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

    v2_count = db.query(sa_func.count(WorkbookRow.id)).filter(
        WorkbookRow.workbook_id == wb.id
    ).scalar() or 0

    if v2_count > 0:
        query = db.query(WorkbookRow).filter(WorkbookRow.workbook_id == wb.id)
        if row_ids is not None:
            query = query.filter(WorkbookRow.id.in_(row_ids))
        elif lead_ids is not None:
            query = query.filter(WorkbookRow.lead_id.in_(lead_ids))
        from apps.api.services.workbook.cell_scope import row_execution_data
        return [row_execution_data(row, wb.columns_config or []) for row in query.all()]

    from apps.api.services.workbook.cell_scope import uses_legacy_leads
    if row_ids is not None or lead_ids == [] or not uses_legacy_leads(wb):
        # Selected v2 rows may have disappeared since enqueue. Never widen the
        # run by switching identity domains or treating an empty list as all.
        return []

    # v1 legacy — leads DB. The /run endpoint already resolved the workbook's
    # filter into an explicit lead_ids list and passes it in, so prefer fetching
    # exactly those by id. (get_leads only honors status/city/source/score_tier,
    # so re-deriving from filter_criteria here would drop fields like
    # "specialization" and enrich the whole table — see workbooks.run_workbook.)
    import dataclasses
    from apps.api.core.tenancy import current_workspace_var
    from apps.api.services.leadgen.store import get_lead_store
    from apps.api.services.workspace import manager as ws_manager

    fc = wb.filter_criteria or {}
    # Scoped lead store (not a bare default-path LeadDB) so the v1 read is correct
    # under per-workspace SQLite and RLS-protected Postgres. The run is already
    # inside workspace_scope(workspace_id); use that active tenant (== wb's).
    _ws = current_workspace_var.get() or wb.workspace_id or ""
    _slug = ws_manager.workspace_slug(_ws) or "" if _ws else ""
    lead_db = get_lead_store(_ws, _slug)
    try:
        if lead_ids:
            rows = [lead_db.get_lead(i) for i in lead_ids]
            rows = [r for r in rows if r is not None]
        else:
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
    return leads


def _cell_subject(lead):
    return ("row", lead["__row_id"]) if lead.get("__row_id") is not None else ("lead", lead["id"])


def _stored_cells(db, workbook_id, leads, status):
    """Read v2 cell status by row identity, with a separate legacy path."""
    found = set()
    row_ids = [lead["__row_id"] for lead in leads if lead.get("__row_id") is not None]
    if row_ids:
        for row in db.query(WorkbookRow).filter(WorkbookRow.workbook_id == workbook_id, WorkbookRow.id.in_(row_ids)).all():
            for cid, cell in (row.enrichments or {}).items():
                if isinstance(cell, dict) and cell.get("status") == status and (status != "complete" or cell.get("value") is not None):
                    found.add((("row", row.id), cid))
    lead_ids = [lead["id"] for lead in leads if lead.get("__row_id") is None]
    if lead_ids:
        query = db.query(WorkbookEnrichment).filter(
            WorkbookEnrichment.workbook_id == workbook_id,
            WorkbookEnrichment.lead_id.in_(lead_ids), WorkbookEnrichment.status == status,
        )
        if status == "complete":
            query = query.filter(WorkbookEnrichment.value.isnot(None))
        found.update((("lead", cell.lead_id), cell.column_id) for cell in query.all())
    return found


async def _run_one_cell(workbook_id, lead_data, col, columns_config, redis_client,
                        force: bool = False) -> dict:
    """Run a single cell in its own DB session (Session is not concurrency-safe)."""
    try:
        from apps.api.services.workbook.batch_attempts import batch_owner, fence_workbook_run_state
        if batch_owner.get() is not None:
            # Short transaction only: release ownership-check locks before any
            # provider/output awaits. The result transaction checks again.
            with SessionLocal() as ownership_db:
                fence_workbook_run_state(ownership_db, workbook_id)
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
                force=force,
            )
    except Exception as e:
        logger.error(f"Cell {col.get('id')} for lead {lead_data.get('id')} crashed: {e}")
        # The failed cell transaction rolled back, which may otherwise expose
        # an old complete result to retry/dependency selection.
        try:
            from apps.api.services.workbook.batch_attempts import fence_workbook_run_state
            with SessionLocal() as failure_db:
                fence_workbook_run_state(failure_db, workbook_id)
                _set_enrichment(failure_db, workbook_id, lead_data["id"], col["id"], None, "error",
                                error="cell_execution_failed", row_id=lead_data.get("__row_id"))
                failure_db.commit()
        except Exception:
            logger.warning("Could not persist cell failure; ownership or storage may have changed")
        return {"success": False, "error": str(e)[:200]}


async def _run_one_row(workbook_id, lead, ordered_cols, columns_config, redis_client,
                       should_stop=None, force: bool = False) -> dict:
    """Run all columns for ONE row, sequentially in dependency order.

    Each successful cell value is threaded back into a local copy of the row so
    that a downstream column referencing {this_column} sees the produced value.
    Returns {completed, errors}. Rows are run concurrently by the caller.

    should_stop() is checked before each cell so a /stop (or a cancelled job)
    halts the run within a couple seconds instead of only at batch boundaries.
    """
    row = dict(lead)  # local, mutable: downstream cols read earlier results
    identities = {key: lead[key] for key in ("id", "__row_id", "__lead_id") if key in lead}
    completed = errors = 0
    failed_columns = []
    from apps.api.services.workbook.column_deps import referencing_columns
    for col in ordered_cols:
        if should_stop is not None and should_stop():
            break
        if any(referencing_columns([col], failed) for failed in failed_columns):
            # Never execute a dependent against the old hydrated result after
            # its upstream attempt failed. Persist the reason for reload/retry.
            from apps.api.services.workbook.batch_attempts import fence_workbook_run_state
            with SessionLocal() as db:
                fence_workbook_run_state(db, workbook_id)
                _set_enrichment(db, workbook_id, row["id"], col["id"], None, "error",
                                error="upstream_dependency_failed", row_id=row.get("__row_id"))
                db.commit()
            res = {"success": False, "error": "upstream_dependency_failed"}
        else:
            res = await _run_one_cell(workbook_id, row, col, columns_config, redis_client,
                                      force=force)
        if isinstance(res, dict) and res.get("success"):
            completed += 1
            val = res.get("value")
            if val is not None:
                # expose under both id and display name for {ref} resolution
                row[col["id"]] = val
                if col.get("name"):
                    row[col["name"]] = val
                row.update(identities)
        else:
            errors += 1
            failed_columns.append(col)
            for key in (col.get("id"), col.get("name")):
                if key not in identities:
                    row.pop(key, None)
    return {"completed": completed, "errors": errors}


# ── Message Batches pre-pass (Anthropic bulk AI-column cost lever) ──────
# When Anthropic is the serving provider and a workbook run has independent
# ai_formula columns over many rows, submitting those per-row prompts as ONE
# Anthropic Message Batch runs at ~50% cost (async). This is opt-in/graceful:
# only independent ai_formula columns are eligible (no dependency threading to
# break), small runs and non-Anthropic providers keep the synchronous path, and
# any custom_id the batch doesn't return falls through to the normal per-row run.

BATCH_ENABLED = str(os.getenv("WORKBOOK_BATCH_ENABLED", "1")).strip().lower() not in ("0", "false", "no", "off")
BATCH_MIN_ROWS = int(os.getenv("WORKBOOK_BATCH_MIN_ROWS", "20"))


def _batch_eligible_columns(ordered_cols: list) -> list:
    """The ai_formula columns safe to batch: independent + text output.

    JSON-output AI columns go through llm.extract_json (parse + retry on the
    sync path) — we keep those synchronous rather than reimplement that loop in
    the batch path. Independence (no {ref} edges) guarantees batching them out of
    band can't starve a downstream column of its input.
    """
    from apps.api.services.workbook.column_deps import independent_columns
    indep_ids = {c["id"] for c in independent_columns(ordered_cols)}
    return [
        c for c in ordered_cols
        if c.get("type") == "ai_formula"
        and c["id"] in indep_ids
        and (c.get("output_format") or "text") != "json"
        and (c.get("prompt") or "").strip()
    ]


async def _run_ai_batch_prepass(
    workbook_id: str,
    work_items: list,
    columns_config: list,
    redis_client,
    should_stop=None,
) -> tuple[set, int]:
    """Submit eligible ai_formula cells as one Anthropic batch; write results.

    Mutates nothing; returns ({(lead_id, col_id) handled}, completed_count). The
    caller removes the handled pairs from work_items so the sync loop runs only
    the remainder (non-AI cols, JSON AI cols, and any cell the batch didn't
    return). Returns an empty set when batching isn't applicable (non-Anthropic,
    disabled, too few rows, no eligible columns) so the caller no-ops cleanly.
    """
    from apps.api.services.workbook.batch_attempts import (
        batch_owner, current_batch_attempt, claim_batch_attempt,
        acknowledge_batch_attempt, fence_batch_result, checkpoint_batch_results, BatchRecoveryRequired,
    )
    owner = batch_owner.get()
    prior_attempt = None
    if owner is not None:
        try:
            if owner["workbook_id"] != workbook_id:
                raise ValueError("Workbook changed")
            prior_attempt = current_batch_attempt(SessionLocal, **owner)
        except Exception as error:
            raise BatchRecoveryRequired("Cannot verify prior batch ownership; no fallback permitted") from error
    if not BATCH_ENABLED:
        if prior_attempt is not None:
            raise BatchRecoveryRequired("Batching disabled with an existing attempt; reconcile before retrying")
        return set(), 0

    prov = llm.anthropic_provider()
    if not prov:
        if prior_attempt is not None:
            raise BatchRecoveryRequired("Batch provider changed with an existing attempt; reconcile before retrying")
        return set(), 0  # Anthropic isn't the serving provider → sync path

    # Gather the (lead, col) cells eligible for batching across all rows.
    from apps.api.services.workbook.ai_column import build_ai_prompt, AI_COLUMN_SYSTEM

    requests: list[dict] = []
    index: dict[str, tuple] = {}  # custom_id → (cell_subject_id, col_id, row_id)
    # Selection is not a dependency graph: omitted upstream/downstream columns
    # must not turn a coupled column into an apparently independent one.
    eligible_ids = {column["id"] for column in _batch_eligible_columns([
        column for column in columns_config if column.get("type") in ENRICHMENT_COL_TYPES
    ])}
    for lead, cols in work_items:
        eligible = [column for column in cols if column.get("id") in eligible_ids]
        for col in eligible:
            cells = {k: {"value": v} for k, v in lead.items()}
            prompt = build_ai_prompt(col.get("prompt", ""), cells, columns_config)
            subject = f"row_{lead['__row_id']}" if lead.get("__row_id") is not None else str(lead["id"])
            cid = f"{subject}::{col['id']}"
            index[cid] = (lead["id"], col["id"], lead.get("__row_id"))
            requests.append({
                "custom_id": cid,
                "prompt": prompt,
                "system": AI_COLUMN_SYSTEM,
                "max_tokens": int(col.get("max_tokens", 1500)),
            })

    if len(requests) < BATCH_MIN_ROWS:
        if prior_attempt is not None:
            raise BatchRecoveryRequired("Batch eligibility changed with an existing attempt; reconcile before retrying")
        return set(), 0  # too small to be worth the async round-trip

    logger.info(f"[batch] submitting {len(requests)} ai_formula cells to Anthropic Message Batch")
    batch_options = {}
    cached_results = None
    if current_execution.get() is not None:
        if owner is None or owner["workbook_id"] != workbook_id:
            raise BatchRecoveryRequired("Queued batch requires an active scoped lease")
        try:
            claim = claim_batch_attempt(SessionLocal, contract={"requests": requests, "provider": prov}, **owner)
        except Exception as error:
            raise BatchRecoveryRequired("Batch claim unavailable; no synchronous fallback permitted") from error
        if claim["action"] == "reconcile":
            raise BatchRecoveryRequired("Batch submission outcome unknown; reconcile before retrying")
        cached_results = claim.get("results")
        def checkpoint_result(custom_id, text):
            if custom_id not in index:
                raise BatchRecoveryRequired("Batch returned an unexpected request identity")
            checkpoint_batch_results(SessionLocal, contract_hash=claim["contract_hash"],
                                     results={custom_id: text}, **owner)
        batch_options = {
            "batch_id": claim["vendor_batch_id"], "strict_results": True,
            "on_result": checkpoint_result,
            "known_result_ids": list(cached_results or {}),
            "on_submitted": lambda vendor_id: acknowledge_batch_attempt(
                SessionLocal, contract_hash=claim["contract_hash"], vendor_batch_id=vendor_id, **owner),
        }
    try:
        cache_complete = cached_results is not None and set(cached_results) == set(index)
        results = cached_results if cache_complete else await llm.batch_complete_anthropic(
            requests, prov=prov, should_stop=should_stop, **batch_options)
        if cached_results is not None and not cache_complete:
            results = {**cached_results, **results}
    except Exception as e:
        if batch_options:
            raise BatchRecoveryRequired("Batch results unavailable; retrieve or reconcile the existing attempt") from e
        logger.warning(f"[batch] submission failed, falling back to sync per-row: {e}")
        return set(), 0
    if batch_options and set(results) != set(index):
        raise BatchRecoveryRequired("Batch has unresolved cells; automatic synchronous fallback is disabled")
    if batch_options and not cache_complete:
        try:
            checkpoint_batch_results(SessionLocal, contract_hash=claim["contract_hash"], results=results, **owner)
        except Exception as error:
            raise BatchRecoveryRequired("Batch results could not be checkpointed; retry retrieval, not submission") from error

    handled: set = set()
    completed = 0
    # Persist each returned cell exactly like enrich_cell's ai path (provider="ai").
    for cid, text in results.items():
        if cid not in index:
            continue
        lead_id, col_id, row_id = index[cid]
        with SessionLocal() as cdb:
            if batch_options:
                try:
                    fence_batch_result(cdb, contract_hash=claim["contract_hash"], **owner)
                except Exception as error:
                    raise BatchRecoveryRequired("Batch result writer lost ownership; current cell was not saved") from error
            _set_enrichment(cdb, workbook_id, lead_id, col_id, text, "complete", provider="ai", row_id=row_id)
            cdb.commit()
        handled.add((f"row_{row_id}" if row_id is not None else lead_id, col_id))
        completed += 1
        if redis_client is not None:
            try:
                await _broadcast(redis_client, workbook_id, {
                    "type": "cell_update", "leadId": lead_id, "rowId": row_id,
                    "colId": col_id,
                    "value": text, "status": "complete", "provider": "ai", "error": None,
                })
            except Exception:
                pass

    logger.info(f"[batch] {completed}/{len(requests)} cells filled via batch; "
                f"{len(requests) - completed} fall through to sync")
    return handled, completed


def _apply_batch_handled(work_items: list, handled: set) -> list:
    """Drop batch-handled (lead, col) pairs from work_items; prune empty rows."""
    if not handled:
        return work_items
    out = []
    for lead, cols in work_items:
        subject = f"row_{lead['__row_id']}" if lead.get("__row_id") is not None else lead["id"]
        remaining = [c for c in cols if (subject, c["id"]) not in handled]
        if remaining:
            out.append((lead, remaining))
    return out


async def run_workbook_enrichment(
    workbook_id: str,
    column_ids: Optional[list] = None,
    row_ids: Optional[list] = None,
    lead_ids: Optional[list] = None,
    concurrency: int = DEFAULT_CONCURRENCY,
    job_id: Optional[int] = None,
    max_providers: int = 0,
    retry_passes: int = 1,
    provider_timeout: float = 10.0,
    fill_missing: bool = False,
    force: bool = False,
    workspace_id: Optional[str] = None,
    row_columns: Optional[dict] = None,
) -> Dict[str, Any]:
    """Run a workbook enrichment under its tenant scope.

    ``workspace_id`` is the tenant this run belongs to; it arrives out-of-band
    (job payload / caller), never read back off the workbook row. We enter
    ``workspace_scope`` FIRST (fail-loud on empty) so every session opened by the
    run — including the concurrent per-cell sessions and the lead-store read-backs
    — is bound to the correct tenant for RLS. Safe to nest when a caller (worker
    handler / refresh / automation) has already scoped to the same workspace.
    """
    from apps.api.core.tenancy import workspace_scope

    from apps.api.services.workbook.execution_identity import execution_scope

    with workspace_scope(workspace_id), execution_scope(workspace_id, workbook_id, job_id):
        return await _run_workbook_enrichment_impl(
            workbook_id=workbook_id,
            column_ids=column_ids,
            row_ids=row_ids,
            lead_ids=lead_ids,
            concurrency=concurrency,
            job_id=job_id,
            max_providers=max_providers,
            retry_passes=retry_passes,
            provider_timeout=provider_timeout,
            fill_missing=fill_missing,
            force=force,
            row_columns=row_columns,
        )


async def _run_workbook_enrichment_impl(
    workbook_id: str,
    column_ids: Optional[list] = None,
    row_ids: Optional[list] = None,
    lead_ids: Optional[list] = None,
    concurrency: int = DEFAULT_CONCURRENCY,
    job_id: Optional[int] = None,
    max_providers: int = 0,
    retry_passes: int = 1,
    provider_timeout: float = 10.0,
    fill_missing: bool = False,
    force: bool = False,
    row_columns: Optional[dict] = None,
) -> Dict[str, Any]:
    """Concurrent, pause-aware, crash-recoverable workbook run.

    Self-contained (own sessions) so it can run under the queue worker. Updates
    workbook status/progress and derives `complete` from cell completion, so the
    workbook can never be left stuck in `running` by a dropped request.

    Honors two stop signals (checked before every cell, throttled to one DB read
    every ~2s): the workbook being set to `paused` by /stop, and this run's Job
    being cancelled (e.g. superseded by a newer run). Either halts promptly so a
    Stop click and a re-run don't leave a zombie run grinding in the background.
    """
    # ── Load config + rows ──
    from apps.api.services.workbook.batch_attempts import fence_workbook_run_state
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
        from apps.api.services.workbook.batch_attempts import batch_owner, current_batch_attempt, BatchRecoveryRequired
        owner = batch_owner.get()
        if owner is not None and current_batch_attempt(SessionLocal, **owner) is not None:
            raise BatchRecoveryRequired("Workbook scope is empty with an existing batch; reconcile before completing")
        with SessionLocal() as db:
            fence_workbook_run_state(db, workbook_id)
            wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
            if wb:
                wb.status = "complete"
                db.commit()
        return {"completed": 0, "errors": 0, "total": 0, "rows": len(leads)}

    # Only the active queue lease may assert running state.
    with SessionLocal() as sdb:
        fence_workbook_run_state(sdb, workbook_id)
        w0 = sdb.query(Workbook).filter(Workbook.id == workbook_id).first()
        if w0 and w0.status != "paused":
            w0.status = "running"
            sdb.commit()

    # Order columns so a column referencing {another} runs AFTER it; then run
    # row-major (columns sequential per row, threading results) with rows
    # concurrent. This makes derived columns (formula/http/ai) see their inputs.
    from apps.api.services.workbook.column_deps import topo_sort_columns
    ordered_cols = topo_sort_columns(enrichment_cols)
    _RUN_CONFIG["max_providers"] = max_providers      # read by enrich_cell
    _RUN_CONFIG["provider_timeout"] = provider_timeout

    # Build the work list: (lead, columns_to_run). In fill-missing mode only the
    # cells that aren't already complete are run — gaps get filled, good values
    # are preserved (a re-run won't clobber a complete cell with a fresh miss).
    if fill_missing:
        with SessionLocal() as sdb:
            done_set = _stored_cells(sdb, workbook_id, leads, "complete")
        work_items = [
            (lead, cols)
            for lead in leads
            for cols in [[c for c in ordered_cols if (_cell_subject(lead), c["id"]) not in done_set]]
            if cols
        ]
        logger.info(f"fill_missing: {sum(len(c) for _, c in work_items)} gaps across "
                    f"{len(work_items)} rows (of {len(leads)})")
    else:
        work_items = [(lead, ordered_cols) for lead in leads]

    from apps.api.services.workbook.cell_scope import restrict_work_items
    work_items = restrict_work_items(work_items, row_columns)
    selected_cells = {(_cell_subject(lead), column["id"]) for lead, cols in work_items for column in cols}

    total = sum(len(cols) for _, cols in work_items)
    completed = errors = rows_done = 0
    stopped = False
    failed = False
    redis_client = _make_redis()

    # Cooperative-stop probe. Checked before every cell and every batch, but the
    # underlying DB read is throttled to once per ~2s so an 11-column run on one
    # row doesn't spam queries. Latches once true so we stop everywhere at once.
    import time as _time
    _stop_state = {"stopped": False, "checked_at": 0.0}

    def _should_stop() -> bool:
        if _stop_state["stopped"]:
            return True
        now = _time.monotonic()
        if now - _stop_state["checked_at"] < 2.0:
            return False
        _stop_state["checked_at"] = now
        with SessionLocal() as sdb:
            wstatus = sdb.query(Workbook.status).filter(Workbook.id == workbook_id).scalar()
            jstatus = None
            if job_id is not None:
                from apps.api.models import Job
                jstatus = sdb.query(Job.status).filter(Job.id == job_id).scalar()
        if wstatus == "paused" or jstatus in ("cancelled", "failed"):
            _stop_state["stopped"] = True
        return _stop_state["stopped"]

    # ── Cost lever: batch eligible AI cells before the sync per-row loop. ──
    # Independent text ai_formula columns over many rows go to the Anthropic
    # Message Batch API (~50% cost). Handled cells are removed from work_items so
    # the sync loop runs only the remainder; non-Anthropic/small runs no-op here.
    try:
        if not _should_stop():
            try:
                handled, batch_completed = await _run_ai_batch_prepass(
                    workbook_id, work_items, columns_config, redis_client,
                    should_stop=_should_stop,
                )
                if handled:
                    work_items = _apply_batch_handled(work_items, handled)
                    completed += batch_completed
                    total = sum(len(cols) for _, cols in work_items) + batch_completed
            except Exception as e:
                from apps.api.services.workbook.batch_attempts import BatchRecoveryRequired, batch_owner, current_batch_attempt
                if isinstance(e, BatchRecoveryRequired):
                    raise
                owner = batch_owner.get()
                if owner is not None and current_batch_attempt(SessionLocal, **owner) is not None:
                    raise BatchRecoveryRequired("Existing batch failed recovery; synchronous fallback is disabled") from e
                logger.warning(f"[batch] pre-pass errored, continuing with sync run: {e}")

        for i in range(0, len(work_items), concurrency):
            if _should_stop():
                stopped = True
                break

            batch = work_items[i:i + concurrency]
            results = await asyncio.gather(
                *[_run_one_row(workbook_id, lead, cols, columns_config,
                               redis_client, should_stop=_should_stop, force=force)
                  for lead, cols in batch],
                return_exceptions=True,
            )
            for (_, attempted_columns), r in zip(batch, results):
                rows_done += 1
                if isinstance(r, dict):
                    completed += r.get("completed", 0)
                    errors += r.get("errors", 0)
                else:
                    errors += len(attempted_columns)

            # Progress: completed_rows = rows fully processed so far
            with SessionLocal() as sdb:
                fence_workbook_run_state(sdb, workbook_id)
                w = sdb.query(Workbook).filter(Workbook.id == workbook_id).first()
                if w:
                    w.total_rows = len(leads)
                    w.completed_rows = min(len(leads), rows_done)
                    sdb.commit()

        # ── Retry passes: re-run only the cells still in `error`. Transient
        # failures (LLM rate-limit, momentary network) recover, and the full
        # waterfall gets another shot at the hard cells. This is what pushes the
        # fill rate up toward the success target. Bounded by retry_passes. ──
        lead_by_id = {_cell_subject(lead): lead for lead in leads}
        from apps.api.services.workbook.cell_scope import allows_automatic_retry
        retryable_ids = {column["id"] for column in ordered_cols if allows_automatic_retry(column)}
        for _pass in range(max(0, retry_passes)):
            if stopped or _should_stop():
                break
            with SessionLocal() as sdb:
                err_cells = _stored_cells(sdb, workbook_id, leads, "error")
            err_by_lead: dict[tuple, set] = {}
            for lid, cid in err_cells:
                if lid in lead_by_id and (lid, cid) in selected_cells and cid in retryable_ids:
                    err_by_lead.setdefault(lid, set()).add(cid)
            if not err_by_lead:
                break
            targets = [
                (lead_by_id[lid], [c for c in ordered_cols if c["id"] in cols])
                for lid, cols in sorted(err_by_lead.items())
            ]
            targets = restrict_work_items(targets, row_columns)
            if not targets:
                break
            n_cells = sum(len(cols) for _, cols in targets)
            logger.info(f"[retry pass {_pass + 1}/{retry_passes}] re-running {n_cells} error cells")
            for i in range(0, len(targets), concurrency):
                if _should_stop():
                    stopped = True
                    break
                chunk = targets[i:i + concurrency]
                fixed = await asyncio.gather(
                    *[_run_one_row(workbook_id, lead, cols, columns_config,
                                   redis_client, should_stop=_should_stop)
                      for lead, cols in chunk],
                    return_exceptions=True,
                )
                for r in fixed:
                    if isinstance(r, dict):
                        completed += r.get("completed", 0)
                        errors -= r.get("completed", 0)  # moved error → complete
    except asyncio.CancelledError:
        stopped = True
        raise
    except Exception:
        failed = True
        raise
    finally:
        # ── Finalize status (never leave it stuck in running) ──
        _ws_for_emit = None
        final_status = "failed" if failed else "paused" if stopped else "failed" if errors > 0 else "complete"
        owns_state = True
        with SessionLocal() as sdb:
            try:
                fence_workbook_run_state(sdb, workbook_id)
            except ValueError:
                owns_state = False
            w = sdb.query(Workbook).filter(Workbook.id == workbook_id).first() if owns_state else None
            if w:
                _ws_for_emit = w.workspace_id
                if w.status != "paused":
                    w.status = final_status
                    w.completed_rows = min(len(leads), rows_done) if stopped or failed else len(leads)
                    sdb.commit()
                else:
                    final_status = "paused"
        # Automations on_row_changed: flush prior-value-captured deltas AFTER the
        # write committed (a rolled-back write never fires a rule). No-op when off.
        try:
            flush_row_change_emits(_ws_for_emit)
        except Exception as e:
            logger.debug("row-change flush skipped: %s", e)
        if redis_client is not None:
            try:
                if owns_state:
                    await _broadcast(redis_client, workbook_id, {
                        "type": "workbook_status",
                        "status": final_status,
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
    """queue_service handler for the 'run_workbook' job type.

    Worker tenant signal comes from the payload (OD-4): enter ``workspace_scope``
    first and fail loud if ``workspace_id`` is absent — never run a workbook
    enrichment unscoped/global.
    """
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.workbook.batch_attempts import batch_lease_scope

    logger.info(f"[job {job_id}] run_workbook {payload.get('workbook_id')}")
    workspace_id = payload.get("workspace_id")
    with workspace_scope(workspace_id), batch_lease_scope(job_id, payload):
        # Size the killable worker pool to the configured count before running.
        try:
            from apps.api.services.workbook import provider_runner
            provider_runner.ensure_workers(payload.get("provider_workers"))
        except Exception as e:
            logger.warning(f"provider pool sizing skipped: {e}")
        result = await run_workbook_enrichment(
            workbook_id=payload["workbook_id"],
            column_ids=payload.get("column_ids"),
            row_ids=payload.get("row_ids"),
            lead_ids=payload.get("lead_ids"),
            concurrency=payload.get("concurrency", DEFAULT_CONCURRENCY),
            job_id=job_id,
            max_providers=payload.get("max_providers", 0),
            retry_passes=payload.get("retry_passes", 1),
            provider_timeout=float(payload.get("provider_timeout", 10)),
            fill_missing=bool(payload.get("fill_missing", False)),
            force=bool(payload.get("force", False)),
            workspace_id=workspace_id,
            row_columns=payload.get("row_columns"),
        )
        logger.info(f"[job {job_id}] run_workbook done: {result}")
        # Receipt persistence must not turn already-executed external actions
        # into an automatic retry if the diagnostic write itself fails.
        try:
            from apps.api.services.workbook.run_receipts import persist_run_result
            persist_run_result(job_id, payload, result)
        except Exception:
            logger.exception("Could not persist workbook result for job %s", job_id)


async def _broadcast(redis_client, workbook_id: str, message: dict):
    """Broadcast a message to all WebSocket clients via Redis pub/sub."""
    try:
        await redis_client.publish(
            f"workbook:{workbook_id}",
            json.dumps(message, default=str),
        )
    except Exception as e:
        logger.warning(f"Redis broadcast failed: {e}")
