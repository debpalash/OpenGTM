"""
Formula action column — transform row values with a SAFE expression evaluator.

Ported from eliasstravik/rowbound's formula columns (Clay-style
`{Email}.split("@")[1]`), but — per the catalog's explicit security caution —
WITHOUT eval / new Function / node:vm. We parse the expression with Python's
`ast` module and walk it against a strict node + function + method whitelist, so
no attribute escapes, no imports, no dunder access, no arbitrary calls are
possible. Anything outside the whitelist raises FormulaError.

Columns reference each other with {column} (same as AI columns). Examples:
  {Email}.split("@")[1]                 -> email domain
  upper({first_name}) + " " + {last}    -> "ELON Musk"
  default({email}, {secondary_email})   -> first non-empty
  {company_size} * 2
"""

import ast
import logging
import re
from typing import Any, Dict, List

logger = logging.getLogger("workbook.formula_column")


class FormulaError(Exception):
    pass


# Whitelisted bare functions available in formulas.
def _default(*args):
    for a in args:
        if a not in (None, "", [], {}):
            return a
    return ""


_FUNCS = {
    "default": _default,
    "lower": lambda s: str(s).lower(),
    "upper": lambda s: str(s).upper(),
    "strip": lambda s: str(s).strip(),
    "trim": lambda s: str(s).strip(),
    "len": lambda s: len(s),
    "str": lambda s: str(s),
    "int": lambda s: int(float(s)) if str(s).strip() not in ("", "None") else 0,
    "float": lambda s: float(s) if str(s).strip() not in ("", "None") else 0.0,
    "round": lambda s, n=0: round(float(s), int(n)),
    "title": lambda s: str(s).title(),
    "replace": lambda s, a, b: str(s).replace(a, b),
    "concat": lambda *xs: "".join(str(x) for x in xs),
}

# Whitelisted string methods callable on a value (e.g. {x}.split("@")).
_STR_METHODS = {"split", "strip", "lower", "upper", "replace", "title", "lstrip",
                "rstrip", "startswith", "endswith", "zfill", "find", "join"}

# Allowed AST node types.
_ALLOWED_NODES = (
    ast.Expression, ast.Constant, ast.Name, ast.Load,
    ast.BinOp, ast.Add, ast.Sub, ast.Mult, ast.Div, ast.Mod, ast.FloorDiv,
    ast.UnaryOp, ast.UAdd, ast.USub,
    ast.Call, ast.Attribute, ast.Subscript, ast.Slice, ast.Index if hasattr(ast, "Index") else ast.Slice,
    ast.List, ast.Tuple,
    ast.Compare, ast.Eq, ast.NotEq, ast.Lt, ast.LtE, ast.Gt, ast.GtE,
    ast.BoolOp, ast.And, ast.Or, ast.IfExp,
)


def _validate(node: ast.AST):
    for child in ast.walk(node):
        if not isinstance(child, _ALLOWED_NODES):
            raise FormulaError(f"disallowed expression element: {type(child).__name__}")
        # Calls: only bare whitelisted funcs, or whitelisted string methods.
        if isinstance(child, ast.Call):
            fn = child.func
            if isinstance(fn, ast.Name):
                if fn.id not in _FUNCS:
                    raise FormulaError(f"unknown function: {fn.id}")
            elif isinstance(fn, ast.Attribute):
                if fn.attr not in _STR_METHODS:
                    raise FormulaError(f"method not allowed: .{fn.attr}")
            else:
                raise FormulaError("unsupported call target")
        # Attributes outside a method call are forbidden (no .__class__ etc.)
        if isinstance(child, ast.Attribute) and child.attr.startswith("_"):
            raise FormulaError("dunder/private access not allowed")


def _eval(node: ast.AST, variables: Dict[str, Any]) -> Any:
    if isinstance(node, ast.Expression):
        return _eval(node.body, variables)
    if isinstance(node, ast.Constant):
        return node.value
    if isinstance(node, ast.Name):
        if node.id in variables:
            return variables[node.id]
        raise FormulaError(f"unknown name: {node.id}")
    if isinstance(node, ast.BinOp):
        l, r = _eval(node.left, variables), _eval(node.right, variables)
        op = node.op
        if isinstance(op, ast.Add):
            if isinstance(l, str) or isinstance(r, str):
                return f"{l}{r}"
            return l + r
        if isinstance(op, ast.Sub): return l - r
        if isinstance(op, ast.Mult): return l * r
        if isinstance(op, ast.Div): return l / r
        if isinstance(op, ast.FloorDiv): return l // r
        if isinstance(op, ast.Mod): return l % r
    if isinstance(node, ast.UnaryOp):
        v = _eval(node.operand, variables)
        return +v if isinstance(node.op, ast.UAdd) else -v
    if isinstance(node, ast.Call):
        args = [_eval(a, variables) for a in node.args]
        if isinstance(node.func, ast.Name):
            return _FUNCS[node.func.id](*args)
        # method call on a value
        target = _eval(node.func.value, variables)
        return getattr(str(target), node.func.attr)(*args)
    if isinstance(node, ast.Attribute):
        # only reached as a Call target; standalone attribute is blocked above
        raise FormulaError("bare attribute access not allowed")
    if isinstance(node, ast.Subscript):
        target = _eval(node.value, variables)
        idx = node.slice
        if isinstance(idx, ast.Slice):
            lo = _eval(idx.lower, variables) if idx.lower else None
            hi = _eval(idx.upper, variables) if idx.upper else None
            return target[lo:hi]
        key = _eval(idx, variables)
        try:
            return target[key]
        except (IndexError, KeyError):
            return ""
    if isinstance(node, (ast.List, ast.Tuple)):
        return [_eval(e, variables) for e in node.elts]
    if isinstance(node, ast.Compare):
        left = _eval(node.left, variables)
        for op, comp in zip(node.ops, node.comparators):
            right = _eval(comp, variables)
            ok = (
                (isinstance(op, ast.Eq) and left == right) or
                (isinstance(op, ast.NotEq) and left != right) or
                (isinstance(op, ast.Lt) and left < right) or
                (isinstance(op, ast.LtE) and left <= right) or
                (isinstance(op, ast.Gt) and left > right) or
                (isinstance(op, ast.GtE) and left >= right)
            )
            if not ok:
                return False
            left = right
        return True
    if isinstance(node, ast.BoolOp):
        vals = [_eval(v, variables) for v in node.values]
        return all(vals) if isinstance(node.op, ast.And) else any(vals)
    if isinstance(node, ast.IfExp):
        return _eval(node.body, variables) if _eval(node.test, variables) else _eval(node.orelse, variables)
    raise FormulaError(f"cannot evaluate {type(node).__name__}")


_PLACEHOLDER_RE = re.compile(r"\{([^}]+)\}")


def evaluate_formula(expr: str, row_values: Dict[str, str]) -> Any:
    """Evaluate a formula expression, resolving {column} refs from row_values."""
    if not expr or not expr.strip():
        raise FormulaError("empty formula")

    # Map each distinct {placeholder} to a safe variable name and collect values.
    variables: Dict[str, Any] = {}
    name_for: Dict[str, str] = {}

    def _sub(m):
        key = m.group(1).strip()
        if key not in name_for:
            var = f"_v{len(name_for)}"
            name_for[key] = var
            # resolve value (exact, then case-insensitive)
            val = row_values.get(key)
            if val is None:
                kl = key.lower()
                for k, v in row_values.items():
                    if k.lower() == kl:
                        val = v
                        break
            variables[var] = "" if val is None else val
        return name_for[key]

    safe_expr = _PLACEHOLDER_RE.sub(_sub, expr)

    try:
        tree = ast.parse(safe_expr, mode="eval")
    except SyntaxError as e:
        raise FormulaError(f"syntax error: {e}")
    _validate(tree)
    return _eval(tree, variables)


async def execute_formula_column(col_config: dict, lead_data: dict, columns_config: list) -> Dict[str, Any]:
    """Run one formula cell. Returns {success, value, error}."""
    from apps.api.services.workbook.ai_column import _get_row_values
    expr = (col_config.get("formula") or "").strip()
    if not expr:
        return {"success": False, "value": None, "error": "no_formula"}
    cells = {k: {"value": v} for k, v in lead_data.items()}
    row_values = _get_row_values(cells, columns_config)
    try:
        value = evaluate_formula(expr, row_values)
    except FormulaError as e:
        return {"success": False, "value": None, "error": str(e)[:120]}
    if value is None or value == "":
        return {"success": False, "value": None, "error": "empty_result"}
    if isinstance(value, bool):
        value = "true" if value else "false"
    return {"success": True, "value": value, "error": None}
