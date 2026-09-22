"""
Agentic enrichment column (Pillar 4).

Where a `waterfall` column runs a fixed provider order, an `agent` column is
goal-directed: it picks the next tool dynamically (cost-aware), observes each
result, self-heals on rate-limits (reroutes instead of failing), respects a
per-cell step/cost budget, and emits a reasoning trace the user can audit.

Tools are the same registry providers — the agent only *chooses* the order. A
`browser_use` tool is attempted last as a fallback for JS/SPA/form-gated sites
(used only if such a provider is registered; otherwise skipped gracefully).
"""

import logging
import time
import asyncio
from decimal import Decimal, ROUND_CEILING, ROUND_FLOOR
from apps.api.database import SessionLocal
from apps.api.services.workbook.execution_identity import current_execution
from apps.api.services.workbook.spend_service import execute_reserved_attempt
from apps.api.services.workbook.provider_accounting import accounting_envelope
from apps.api.services.workbook.spend_models import WorkbookSpendAttempt
from typing import Dict, List

from sqlalchemy.orm import Session

from apps.api.services.leadgen.models import Lead
from apps.api.services.workbook.providers import get_provider, EnrichmentResult
from apps.api.services.workbook.provider_runner import run_provider
from apps.api.services.workbook.models import Workbook
from apps.api.services.workbook import planner as _planner
from apps.api.services.workbook.trace_models import CellTrace

logger = logging.getLogger("workbook.agent")

DEFAULT_MAX_STEPS = 6
DEFAULT_MAX_COST = 0.10


def _infer_target(col_config: dict) -> str:
    return col_config.get("target_field") or col_config.get("lead_field") or "email"


def _save_trace(db, workbook_id, lead_id, col_id, goal, steps, outcome, spent):
    existing = db.query(CellTrace).filter(
        CellTrace.workbook_id == workbook_id,
        CellTrace.lead_id == lead_id,
        CellTrace.column_id == col_id,
    ).first()
    if existing:
        existing.goal = goal; existing.steps = steps
        existing.outcome = outcome; existing.total_cost_usd = {"spent": round(spent, 4)}
    else:
        # Denormalized tenant from the active workspace scope (agent runs inside
        # workspace_scope) so the trace row matches the RLS GUC / WITH CHECK.
        from apps.api.core.tenancy import current_workspace_var
        db.add(CellTrace(
            workbook_id=workbook_id, workspace_id=current_workspace_var.get(),
            lead_id=lead_id, column_id=col_id,
            goal=goal, steps=steps, outcome=outcome,
            total_cost_usd={"spent": round(spent, 4)},
        ))


async def run_agent_cell(
    db: Session, workbook_id: str, lead_id: int, col_config: dict, lead_data: dict,
    *, provider_timeout: float = 10.0,
) -> Dict:
    """Goal-directed enrichment for one cell. Returns {value, provider, error, trace}."""
    goal = col_config.get("goal") or f"Find {_infer_target(col_config)}"
    target = _infer_target(col_config)
    policy = col_config.get("policy") or {}
    max_steps = int(policy.get("max_steps", DEFAULT_MAX_STEPS))
    max_cost = float(policy.get("max_cost_usd", DEFAULT_MAX_COST))

    # Candidate tools: explicit list, else the field's default chain.
    from apps.api.services.workbook.enrichment import DEFAULT_WATERFALLS
    configured_tools = col_config.get("tools")
    # Explicitly choosing no providers must not expand into the default chain.
    tools: List[str] = list(configured_tools if configured_tools is not None else DEFAULT_WATERFALLS.get(target, []))
    # Workbook budget headroom (agent never exceeds the smaller of cell/workbook caps).
    wb_row = db.query(Workbook.budget_max_usd, Workbook.budget_spent_usd).filter(
        Workbook.id == workbook_id
    ).first()
    wb_remaining = None
    if wb_row and (wb_row[0] or 0) > 0:
        wb_remaining = (wb_row[0] or 0) - (wb_row[1] or 0)

    lead = Lead.from_dict(lead_data)
    steps = []
    spent = 0.0
    value = None
    provider_used = None
    outcome = "exhausted"
    provider_attempts = []
    identity = current_execution.get()
    if identity is not None and identity.workbook_id != workbook_id:
        raise ValueError("Agent execution workbook does not match queue identity")
    reservation_accounted = False
    row_identity = f"row:{lead_data['__row_id']}" if lead_data.get("__row_id") is not None else f"lead:{lead_id}"
    prior_providers = []
    if identity is not None:
        # Persisted attempts must be visited before planning fresh spend. A saved
        # result can be replayed even when its original charge exhausted the cap.
        prior_providers = [row[0] for row in db.query(WorkbookSpendAttempt.provider).filter(
            WorkbookSpendAttempt.workspace_id == identity.workspace_id,
            WorkbookSpendAttempt.workbook_id == workbook_id,
            WorkbookSpendAttempt.run_id == identity.run_id,
            WorkbookSpendAttempt.row_identity == row_identity,
            WorkbookSpendAttempt.column_id == col_config["id"],
        ).order_by(WorkbookSpendAttempt.created_at, WorkbookSpendAttempt.id).all()]

    for step_i in range(1, max_steps + 1):
        cell_budget = max_cost - spent
        if wb_remaining is not None:
            cell_budget = min(cell_budget, wb_remaining - spent)
        # Re-plan each step: cooldowns/affordability can change as we go.
        ordered = _planner.order_chain(db, target, tools, budget_remaining=cell_budget)
        ordered = list(dict.fromkeys([p for p in prior_providers if p in tools] + ordered))
        # Drop tools already tried.
        tried = {s["provider"] for s in steps}
        ordered = [t for t in ordered if t not in tried]
        if not ordered:
            # Positive headroom can still be insufficient for the next lookup.
            # Do not report an already-tried provider as a budget blocker.
            budget_blocked = any(
                t not in tried and _planner.is_paid(t) and _planner.provider_cost(t) > cell_budget
                for t in tools
            )
            outcome = "budget" if budget_blocked else "exhausted"
            break

        provider_name = ordered[0]
        from apps.api.services.workbook.batch_attempts import check_workbook_run_owner
        check_workbook_run_owner(SessionLocal, workbook_id)
        provider = get_provider(provider_name)
        if not provider:
            steps.append({"step": step_i, "provider": provider_name, "success": False,
                          "reason": "provider_not_registered"})
            tools = [t for t in tools if t != provider_name]
            continue

        t0 = time.monotonic()
        reserved_call = identity is not None and _planner.is_paid(provider_name)
        if identity is not None and not reserved_call:
            from apps.api.services.workbook.vendor_catalog import has_known_cost
            if not has_known_cost(provider_name):
                outcome = "provider_price_unknown"
                steps.append({"step": step_i, "provider": provider_name, "success": False,
                              "reason": outcome})
                break
        try:
            if reserved_call:
                exposure = int((Decimal(str(_planner.provider_cost(provider_name))) * 1000000).to_integral_value(rounding=ROUND_CEILING))
                async def operation():
                    response = await run_provider(provider_name, lead, timeout=provider_timeout)
                    return accounting_envelope(provider_name, response, exposure)
                receipt = await execute_reserved_attempt(operation=operation, reservation=dict(
                    session_factory=SessionLocal, workspace_id=identity.workspace_id, workbook_id=workbook_id,
                    run_id=identity.run_id, row_identity=row_identity, column_id=col_config["id"], provider=provider_name,
                    attempt_key=identity.attempt_key(row_identity=row_identity, column_id=col_config["id"], provider=provider_name),
                    exposure_microusd=exposure,
                    cell_limit_microusd=int((Decimal(str(max_cost)) * 1000000).to_integral_value(rounding=ROUND_FLOOR)),
                    cost_basis={"kind": "catalog_estimate", "provider": provider_name},
                    operation_contract={"inputs": lead_data, "column": col_config},
                ))
                if not receipt["ok"]:
                    outcome = receipt["reason"]
                    steps.append({"step": step_i, "provider": provider_name, "success": False, "reason": outcome})
                    break
                raw = receipt["result"]
                reservation_accounted = True
                spent += receipt["charged_microusd"] / 1000000
            else:
                raw = await run_provider(provider_name, lead, timeout=provider_timeout)
            result = EnrichmentResult(**{key: value for key, value in raw.items() if key != "license"}) if raw else EnrichmentResult(provider=provider_name, success=False)
            latency = (time.monotonic() - t0) * 1000
            got = result.success and result.fields and result.fields.get(target)
            provider_attempts.append({
                "provider": provider_name,
                "field": target,
                "success": bool(got),
                "confidence": result.confidence or provider.default_confidence,
                "latency_ms": latency,
            })
            if got:
                value = str(result.fields[target])
                provider_used = provider_name
                if _planner.is_paid(provider_name) and not reserved_call:
                    spent += _planner.provider_cost(provider_name)
                steps.append({"step": step_i, "provider": provider_name, "success": True,
                              "value": value[:80], "cost": _planner.provider_cost(provider_name),
                              "reason": "goal_met"})
                outcome = "found"
                break
            steps.append({"step": step_i, "provider": provider_name, "success": False,
                          "reason": "no_value"})
        except Exception as e:
            latency = (time.monotonic() - t0) * 1000
            err = str(e)[:160]
            rl = _planner.looks_rate_limited(err)
            timed_out = isinstance(e, asyncio.TimeoutError)
            provider_attempts.append({
                "provider": provider_name,
                "field": target,
                "success": False,
                "latency_ms": latency,
                "rate_limited": rl,
                "timed_out": timed_out,
            })
            steps.append({"step": step_i, "provider": provider_name, "success": False,
                          "reason": ("timeout" if timed_out else "rate_limited→reroute" if rl else f"error: {err}")})
            if reserved_call:
                outcome = "accounting_uncertain"
                break

    # Charge workbook budget for any paid success.
    if provider_used and _planner.is_paid(provider_used) and not reservation_accounted:
        db.query(Workbook).filter(Workbook.id == workbook_id).update(
            {Workbook.budget_spent_usd: (Workbook.budget_spent_usd + _planner.provider_cost(provider_used))},
            synchronize_session=False,
        )

    _save_trace(db, workbook_id, lead_id, col_config.get("id"), goal, steps, outcome, spent)

    return {
        "value": value,
        "provider": provider_used,
        "error": None if value else f"agent_{outcome}",
        "trace": {"goal": goal, "steps": steps, "outcome": outcome, "spent": round(spent, 4)},
        # The cell runner persists telemetry only after committing the cell, so
        # SQLite never holds its writer lock across a provider network call.
        "_provider_attempts": provider_attempts,
    }
