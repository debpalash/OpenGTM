"""
Research-column ("Claygent") offline eval target — native vs legacy, with a gate.

Scores `execute_research_column` over deterministic fixtures of
(prompt, expected_answer_contains, expected_citation_domain) with the web tools
(search/fetch) and the LLM mocked, for BOTH the native tool-use path and the
legacy ReAct path. Reports correctness + citation-presence and applies a pass/
fail GATE so §10's "compare native to the legacy baseline" can block a rollout:

  - native correctness >= legacy correctness - EVAL_REGRESSION_TOLERANCE (default 0)
  - native citation-presence >= CITATION_FLOOR (default 0.9) on cases that should cite

No network, no real Anthropic SDK. Run:

  PYTHONPATH=. uv run --group dev python -m \
    apps.api.services.leadgen.enrichment.eval.research_eval
  → exits non-zero if the gate fails.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
from dataclasses import dataclass
from typing import Dict, List, Optional

from apps.api.services.workbook import research_column as RC


# ── Fixtures ──────────────────────────────────────────────────────────

@dataclass
class Fixture:
    prompt: str
    company: str
    # The scripted "world": search results + per-URL page text the mock serves.
    search_results: List[dict]
    pages: Dict[str, str]
    # The canned model answer + citation the mock LLM "produces".
    answer: str
    citation_url: Optional[str]
    expect_answer_contains: str
    expect_citation_domain: Optional[str]  # None → no citation expected


FIXTURES: List[Fixture] = [
    Fixture(
        prompt="Does {company} have SOC 2? Cite the source.",
        company="Acme",
        search_results=[{"title": "Acme Trust", "href": "https://acme.example.com/soc2",
                         "body": "Acme is SOC 2 Type II certified."}],
        pages={"https://acme.example.com/soc2": "Acme is SOC 2 Type II certified."},
        answer="Yes — Acme is SOC 2 Type II certified.",
        citation_url="https://acme.example.com/soc2",
        expect_answer_contains="SOC 2",
        expect_citation_domain="acme.example.com",
    ),
    Fixture(
        prompt="Does {company} use Kubernetes? Cite the source.",
        company="Globex",
        search_results=[{"title": "Globex Eng", "href": "https://globex.example.com/stack",
                         "body": "Globex runs Kubernetes in production."}],
        pages={"https://globex.example.com/stack": "Globex runs Kubernetes in production."},
        answer="Yes — Globex runs Kubernetes in production.",
        citation_url="https://globex.example.com/stack",
        expect_answer_contains="Kubernetes",
        expect_citation_domain="globex.example.com",
    ),
]


# ── Mock plumbing (shared between native + legacy runs) ───────────────

class _Usage:
    input_tokens = 10
    output_tokens = 10
    cache_creation_input_tokens = 0
    cache_read_input_tokens = 0


class _TextBlock:
    type = "text"

    def __init__(self, text):
        self.text = text


class _ToolUseBlock:
    type = "tool_use"

    def __init__(self, name, inp, id="tu"):
        self.name = name
        self.input = inp
        self.id = id


class _Resp:
    def __init__(self, content, stop_reason):
        self.content = content
        self.stop_reason = stop_reason
        self.usage = _Usage()


class _EvalLLM:
    """Mock llm for both paths.

    Native: turn 1 = fetch the first result URL, turn 2 = end; synth returns the
    canned answer + citation. Legacy: extract_json returns fetch then answer;
    complete returns the canned answer.
    """

    def __init__(self, fx: Fixture):
        self.fx = fx
        self._native_turn = 0

    # native path
    async def anthropic_tool_call(self, messages, *, system=None, tools=None,
                                  tool_choice=None, output_format=None,
                                  max_tokens=1024, prov=None):
        if output_format is not None:
            cit = ([{"url": self.fx.citation_url, "title": self.fx.company, "quoted_text": "..."}]
                   if self.fx.citation_url else [])
            return _Resp([_TextBlock(json.dumps({"answer": self.fx.answer, "citations": cit}))], "end_turn")
        self._native_turn += 1
        if self._native_turn == 1 and self.fx.citation_url:
            return _Resp([_ToolUseBlock("fetch", {"url": self.fx.citation_url})], "tool_use")
        return _Resp([_TextBlock(self.fx.answer)], "end_turn")

    def anthropic_cost_usd(self, usage, prov=None):
        return 0.001

    def anthropic_provider(self):
        return {"id": "anthropic", "model": "claude-opus-4-8"}

    # legacy path
    async def extract_json(self, prompt, system="", max_tokens=1024):
        if self.fx.citation_url and "Observations so far:\n(no observations yet)" in prompt:
            return {"action": "fetch", "url": self.fx.citation_url}
        return {"action": "answer", "final": self.fx.answer}

    async def complete(self, prompt, system="", max_tokens=1024, temperature=0.2):
        return self.fx.answer


def _install_mocks(monkeypatch, fx: Fixture, native: bool):
    fake = _EvalLLM(fx)
    monkeypatch.setattr(RC, "llm", fake)
    monkeypatch.setattr(RC, "_native_enabled", lambda: native)
    monkeypatch.setattr(RC, "_get_lead_values", lambda lead, cols: {"company": fx.company})
    monkeypatch.setattr(RC, "_is_safe_public_url", lambda u: (True, ""))
    monkeypatch.setattr(RC, "_revalidate_public_url", lambda u: (True, ""))

    async def _search(query, max_results=5):
        return fx.search_results
    monkeypatch.setattr(RC, "_ddg_search", _search)

    class _Scraper:
        async def scrape(self, url):
            return {"preview_text": fx.pages.get(url, ""), "status": 200}
    import apps.api.services.scraper as scraper_mod
    monkeypatch.setattr(scraper_mod, "UniversalScraper", lambda: _Scraper())
    return fake


# ── Scoring ───────────────────────────────────────────────────────────

@dataclass
class EvalResult:
    path: str
    n: int
    correctness: float
    citation_presence: float


async def _run_one(fx: Fixture, native: bool) -> dict:
    out = await RC.execute_research_column(fx.prompt, {}, [], max_steps=4, cell_budget_usd=100.0)
    value = (out.get("value") or "")
    correct = fx.expect_answer_contains.lower() in value.lower()
    cited = False
    if fx.expect_citation_domain:
        md = (out.get("metadata") or {}).get("research") or {}
        cites = md.get("citations") or []
        cited = any(fx.expect_citation_domain in (c.get("url") or "") for c in cites)
    return {"correct": correct, "expect_citation": bool(fx.expect_citation_domain), "cited": cited}


def run_eval(native: bool) -> EvalResult:
    """Run all fixtures for one path. Self-contained monkeypatching via pytest's
    MonkeyPatch so this is callable from a CLI without a test runner."""
    from _pytest.monkeypatch import MonkeyPatch

    results = []
    for fx in FIXTURES:
        mp = MonkeyPatch()
        try:
            _install_mocks(mp, fx, native)
            results.append(asyncio.run(_run_one(fx, native)))
        finally:
            mp.undo()

    n = len(results)
    correctness = sum(1 for r in results if r["correct"]) / n if n else 0.0
    cite_cases = [r for r in results if r["expect_citation"]]
    citation_presence = (sum(1 for r in cite_cases if r["cited"]) / len(cite_cases)
                         if cite_cases else 1.0)
    return EvalResult("native" if native else "legacy", n, correctness, citation_presence)


def gate(native: EvalResult, legacy: EvalResult,
         tolerance: float = 0.0, citation_floor: float = 0.9) -> tuple[bool, str]:
    """Pass/fail gate: native correctness >= legacy - tolerance AND native
    citation-presence >= floor."""
    ok_corr = native.correctness >= legacy.correctness - tolerance
    ok_cite = native.citation_presence >= citation_floor
    msg = (f"correctness native={native.correctness:.3f} legacy={legacy.correctness:.3f} "
           f"(tol={tolerance}) → {'OK' if ok_corr else 'FAIL'}; "
           f"citation native={native.citation_presence:.3f} floor={citation_floor} "
           f"→ {'OK' if ok_cite else 'FAIL'}")
    return (ok_corr and ok_cite), msg


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Research-column eval with a regression gate")
    ap.add_argument("--tolerance", type=float,
                    default=float(os.environ.get("EVAL_REGRESSION_TOLERANCE", "0")))
    ap.add_argument("--citation-floor", type=float, default=0.9)
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args(argv)

    native = run_eval(native=True)
    legacy = run_eval(native=False)
    passed, msg = gate(native, legacy, args.tolerance, args.citation_floor)

    if args.json:
        print(json.dumps({
            "native": native.__dict__, "legacy": legacy.__dict__,
            "passed": passed, "message": msg,
        }, indent=2))
    else:
        print(f"native : correctness={native.correctness:.3f} citation={native.citation_presence:.3f} n={native.n}")
        print(f"legacy : correctness={legacy.correctness:.3f} citation={legacy.citation_presence:.3f} n={legacy.n}")
        print(f"gate   : {'PASS' if passed else 'FAIL'} — {msg}")
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
