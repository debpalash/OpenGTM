"""
Autopilot — goal → plan → execute agent.

Turns a high-level chat goal ("build me a list of 50 IT staffing firms in Pune
and find their founders' emails") into an ordered PLAN, then executes it by
ORCHESTRATING the existing chat tools (create_source_workbook, add_agent_column,
set_workbook_refresh). It contains NO scraping/enrichment code of its own — the
heavy work runs through the existing live-sourcing + agent-column + queue
infrastructure.

Two-phase, mapped onto the chat's human-in-the-loop gate:
  1. draft_plan(goal)  — SAFE/read-only: returns a plan for the user to see.
  2. execute_plan(plan) — DANGEROUS/gated: runs only after the user approves.

execute_plan receives the chat's `_execute_tool` as a callback so it can reuse
the exact tool implementations without a circular import.
"""
import json
import logging
from typing import Awaitable, Callable, List, Dict, Optional

from apps.api.services.leadgen.llm import llm, _read_setting
from apps.api.services.workbook.prompt_guard import (
    guard_untrusted,
    untrusted_data_system_prompt,
)

logger = logging.getLogger("autopilot")


# ── Caps / config (env-overridable; defaults per spec §10) ────────────


def _cap_int(key: str, default: int) -> int:
    try:
        return int(str(_read_setting(key, str(default))).strip())
    except Exception:
        return default


def AUTOPILOT_MAX_ROWS() -> int:
    return max(1, _cap_int("AUTOPILOT_MAX_ROWS", 500))


def AUTOPILOT_MAX_AGENT_COLUMNS() -> int:
    return max(1, _cap_int("AUTOPILOT_MAX_AGENT_COLUMNS", 8))


def AUTOPILOT_MAX_CELLS() -> int:
    return max(1, _cap_int("AUTOPILOT_MAX_CELLS", 4000))


def _native_enabled() -> bool:
    """LLM planner is ON iff AUTOPILOT_LLM_PLANNER is set AND Anthropic is the
    default-selected provider (so we have native structured output)."""
    val = str(_read_setting("AUTOPILOT_LLM_PLANNER", "0")).strip().lower()
    if val in ("0", "false", "off", "no", ""):
        return False
    return llm.anthropic_provider() is not None


# Allowlist of plan step kinds, each with its allowed params. `execute_plan`
# only actually orchestrates a subset (create_source_workbook / add_agent_column
# / set_workbook_refresh); `add_research_column` and `report` validate but the
# executor treats unsupported kinds as no-ops (forward-compatible). A kind/param
# outside this allowlist FAILS validation → heuristic fallback.
AUTOPILOT_TOOL_CATALOG: Dict[str, set] = {
    "create_source_workbook": {"icp_description", "target_rows", "auto_run", "auto_enrich"},
    "add_agent_column": {"column_name", "goal", "target_field"},
    "add_research_column": {"column_name", "prompt", "max_steps"},
    "set_workbook_refresh": {"signals", "enabled"},
    "report": set(),
}

# Structured-output schema for the LLM plan.
_PLAN_SCHEMA: Dict = {
    "type": "object",
    "properties": {
        "goal": {"type": "string"},
        "estimated_rows": {"type": "integer"},
        "steps": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "kind": {"type": "string",
                             "enum": list(AUTOPILOT_TOOL_CATALOG.keys())},
                    "description": {"type": "string"},
                    "params": {"type": "object", "additionalProperties": True},
                },
                "required": ["kind", "description", "params"],
                "additionalProperties": False,
            },
        },
    },
    "required": ["goal", "estimated_rows", "steps"],
    "additionalProperties": False,
}

_PLANNER_SYSTEM = """You are the Autopilot PLANNER for a B2B lead-research tool.
Turn the user's GOAL into an ordered PLAN of steps drawn ONLY from this catalog:
  - create_source_workbook {icp_description, target_rows, auto_run, auto_enrich}
      Create a live-sourcing workbook for an ideal-customer-profile description.
  - add_agent_column {column_name, goal, target_field}
      A goal-directed per-row enrichment column (e.g. find founder email).
  - add_research_column {column_name, prompt, max_steps}
      A per-row web-research column answering a question with citations.
  - set_workbook_refresh {signals, enabled}
      Auto-refresh the workbook on signals.
  - report {}
      Summarize the results.
Rules:
- Use ONLY the kinds and params above. Do NOT invent kinds or params.
- A typical plan creates a workbook, adds enrichment/research columns, then reports.
- It is fine to add columns to an existing workbook (no leading create needed).
- Keep it minimal — one column per distinct data field the goal asks for.
- estimated_rows is your best estimate of the row count."""


def _planner_system() -> str:
    notice = untrusted_data_system_prompt()
    return _PLANNER_SYSTEM + ("\n\n" + notice if notice else "")


# Map keywords in the goal → an agent column spec (goal-directed per-row agent).
# Order matters: more specific patterns first; we dedupe by target field so
# "founders' emails" yields one Founder-Email column, not Founder + Email.
_FIELD_PATTERNS = [
    (("founder", "ceo", "owner", "co-founder", "director", "promoter"),
     {"label": "Founder Email", "field": "email",
      "goal": "Find the verified email address of the founder, CEO, or owner."}),
    (("email", "e-mail"),
     {"label": "Email", "field": "email",
      "goal": "Find the company's primary contact email address."}),
    (("phone", "contact number", "mobile"),
     {"label": "Phone", "field": "phone",
      "goal": "Find the company's primary phone number."}),
    (("linkedin",),
     {"label": "LinkedIn", "field": "linkedin_url",
      "goal": "Find the company's official LinkedIn page URL."}),
]


def _detect_fields(goal: str) -> List[Dict]:
    """Which enrichment columns does the goal ask for?"""
    g = goal.lower()
    out, seen = [], set()
    for keys, spec in _FIELD_PATTERNS:
        if any(k in g for k in keys) and spec["field"] not in seen:
            out.append(spec)
            seen.add(spec["field"])
    return out


def _heuristic_plan(goal: str, target_count: int = 0) -> Dict:
    """Deterministically turn a goal into an ordered plan of existing primitives.

    Heuristic (no LLM): create a live-sourcing workbook for the ICP described in
    the goal, add a goal-directed agent column per requested data field, then
    report. Returns a plan dict that execute_plan() can run.
    """
    g = (goal or "").strip()
    fields = _detect_fields(g)
    steps: List[Dict] = [{
        "kind": "create_source_workbook",
        "description": f"Create a live-sourcing workbook for: {g}"
                       + (f" (target {target_count} rows)" if target_count else ""),
        # auto_enrich chains run_workbook after sourcing so the agent columns
        # below actually execute (only when there are enrichment fields to fill).
        "params": {"icp_description": g, "target_rows": int(target_count or 0),
                   "auto_run": True, "auto_enrich": bool(fields)},
    }]
    for f in fields:
        steps.append({
            "kind": "add_agent_column",
            "description": f"Add an agent column \"{f['label']}\" — {f['goal']}",
            "params": {"column_name": f["label"], "goal": f["goal"], "target_field": f["field"]},
        })
    steps.append({
        "kind": "report",
        "description": "Summarize the results and link the workbook",
        "params": {},
    })
    return {"goal": g, "estimated_rows": int(target_count or 0) or None, "steps": steps}


# ── LLM planner (native, structured + validated) ──────────────────────


def _validate_plan(plan: Dict) -> Optional[Dict]:
    """Validate + clamp an LLM-emitted plan to the allowlist. None = invalid.

    Rules (spec §5 / edge cases 20-22):
      - Plan must be a non-empty list of steps; every step validates.
      - kind must be in the allowlist; each param must be in that kind's
        allowlist (reject only params not allowed — don't blanket-reject).
      - Clamp estimated_rows / target_rows to AUTOPILOT_MAX_ROWS.
      - Clamp the count of agent/research columns to AUTOPILOT_MAX_AGENT_COLUMNS
        and the rows × columns product to AUTOPILOT_MAX_CELLS.
      - Do NOT require a leading create_source_workbook or trailing report
        (accept add-column-to-existing-workbook plans).
    """
    if not isinstance(plan, dict):
        return None
    steps_in = plan.get("steps")
    if not isinstance(steps_in, list) or not steps_in:
        return None

    max_rows = AUTOPILOT_MAX_ROWS()
    max_cols = AUTOPILOT_MAX_AGENT_COLUMNS()
    max_cells = AUTOPILOT_MAX_CELLS()

    # Clamp estimated rows first; it bounds the rows×cols product.
    try:
        est = int(plan.get("estimated_rows") or 0)
    except (TypeError, ValueError):
        est = 0
    est = max(0, min(est, max_rows))

    out_steps: List[Dict] = []
    column_count = 0
    for step in steps_in:
        if not isinstance(step, dict):
            return None
        kind = step.get("kind")
        if kind not in AUTOPILOT_TOOL_CATALOG:
            logger.info(f"autopilot_plan_validation_failures kind={kind!r}")
            return None
        allowed = AUTOPILOT_TOOL_CATALOG[kind]
        params = step.get("params") or {}
        if not isinstance(params, dict):
            return None
        clean_params: Dict = {}
        for k, v in params.items():
            if k not in allowed:
                logger.info(f"autopilot_plan_validation_failures param={k!r} kind={kind}")
                return None
            clean_params[k] = v

        if kind == "create_source_workbook" and "target_rows" in clean_params:
            try:
                tr = int(clean_params["target_rows"] or 0)
            except (TypeError, ValueError):
                tr = 0
            clean_params["target_rows"] = max(0, min(tr, max_rows))

        if kind in ("add_agent_column", "add_research_column"):
            column_count += 1
            if column_count > max_cols:
                # Clamp: drop excess columns rather than reject the whole plan.
                logger.info("autopilot column count clamped")
                continue

        out_steps.append({
            "kind": kind,
            "description": str(step.get("description", "") or kind),
            "params": clean_params,
        })

    if not out_steps:
        return None

    # Clamp rows × columns product. If the product exceeds AUTOPILOT_MAX_CELLS,
    # reduce estimated_rows (and any create target_rows) so the run fits.
    effective_rows = est or 0
    if column_count > 0 and effective_rows > 0:
        cols = min(column_count, max_cols)
        if effective_rows * cols > max_cells:
            effective_rows = max(1, max_cells // cols)
            for s in out_steps:
                if s["kind"] == "create_source_workbook" and "target_rows" in s["params"]:
                    s["params"]["target_rows"] = min(
                        s["params"]["target_rows"] or effective_rows, effective_rows
                    )

    return {
        "goal": str(plan.get("goal", "") or ""),
        "estimated_rows": effective_rows or None,
        "steps": out_steps,
    }


async def _draft_plan_llm(goal: str, target_count: int, workspace_id: Optional[str]) -> Optional[Dict]:
    """Draft a plan via native structured output; None on any failure.

    The planner consumes ONLY the trusted goal + static catalog + the workspace's
    own prior plans. Prior-goal text is treated as UNTRUSTED and wrapped with
    guard_untrusted so a poisoned prior goal can't bias drafting.
    """
    from apps.api.services.agent import autopilot_memory

    mem = autopilot_memory.recent(workspace_id, limit=3)
    mem_lines = []
    for m in mem:
        fenced = guard_untrusted(str(m.get("goal", "")), label="prior goal")
        mem_lines.append(f"- {fenced} → outcome={m.get('outcome')}")
    mem_block = ("\n\nRecent autopilot runs in this workspace (for context only):\n"
                 + "\n".join(mem_lines)) if mem_lines else ""

    user = (
        f"GOAL: {goal}\n"
        + (f"Target row count: {int(target_count)}\n" if target_count else "")
        + mem_block
        + "\n\nProduce the plan as JSON {goal, estimated_rows, steps:[{kind, description, params}]}."
    )
    try:
        resp = await llm.anthropic_tool_call(
            [{"role": "user", "content": user}],
            system=_planner_system(),
            output_format={"type": "json_schema", "schema": _PLAN_SCHEMA},
            max_tokens=1024,
        )
    except Exception as e:
        logger.info(f"autopilot planner LLM call failed: {e}")
        return None
    text = "".join(
        getattr(b, "text", "")
        for b in (getattr(resp, "content", None) or [])
        if getattr(b, "type", "") == "text"
    ).strip()
    try:
        raw = json.loads(text) if text else None
    except Exception:
        raw = None
    if not isinstance(raw, dict):
        return None
    return _validate_plan(raw)


async def draft_plan(goal: str, target_count: int = 0, workspace_id: Optional[str] = None) -> Dict:
    """Draft a plan: native LLM planner (flag on) → validated; else heuristic.

    Always returns a plan dict in the exact shape execute_plan consumes. Never
    raises / never blocks the chat turn — any failure falls back to the heuristic.
    """
    g = (goal or "").strip()
    if _native_enabled():
        try:
            plan = await _draft_plan_llm(g, int(target_count or 0), workspace_id)
            if plan and plan.get("steps"):
                logger.info("autopilot_llm_plans")
                return plan
            logger.info("autopilot_heuristic_fallbacks reason=invalid_or_empty")
        except Exception as e:
            logger.info(f"autopilot_heuristic_fallbacks reason=exception: {e}")
    return _heuristic_plan(g, int(target_count or 0))


def describe_plan(plan: Dict) -> str:
    """Human-readable plan for the confirmation gate (one step per line)."""
    goal = plan.get("goal", "")
    lines = [f"🤖 Autopilot — {goal}"]
    for i, step in enumerate(plan.get("steps", []), 1):
        lines.append(f"{i}. {step.get('description', step.get('kind'))}")
    rows = plan.get("estimated_rows")
    if rows:
        lines.append(f"Target: ~{rows} companies")
    return "\n".join(lines)


async def execute_plan(
    plan: Dict,
    execute_tool: Callable[[str, Dict], Awaitable[str]],
) -> Dict:
    """Execute an (approved) plan by orchestrating the existing chat tools.

    `execute_tool` is the chat's _execute_tool — passed in to avoid a circular
    import and to reuse the exact tool logic (workbook creation, agent columns,
    refresh), including their queue jobs and progress events.

    TENANCY: the caller binds `execute_tool` to the resolved workspace via
    ``functools.partial(_execute_tool, store=..., workspace_id=..., slug=...)``
    (copilotkit.py execute_plan branch), so every step here inherits the same
    tenant-scoped store + RLS scope — the planner cannot reach another workspace.
    """
    wb_id: Optional[str] = None
    steps_done: List[Dict] = []

    for step in plan.get("steps", []):
        kind = step.get("kind")
        params = dict(step.get("params") or {})

        if kind == "create_source_workbook":
            res = json.loads(await execute_tool("create_source_workbook", params))
            wb_id = res.get("workbook_id")
            steps_done.append({"kind": kind, "workbook_id": wb_id, "ok": bool(wb_id)})

        elif kind == "add_agent_column":
            if not wb_id:
                steps_done.append({"kind": kind, "ok": False, "error": "no workbook"})
                continue
            params["workbook_id"] = wb_id
            res = json.loads(await execute_tool("add_agent_column", params))
            steps_done.append({"kind": kind, "column": params.get("column_name"),
                               "ok": "error" not in res})

        elif kind == "set_workbook_refresh":
            if not wb_id:
                continue
            params["workbook_id"] = wb_id
            await execute_tool("set_workbook_refresh", params)
            steps_done.append({"kind": kind, "ok": True})

        # "report" and unknown kinds are no-ops here (summarized below).

    msg = (f"Autopilot built workbook {wb_id}. Sourcing and enrichment are "
           f"running in the background — open /workbooks/{wb_id} to watch rows "
           f"and agent columns fill in.") if wb_id else \
          "Autopilot could not create the workbook."
    # NOTE: no `job_id` here — wb_id is a workbook id, not a leadgen job id, so
    # it must not trigger the chat's TaskDetailCard (which polls /api/jobs/{id}).
    return {"ok": bool(wb_id), "workbook_id": wb_id,
            "goal": plan.get("goal", ""), "steps_done": steps_done, "message": msg}
