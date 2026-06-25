"""Hardened, fail-closed condition evaluator for paid-action gating (§3.9).

The legacy ``workbook/conditions.py`` evaluator is UNSAFE to gate spend:
  * ``_eval_single`` returns True for any unparseable expression,
  * compound parsing is a naive ``str.split(" AND ")`` on the *interpolated*
    string, and
  * placeholders are quote-wrapped into the string, so a field value containing
    ``"`` or `` AND `` can corrupt the parse tree and silently fall through to
    True.

For firing paid actions that means unexpected spend/sends. This evaluator:

  1. Parses the OPERATOR STRUCTURE of the condition FIRST (against ``{field}``
     tokens and literals), and binds field VALUES as typed operands afterwards —
     so a value containing ``"``, `` AND ``, or `` OR `` can never change the
     parse tree.
  2. FAILS CLOSED: any parse error, or any operand that cannot be resolved,
     yields ``False`` (do NOT fire) with a structured reason — the inverse of
     the legacy default-True.

Supported grammar (matches the legacy operator set the UI already exposes):
    condition := disjunction
    disjunction := conjunction ( "OR" conjunction )*
    conjunction := comparison ( "AND" comparison )*
    comparison  := operand OP operand
                 | "not_empty(" operand ")"
                 | "is_empty(" operand ")"
                 | operand "contains" operand
                 | operand "older_than" DURATION
                 | literal-bool
    operand     := "{" field "}" | quoted-string | bareword | number
    OP          := "==" | "!=" | ">=" | "<=" | ">" | "<"

``AND``/``OR``/``contains``/``older_than`` are recognised as keyword TOKENS only
when they appear OUTSIDE a ``{...}`` placeholder and outside a quoted literal —
the tokenizer guarantees that, so values are never mistaken for operators.
"""

from __future__ import annotations

import logging
import re
import time
from dataclasses import dataclass
from typing import Optional

logger = logging.getLogger("automations.safe_conditions")

_OPS = ("==", "!=", ">=", "<=", ">", "<")
_DURATION_RE = re.compile(r"^(\d+)\s*([smhdw])$", re.IGNORECASE)
_DURATION_SECONDS = {"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800}


class ConditionError(ValueError):
    """Raised when a condition cannot be parsed or an operand cannot resolve."""


@dataclass
class EvalResult:
    passed: bool
    error: Optional[str] = None  # set when fail-closed for a parse/resolve reason


# ── operand types produced by the tokenizer ────────────────────────────────

@dataclass
class _Placeholder:
    field: str


@dataclass
class _Literal:
    raw: str  # the literal text (unquoted)


# ── tokenizer ───────────────────────────────────────────────────────────────

@dataclass
class _Token:
    kind: str  # "PLACEHOLDER" | "STRING" | "WORD" | "OP" | "LPAREN" | "RPAREN"
    value: str


def _tokenize(expr: str) -> list[_Token]:
    """Tokenize structurally. Placeholders ({...}) and quoted strings are atomic,
    so their contents can never be re-interpreted as operators/keywords."""
    tokens: list[_Token] = []
    i, n = 0, len(expr)
    while i < n:
        c = expr[i]
        if c.isspace():
            i += 1
            continue
        if c == "{":
            j = expr.find("}", i)
            if j == -1:
                raise ConditionError("unterminated placeholder '{'")
            tokens.append(_Token("PLACEHOLDER", expr[i + 1:j].strip()))
            i = j + 1
            continue
        if c == '"' or c == "'":
            j = expr.find(c, i + 1)
            if j == -1:
                raise ConditionError("unterminated string literal")
            tokens.append(_Token("STRING", expr[i + 1:j]))
            i = j + 1
            continue
        if c == "(":
            tokens.append(_Token("LPAREN", "("))
            i += 1
            continue
        if c == ")":
            tokens.append(_Token("RPAREN", ")"))
            i += 1
            continue
        # multi-char operators first
        matched = False
        for op in _OPS:
            if expr.startswith(op, i):
                tokens.append(_Token("OP", op))
                i += len(op)
                matched = True
                break
        if matched:
            continue
        # bareword / number / keyword (AND/OR/contains/older_than/not_empty/...)
        m = re.match(r"[^\s(){}\"'<>=!]+", expr[i:])
        if not m:
            raise ConditionError(f"unexpected character {c!r}")
        word = m.group(0)
        tokens.append(_Token("WORD", word))
        i += len(word)
    return tokens


# ── value binding (typed, not interpolated) ─────────────────────────────────

def _build_values(cells: dict, cols: list) -> dict:
    """Flat {col_id: str, col_name: str} map from a WorkbookRow's cells.

    ``cells`` is {key: value} OR {key: {"value": ...}}. Both column id and column
    name resolve to the same value (matches legacy ``_get_values``)."""
    name_map = {}
    for c in (cols or []):
        cid = c.get("id")
        if cid is not None:
            name_map[cid] = c.get("name", cid)
    values: dict = {}
    for key, cell in (cells or {}).items():
        if isinstance(cell, dict) and "value" in cell:
            val = cell.get("value", "")
        else:
            val = cell
        val_str = "" if val is None else str(val)
        values[str(key)] = val_str
        nm = name_map.get(key)
        if nm is not None:
            values[str(nm)] = val_str
    return values


def _resolve_operand(tok: _Token, values: dict) -> str:
    """Resolve a single operand token to its string value (fail closed)."""
    if tok.kind == "PLACEHOLDER":
        key = tok.value
        if key in values:
            return values[key]
        kl = key.lower()
        for k, v in values.items():
            if k.lower() == kl:
                return v
        # Unresolved placeholder → fail closed (the field is absent on this row).
        raise ConditionError(f"unresolved field '{{{key}}}'")
    if tok.kind == "STRING":
        return tok.value
    if tok.kind == "WORD":
        return tok.value
    raise ConditionError(f"expected operand, got {tok.kind}")


def _is_empty(v: str) -> bool:
    return v.strip() in ("", "None", "null", "N/A", "NaN")


def _compare(left: str, right: str, op: str) -> bool:
    try:
        ln, rn = float(left), float(right)
        if op == "==": return ln == rn
        if op == "!=": return ln != rn
        if op == ">":  return ln > rn
        if op == "<":  return ln < rn
        if op == ">=": return ln >= rn
        if op == "<=": return ln <= rn
    except (ValueError, TypeError):
        pass
    if op == "==": return left == right
    if op == "!=": return left != right
    if op == ">":  return left > right
    if op == "<":  return left < right
    if op == ">=": return left >= right
    if op == "<=": return left <= right
    raise ConditionError(f"unknown operator {op!r}")


def _eval_comparison(tokens: list[_Token], values: dict) -> bool:
    """Evaluate ONE comparison clause (already split on AND/OR keyword tokens)."""
    if not tokens:
        raise ConditionError("empty comparison")

    # function-style: not_empty(...) / is_empty(...)
    first = tokens[0]
    if first.kind == "WORD" and first.value.lower() in ("not_empty", "is_empty"):
        if len(tokens) < 4 or tokens[1].kind != "LPAREN" or tokens[-1].kind != "RPAREN":
            raise ConditionError(f"malformed {first.value}() call")
        inner = tokens[2:-1]
        if len(inner) != 1:
            raise ConditionError(f"{first.value}() takes exactly one operand")
        val = _resolve_operand(inner[0], values)
        empty = _is_empty(val)
        return (not empty) if first.value.lower() == "not_empty" else empty

    # infix keyword operators: contains / older_than
    for idx, t in enumerate(tokens):
        if t.kind == "WORD" and t.value.lower() in ("contains", "older_than"):
            left_toks = tokens[:idx]
            right_toks = tokens[idx + 1:]
            if len(left_toks) != 1 or len(right_toks) != 1:
                raise ConditionError(f"malformed '{t.value}' expression")
            left = _resolve_operand(left_toks[0], values)
            if t.value.lower() == "contains":
                right = _resolve_operand(right_toks[0], values)
                return right.lower() in left.lower()
            # older_than: left is an epoch-ms/iso timestamp value, right a duration
            return _older_than(left, right_toks[0])

    # binary OP comparison
    for idx, t in enumerate(tokens):
        if t.kind == "OP":
            left_toks = tokens[:idx]
            right_toks = tokens[idx + 1:]
            if len(left_toks) != 1 or len(right_toks) != 1:
                raise ConditionError("malformed comparison")
            left = _resolve_operand(left_toks[0], values)
            right = _resolve_operand(right_toks[0], values)
            return _compare(left, right, t.value)

    raise ConditionError("comparison has no operator")


def _older_than(left_value: str, dur_tok: _Token) -> bool:
    if dur_tok.kind not in ("WORD", "STRING"):
        raise ConditionError("older_than duration must be like '30d'")
    m = _DURATION_RE.match(dur_tok.value.strip())
    if not m:
        raise ConditionError(f"bad duration {dur_tok.value!r} (use e.g. 30d, 12h)")
    seconds = int(m.group(1)) * _DURATION_SECONDS[m.group(2).lower()]
    # Resolve the left timestamp: accept epoch seconds, epoch ms, or ISO-8601.
    ts = _parse_timestamp(left_value)
    if ts is None:
        raise ConditionError("older_than left operand is not a timestamp")
    return (time.time() - ts) > seconds


def _parse_timestamp(v: str) -> Optional[float]:
    v = (v or "").strip()
    if not v:
        return None
    try:
        f = float(v)
        # Heuristic: > 1e12 looks like epoch-ms.
        return f / 1000.0 if f > 1e12 else f
    except ValueError:
        pass
    try:
        from datetime import datetime
        return datetime.fromisoformat(v.replace("Z", "+00:00")).timestamp()
    except Exception:
        return None


def _split_keyword(tokens: list[_Token], keyword: str) -> list[list[_Token]]:
    """Split a token list on a top-level keyword token (AND/OR). Keyword tokens
    only ever come from outside placeholders/strings (tokenizer guarantees it)."""
    groups: list[list[_Token]] = [[]]
    for t in tokens:
        if t.kind == "WORD" and t.value.upper() == keyword:
            groups.append([])
        else:
            groups[-1].append(t)
    return groups


def _evaluate_tokens(tokens: list[_Token], values: dict) -> bool:
    # OR binds looser than AND.
    or_groups = _split_keyword(tokens, "OR")
    if len(or_groups) > 1:
        return any(_evaluate_tokens(g, values) for g in or_groups)
    and_groups = _split_keyword(tokens, "AND")
    if len(and_groups) > 1:
        return all(_evaluate_tokens(g, values) for g in and_groups)
    return _eval_comparison(tokens, values)


def safe_evaluate_condition(condition: str, cells: dict, cols: list) -> EvalResult:
    """Evaluate ``condition`` against a WorkbookRow's cells, FAIL CLOSED.

    Returns ``EvalResult(passed, error)``. On any parse/resolve failure
    ``passed`` is ``False`` and ``error`` holds the reason (caller records
    ``skip_reason="condition"``)."""
    if condition is None:
        return EvalResult(True)
    cond = condition.strip()
    if cond == "":
        return EvalResult(True)
    low = cond.lower()
    if low in ("true", "always", "1", "yes"):
        return EvalResult(True)
    if low in ("false", "never", "0", "no"):
        return EvalResult(False)
    try:
        tokens = _tokenize(cond)
        if not tokens:
            return EvalResult(False, "empty condition")
        values = _build_values(cells, cols)
        return EvalResult(bool(_evaluate_tokens(tokens, values)))
    except ConditionError as e:
        logger.info("condition fail-closed: %s | condition=%r", e, condition)
        return EvalResult(False, str(e))
    except Exception as e:  # defensive: never let an evaluator bug fire spend
        logger.warning("condition unexpected error fail-closed: %s | %r", e, condition)
        return EvalResult(False, f"evaluator error: {e}")


def validate_condition(condition: str) -> None:
    """Create-time validation (§4): parse against a representative + ADVERSARIAL
    populated row (a value containing ``"``, `` AND ``, `` OR ``) to surface
    injection-style failures early. Raises ConditionError on a structural parse
    failure; an unresolved-field error on the sample row is NOT fatal (the real
    row may have the field) — only a tokenizer/structure error is raised."""
    if not condition or not condition.strip():
        return
    low = condition.strip().lower()
    if low in ("true", "always", "1", "yes", "false", "never", "0", "no"):
        return
    # 1) structural parse must succeed.
    tokens = _tokenize(condition)
    if not tokens:
        raise ConditionError("empty condition")
    # 2) collect referenced fields and bind adversarial values, then ensure the
    #    operator structure still evaluates without a structural error.
    fields = {t.value for t in tokens if t.kind == "PLACEHOLDER"}
    adversarial = '" AND {x}=="  OR evil'
    sample_cols = [{"id": f, "name": f} for f in fields]
    sample_cells = {f: adversarial for f in fields}
    values = _build_values(sample_cells, sample_cols)
    try:
        _evaluate_tokens(tokens, values)
    except ConditionError as e:
        # Re-raise only structural problems; value-driven comparison results are fine.
        msg = str(e).lower()
        if "field" in msg and "unresolved" in msg:
            return
        raise
