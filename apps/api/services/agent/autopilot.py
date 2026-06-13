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
from typing import Awaitable, Callable, List, Dict, Optional


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


def draft_plan(goal: str, target_count: int = 0) -> Dict:
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
