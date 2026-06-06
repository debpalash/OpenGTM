"""
Research column ("Claygent") — a bounded web-research agent per row.

Unlike an AI column (which reasons over existing row data), a research column
*browses the web* to answer an arbitrary question about the row, e.g.
"Does {company} use Kubernetes? cite a source".

It runs a small ReAct loop driven by llm.extract_json (the LLM client has no
native tool-use): each step the model picks search / fetch / answer. Tools reuse
existing infra — _ddg_search for search, UniversalScraper for fetch (behind the
SSRF guard) — and the loop is hard-bounded by max_steps so cost/latency stay
predictable.
"""

import logging
from typing import Any, Dict, List

from apps.api.services.leadgen.llm import llm
from apps.api.services.leadgen.job_runner import _ddg_search
from apps.api.services.workbook.ai_column import _resolve_prompt
from apps.api.services.workbook.enrichment import _get_lead_values
from apps.api.services.workbook.output import _is_safe_public_url

logger = logging.getLogger("workbook.research")

MAX_STEPS_CAP = 6          # hard ceiling regardless of column config
SEARCH_RESULTS = 5
FETCH_CHARS = 1500         # cap page text fed back to the model

_REACT_SYSTEM = """You are a web-research agent answering ONE question about a company/lead.
You work in steps. At each step reply with a SINGLE JSON object choosing ONE action:
  {"action":"search","query":"<concise web search query>"}   - search the web
  {"action":"fetch","url":"<a url from a previous search result>"}  - read a page
  {"action":"answer","final":"<concise answer, cite a source url if relevant>"}  - finish
Rules:
- Prefer 1-2 searches, then answer. Only fetch a page when snippets are insufficient.
- Be concise and factual. If unsure, give your best inference and say it's uncertain.
- Output ONLY the JSON object — no prose, no markdown."""


def _scratchpad_text(steps: List[Dict[str, str]]) -> str:
    if not steps:
        return "(no observations yet)"
    out = []
    for s in steps:
        out.append(f"[{s['action']}] {s['arg']}\n-> {s['observation']}")
    return "\n\n".join(out)


async def execute_research_column(
    prompt_template: str,
    lead_data: dict,
    columns_config: list,
    max_steps: int = 4,
    output_format: str = "text",
) -> Dict[str, Any]:
    """Run the research agent for one row.

    Returns {"success": bool, "value": str, "error": str|None}.
    """
    values = _get_lead_values(lead_data, columns_config)
    question = _resolve_prompt(prompt_template, values)
    if not question.strip():
        return {"success": False, "value": "", "error": "empty_prompt"}

    steps: List[Dict[str, str]] = []
    bound = max(1, min(int(max_steps or 4), MAX_STEPS_CAP))

    for _ in range(bound):
        user = (
            f"Question: {question}\n\n"
            f"Observations so far:\n{_scratchpad_text(steps)}\n\n"
            f"Choose your next action as a JSON object."
        )
        decision = await llm.extract_json(user, system=_REACT_SYSTEM, max_tokens=400)
        action = (decision or {}).get("action", "")

        if action == "answer":
            final = str(decision.get("final", "")).strip()
            if final:
                return {"success": True, "value": final, "error": None}
            break  # empty answer → fall through to forced synthesis

        if action == "search":
            query = str(decision.get("query", "")).strip()
            if not query:
                steps.append({"action": "search", "arg": "(missing query)", "observation": "no query given"})
                continue
            try:
                results = await _ddg_search(query, max_results=SEARCH_RESULTS)
            except Exception as e:
                results = []
                logger.debug(f"research search failed: {e}")
            obs = "\n".join(
                f"- {r.get('title','')} :: {r.get('href','')}\n  {r.get('body','')[:160]}"
                for r in results[:SEARCH_RESULTS]
            ) or "no results"
            steps.append({"action": "search", "arg": query, "observation": obs})
            continue

        if action == "fetch":
            url = str(decision.get("url", "")).strip()
            ok, reason = _is_safe_public_url(url) if url else (False, "missing url")
            if not ok:
                steps.append({"action": "fetch", "arg": url, "observation": f"blocked: {reason}"})
                continue
            try:
                from apps.api.services.scraper import UniversalScraper
                page = await UniversalScraper().scrape(url)
                text = (page.get("preview_text") or "")[:FETCH_CHARS]
                obs = text or f"(no readable text; status {page.get('status')})"
            except Exception as e:
                obs = f"fetch error: {str(e)[:120]}"
            steps.append({"action": "fetch", "arg": url, "observation": obs})
            continue

        # Unknown / malformed action → nudge with one more step
        steps.append({"action": "noop", "arg": str(action), "observation": "invalid action"})

    # Out of steps (or empty answer): force a final answer from what we gathered.
    final_prompt = (
        f"Question: {question}\n\n"
        f"Research notes:\n{_scratchpad_text(steps)}\n\n"
        f"Give a concise, factual final answer based ONLY on the notes above. "
        f"If the notes are insufficient, say what's known and that it's uncertain."
    )
    final = (await llm.complete(final_prompt, system="You are a concise research assistant.", max_tokens=300)).strip()
    if final:
        return {"success": True, "value": final, "error": None}
    return {"success": False, "value": "", "error": "no_answer"}
