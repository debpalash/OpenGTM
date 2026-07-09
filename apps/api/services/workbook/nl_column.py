"""
NL → column generator ("AI formula generator", Clay parity).

Turns a natural-language instruction ("extract the domain from the website
URL") into a ready-to-add column config. The LLM picks the CHEAPEST sufficient
column kind:

  formula     — pure string/number transform, runs the safe AST evaluator
                (formula_column.py) — zero marginal cost per row.
  ai_formula  — needs semantic judgment → LLM prompt with {Column} refs.
  http        — needs an external API call → http column config skeleton.

Every candidate is VALIDATED before it is returned:
  * formula expressions must parse + evaluate against the safe evaluator on a
    dummy row. On failure we retry ONCE with the error fed back, then fall back
    to an ai_formula column (never return a broken formula).
  * ai_formula / http configs must pass the same Pydantic schema the
    add-column path uses (ColumnConfig).
"""

import logging
import re
from typing import Any, Dict, List, Optional, Tuple

from apps.api.services.leadgen.llm import llm
from apps.api.services.workbook.formula_column import (
    _FUNCS, _STR_METHODS, evaluate_formula,
)
from apps.api.services.workbook.schemas import ColumnConfig

logger = logging.getLogger("workbook.nl_column")


class NLColumnError(Exception):
    """Generation failed in a way we can't repair (bad/empty LLM output)."""


_KINDS = ("formula", "ai_formula", "http")
_HTTP_METHODS = ("GET", "POST", "PUT", "PATCH")

# ── Prompt construction ────────────────────────────────────────────────────

# The DSL reference is DERIVED from the evaluator's whitelists so the prompt
# can never drift from what actually validates.
_DSL_REFERENCE = f"""FORMULA DSL (safe expression evaluator — NOT Python, NOT JavaScript):
- Reference other columns as {{Column Name}} (curly braces, exact or case-insensitive name).
- Allowed bare functions: {", ".join(sorted(_FUNCS))}.
- Allowed string methods on a value: .{", .".join(sorted(_STR_METHODS))}.
- Operators: + - * / // %  |  comparisons == != < <= > >=  |  and / or  |  ternary "x if cond else y" | indexing/slicing [0], [0:4].
- Literals: strings, numbers, lists. NOTHING else: no imports, no attribute access, no regex, no other functions or methods.
- Missing columns resolve to "" and out-of-range indexes return "" (safe)."""

_SYSTEM = (
    "You translate a user's natural-language request into a spreadsheet column config "
    "for a lead-enrichment workbook. Pick the CHEAPEST kind that can do the job:\n"
    '1. "formula" — deterministic string/number transforms of existing columns (free, instant).\n'
    '2. "ai_formula" — requires semantic judgment, classification, summarization or writing (costs an LLM call per row).\n'
    '3. "http" — requires calling an external HTTP API (give a config skeleton the user finishes).\n'
    "Never pick ai_formula for something a formula can do. Respond with JSON only."
)

_FEW_SHOTS = """Examples:
Instruction: extract the domain from the email
{"kind": "formula", "name": "Email Domain", "formula": "{Email}.split(\\"@\\")[1]", "explanation": "Splits the email on @ and keeps the domain part. Free — no LLM or API calls."}

Instruction: full name in caps
{"kind": "formula", "name": "Full Name", "formula": "upper(default({First Name}, \\"\\")) + \\" \\" + upper({Last Name})", "explanation": "Uppercases and joins the name columns."}

Instruction: classify each company as B2B or B2C
{"kind": "ai_formula", "name": "B2B or B2C", "prompt": "Classify {Company} ({Description}) as exactly one of: B2B, B2C, Both. Reply with only the label.", "explanation": "Semantic judgment about the business model — needs an LLM per row."}

Instruction: get the company logo from clearbit
{"kind": "http", "name": "Logo URL", "http_url": "https://logo.clearbit.com/{Website}", "http_method": "GET", "explanation": "Calls Clearbit's logo API per row using the Website column. Add auth headers if your plan needs them."}"""


def _columns_summary(columns_config: List[dict]) -> str:
    if not columns_config:
        return "(no columns yet)"
    lines = []
    for c in columns_config[:60]:
        name = c.get("name") or c.get("id") or "?"
        lines.append(f"- {{{name}}} (type: {c.get('type', 'lead_field')})")
    return "\n".join(lines)


def _build_prompt(
    instruction: str,
    columns_config: List[dict],
    error_feedback: Optional[str] = None,
) -> str:
    parts = [
        "Convert this instruction into ONE workbook column config.",
        f"\nInstruction: {instruction}",
        "\nExisting columns in the workbook (reference them as {Name}):",
        _columns_summary(columns_config),
        "",
        _DSL_REFERENCE,
        "",
        _FEW_SHOTS,
        "",
        "Return a single JSON object with keys:",
        '  kind: "formula" | "ai_formula" | "http"',
        "  name: short column title",
        '  formula: (kind=formula only) DSL expression',
        '  prompt: (kind=ai_formula only) per-row LLM prompt using {Column} placeholders',
        '  http_url, http_method, http_headers, http_body, http_extract: (kind=http only; http_extract is a JSONPath like $.data.email)',
        "  explanation: one or two sentences for the user about what the column does and why this kind",
    ]
    if error_feedback:
        parts.append(
            "\nYour previous formula FAILED validation with this error:\n"
            f"  {error_feedback}\n"
            "Fix it using ONLY the DSL above, or switch kind to ai_formula if a formula cannot express it."
        )
    return "\n".join(parts)


# ── Validation helpers ─────────────────────────────────────────────────────

def _dummy_value(name: str) -> str:
    n = name.lower()
    if "email" in n:
        return "jane.doe@acme-widgets.com"
    if any(k in n for k in ("website", "url", "domain", "linkedin", "link")):
        return "https://www.acme-widgets.com/about"
    if "phone" in n:
        return "+1 555 010 4477"
    if any(k in n for k in ("size", "score", "count", "employee", "revenue", "number", "year")):
        return "42"
    if any(k in n for k in ("name", "person", "contact", "founder")):
        return "Jane Doe"
    return "Acme Widgets Inc"


def _dummy_row(columns_config: List[dict], formula: str) -> Dict[str, str]:
    """Plausible row values for every known column AND every {ref} in the formula."""
    row: Dict[str, str] = {}
    for c in columns_config:
        for key in (c.get("name"), c.get("id")):
            if key:
                row[str(key)] = _dummy_value(str(key))
    for ref in re.findall(r"\{([^}]+)\}", formula or ""):
        row.setdefault(ref.strip(), _dummy_value(ref))
    return row


def _validate_formula(formula: str, columns_config: List[dict]) -> Optional[str]:
    """Parse + evaluate on a dummy row. Returns an error string, or None if OK."""
    if not (formula or "").strip():
        return "empty formula"
    try:
        evaluate_formula(formula, _dummy_row(columns_config, formula))
    except Exception as e:  # FormulaError, ValueError, TypeError, ZeroDivisionError…
        return str(e)[:300]
    return None


_SLUG_RE = re.compile(r"[^a-z0-9]+")


def _make_id(name: str, columns_config: List[dict]) -> str:
    base = _SLUG_RE.sub("_", (name or "generated_column").lower()).strip("_") or "generated_column"
    existing = {str(c.get("id")) for c in columns_config}
    col_id, n = base, 2
    while col_id in existing:
        col_id, n = f"{base}_{n}", n + 1
    return col_id


def _finalize(col: Dict[str, Any], columns_config: List[dict]) -> Dict[str, Any]:
    """Run the candidate through the same schema the add-column path uses."""
    validated = ColumnConfig(**col)
    return validated.model_dump(exclude_none=True)


def _ai_formula_fallback(instruction: str, columns_config: List[dict], name: str) -> Dict[str, Any]:
    """Deterministic fallback when a formula can't be produced: wrap the
    instruction in a per-row LLM prompt over the workbook's columns."""
    refs = [c.get("name") or c.get("id") for c in columns_config if c.get("name") or c.get("id")]
    ctx = "\n".join(f"{r}: {{{r}}}" for r in refs[:8])
    prompt = f"{instruction}\n\nRow data:\n{ctx}" if ctx else instruction
    return {
        "id": _make_id(name, columns_config),
        "name": name,
        "type": "ai_formula",
        "width": 300,
        "prompt": prompt,
    }


# ── Candidate → column config ──────────────────────────────────────────────

def _build_candidate(
    data: dict, instruction: str, columns_config: List[dict]
) -> Tuple[Optional[Dict[str, Any]], str, str, Optional[str]]:
    """Turn one LLM answer into (column, kind, explanation, formula_error).

    formula_error is set (and column is None) only when kind==formula and the
    expression failed the safe evaluator — the caller retries/falls back.
    Any other invalid answer raises NLColumnError.
    """
    if not isinstance(data, dict) or not data:
        raise NLColumnError("empty LLM response")
    kind = str(data.get("kind") or "").strip()
    if kind not in _KINDS:
        raise NLColumnError(f"unknown kind: {kind!r}")

    name = str(data.get("name") or "").strip() or instruction[:40].strip().title() or "Generated Column"
    explanation = str(data.get("explanation") or "").strip()
    col: Dict[str, Any] = {
        "id": _make_id(name, columns_config),
        "name": name,
        "type": kind,
        "width": 300 if kind == "ai_formula" else 200,
    }

    if kind == "formula":
        formula = str(data.get("formula") or "").strip()
        err = _validate_formula(formula, columns_config)
        if err:
            return None, kind, explanation, err
        col["formula"] = formula

    elif kind == "ai_formula":
        prompt = str(data.get("prompt") or "").strip()
        if not prompt:
            raise NLColumnError("ai_formula answer missing prompt")
        col["prompt"] = prompt

    else:  # http
        url = str(data.get("http_url") or "").strip()
        if not url:
            raise NLColumnError("http answer missing http_url")
        method = str(data.get("http_method") or "GET").strip().upper()
        if method not in _HTTP_METHODS:
            method = "GET"
        col["http_url"] = url
        col["http_method"] = method
        headers = data.get("http_headers")
        if isinstance(headers, dict) and headers:
            col["http_headers"] = {str(k): str(v) for k, v in headers.items()}
        if data.get("http_body") is not None and method != "GET":
            col["http_body"] = data["http_body"]
        extract = str(data.get("http_extract") or "").strip()
        if extract:
            col["http_extract"] = extract

    return _finalize(col, columns_config), kind, explanation, None


# ── Public API ─────────────────────────────────────────────────────────────

async def generate_column(instruction: str, columns_config: List[dict]) -> Dict[str, Any]:
    """NL instruction → {"column": {...}, "kind": ..., "explanation": ...}.

    Formula candidates are validated against the safe evaluator; one repair
    retry (error fed back), then fall back to ai_formula. ai_formula/http
    candidates are validated with the add-column ColumnConfig schema.
    """
    instruction = (instruction or "").strip()
    if not instruction:
        raise NLColumnError("empty instruction")
    columns_config = columns_config or []

    data = await llm.extract_json(
        _build_prompt(instruction, columns_config), system=_SYSTEM, max_tokens=1024
    )
    column, kind, explanation, formula_err = _build_candidate(data, instruction, columns_config)

    if formula_err is not None:
        logger.info("nl_column: formula failed validation (%s) — retrying once", formula_err)
        retry = await llm.extract_json(
            _build_prompt(instruction, columns_config, error_feedback=formula_err),
            system=_SYSTEM,
            max_tokens=1024,
        )
        try:
            column, kind, explanation, formula_err = _build_candidate(retry, instruction, columns_config)
        except NLColumnError:
            column, formula_err = None, "retry produced an invalid answer"
        if formula_err is not None:
            # Final fallback: never return a broken formula — degrade to ai_formula.
            name = str((retry or data or {}).get("name") or "").strip() or instruction[:40].strip().title()
            column = _finalize(_ai_formula_fallback(instruction, columns_config, name), columns_config)
            kind = "ai_formula"
            explanation = (
                "A safe formula for this couldn't be validated, so this column uses an AI "
                "prompt per row instead."
            )

    if not explanation:
        explanation = f"Adds a {kind} column for: {instruction}"
    return {"column": column, "kind": kind, "explanation": explanation}
