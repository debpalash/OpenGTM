"""
Conditional Execution Engine — "Only Run If" logic for workbook columns.

This is Clay's credit-saving feature: each enrichment/AI column can have
a condition that must be true before the column runs.

Condition syntax (simple expression language):
  - {email} == ""           → only run if email is empty
  - {company} != ""         → only run if company exists
  - {score} > 50            → only run if score > 50
  - {status} == "active"    → only run if status matches
  - {email} == "" AND {website} != ""  → compound conditions
  - {email} == "" OR {phone} == ""     → OR conditions
  - true / false / always / never      → literals

Column config example:
  {
    "id": "email_enrichment",
    "type": "waterfall",
    "condition": "{email} == \"\" AND {website} != \"\"",
    "waterfall": ["mailscout", "crosslinked"]
  }
"""

import logging
import re
from typing import Any, Dict, Optional

logger = logging.getLogger("workbook.conditions")


def evaluate_condition(
    condition: str,
    row_cells: dict,
    columns_config: list,
) -> bool:
    """Evaluate a column condition against row data.

    Args:
        condition: The condition expression string
        row_cells: The row's cell data {col_id: {value, status, ...}}
        columns_config: The workbook's column configuration

    Returns:
        True if the condition passes (column should run), False otherwise.
        Returns True if no condition is set (always run).
    """
    if not condition:
        return True

    condition = condition.strip()

    # Literal shortcuts
    if condition.lower() in ("true", "always", "1", "yes"):
        return True
    if condition.lower() in ("false", "never", "0", "no"):
        return False

    # Build values dict
    values = _get_values(row_cells, columns_config)

    # Resolve {column} placeholders to actual values
    resolved = _resolve_placeholders(condition, values)

    # Handle AND / OR compound conditions
    if " AND " in resolved:
        parts = resolved.split(" AND ")
        return all(_eval_single(p.strip()) for p in parts)

    if " OR " in resolved:
        parts = resolved.split(" OR ")
        return any(_eval_single(p.strip()) for p in parts)

    return _eval_single(resolved)


def _get_values(row_cells: dict, columns_config: list) -> Dict[str, str]:
    """Extract flat {col_id: value, col_name: value} dict."""
    col_name_map = {c["id"]: c.get("name", c["id"]) for c in columns_config}
    values = {}

    for col_id, cell in row_cells.items():
        val = cell.get("value", "") if isinstance(cell, dict) else cell
        val_str = str(val) if val is not None else ""
        values[col_id] = val_str
        values[col_name_map.get(col_id, col_id)] = val_str

    return values


def _resolve_placeholders(condition: str, values: Dict[str, str]) -> str:
    """Replace {column_id} with quoted values."""
    def replacer(match):
        key = match.group(1).strip()
        # Try exact match
        if key in values:
            return f'"{values[key]}"'
        # Try case-insensitive
        key_lower = key.lower()
        for k, v in values.items():
            if k.lower() == key_lower:
                return f'"{v}"'
        # Not found — return empty
        return '""'

    return re.sub(r'\{([^}]+)\}', replacer, condition)


def _eval_single(expr: str) -> bool:
    """Evaluate a single comparison expression.

    Supported: ==, !=, >, <, >=, <=, contains, not_empty, is_empty
    """
    expr = expr.strip()

    # Special functions
    if expr.lower().startswith("not_empty("):
        val = _extract_paren(expr)
        return val not in ("", '""', "None", "null", "N/A")

    if expr.lower().startswith("is_empty("):
        val = _extract_paren(expr)
        return val in ("", '""', "None", "null", "N/A")

    # Comparison operators (order matters — check >= before >)
    for op in ["!=", ">=", "<=", "==", ">", "<"]:
        if op in expr:
            parts = expr.split(op, 1)
            if len(parts) == 2:
                left = _clean_value(parts[0].strip())
                right = _clean_value(parts[1].strip())
                return _compare(left, right, op)

    # "contains" keyword
    if " contains " in expr.lower():
        parts = re.split(r'\s+contains\s+', expr, flags=re.IGNORECASE)
        if len(parts) == 2:
            left = _clean_value(parts[0].strip())
            right = _clean_value(parts[1].strip())
            return right.lower() in left.lower()

    # Fail CLOSED: an unparseable condition must NOT run the column. The whole
    # point of "only run if" is to save spend; defaulting to run means a typo in
    # the condition silently bills every paid provider on every row. Skip and
    # surface the misconfiguration loudly so the user can fix the expression.
    logger.warning(
        "Unparseable column condition %r — skipping column (fail-closed). "
        "Fix the condition expression to run it.", expr,
    )
    return False


def _clean_value(val: str) -> str:
    """Remove surrounding quotes from a value."""
    val = val.strip()
    if len(val) >= 2 and val[0] == '"' and val[-1] == '"':
        return val[1:-1]
    if len(val) >= 2 and val[0] == "'" and val[-1] == "'":
        return val[1:-1]
    return val


def _compare(left: str, right: str, op: str) -> bool:
    """Compare two values with the given operator."""
    # Try numeric comparison
    try:
        left_num = float(left)
        right_num = float(right)
        if op == "==": return left_num == right_num
        if op == "!=": return left_num != right_num
        if op == ">":  return left_num > right_num
        if op == "<":  return left_num < right_num
        if op == ">=": return left_num >= right_num
        if op == "<=": return left_num <= right_num
    except (ValueError, TypeError):
        pass

    # String comparison
    if op == "==": return left == right
    if op == "!=": return left != right
    if op == ">":  return left > right
    if op == "<":  return left < right
    if op == ">=": return left >= right
    if op == "<=": return left <= right

    # Unknown operator — fail closed (don't spend on an expression we can't read).
    return False


def _extract_paren(expr: str) -> str:
    """Extract value from function call: fn(value) -> value."""
    match = re.search(r'\((.+)\)', expr)
    if match:
        return _clean_value(match.group(1))
    return ""
