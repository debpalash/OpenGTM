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
from typing import Dict, List

from sqlalchemy.orm import Session

from apps.api.services.leadgen.models import Lead
from apps.api.services.workbook.providers import get_provider
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
        db.add(CellTrace(
            workbook_id=workbook_id, lead_id=lead_id, column_id=col_id,
            goal=goal, steps=steps, outcome=outcome,
            total_cost_usd={"spent": round(spent, 4)},
        ))


async def run_agent_cell(
    db: Session, workbook_id: str, lead_id: int, col_config: dict, lead_data: dict,
) -> Dict:
    """Goal-directed enrichment for one cell. Returns {value, provider, error, trace}."""
    goal = col_config.get("goal") or f"Find {_infer_target(col_config)}"
    target = _infer_target(col_config)
    policy = col_config.get("policy") or {}
    max_steps = int(policy.get("max_steps", DEFAULT_MAX_STEPS))
    max_cost = float(policy.get("max_cost_usd", DEFAULT_MAX_COST))

    # Candidate tools: explicit list, else the field's default chain.
    from apps.api.services.workbook.enrichment import DEFAULT_WATERFALLS
    tools: List[str] = col_config.get("tools") or DEFAULT_WATERFALLS.get(target, [])
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

    for step_i in range(1, max_steps + 1):
        cell_budget = max_cost - spent
        if wb_remaining is not None:
            cell_budget = min(cell_budget, wb_remaining - spent)
        # Re-plan each step: cooldowns/affordability can change as we go.
        ordered = _planner.order_chain(db, target, tools, budget_remaining=cell_budget)
        # Drop tools already tried.
        tried = {s["provider"] for s in steps}
        ordered = [t for t in ordered if t not in tried]
        if not ordered:
            outcome = "budget" if (cell_budget <= 0 and any(_planner.is_paid(t) for t in tools)) else "exhausted"
            break

        provider_name = ordered[0]
        provider = get_provider(provider_name)
        if not provider:
            steps.append({"step": step_i, "provider": provider_name, "success": False,
                          "reason": "provider_not_registered"})
            tools = [t for t in tools if t != provider_name]
            continue

        t0 = time.monotonic()
        try:
            result = await provider.enrich(lead)
            latency = (time.monotonic() - t0) * 1000
            got = result.success and result.fields and result.fields.get(target)
            _planner.record_attempt(db, provider_name, target, success=bool(got),
                                    confidence=(result.confidence or provider.default_confidence),
                                    latency_ms=latency)
            if got:
                value = str(result.fields[target])
                provider_used = provider_name
                if _planner.is_paid(provider_name):
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
            _planner.record_attempt(db, provider_name, target, success=False,
                                    latency_ms=latency, rate_limited=rl)
            steps.append({"step": step_i, "provider": provider_name, "success": False,
                          "reason": ("rate_limited→reroute" if rl else f"error: {err}")})

    # Charge workbook budget for any paid success.
    if provider_used and _planner.is_paid(provider_used):
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
    }
