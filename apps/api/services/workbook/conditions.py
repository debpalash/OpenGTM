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
from typing import Dict

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

    # Parse syntax before resolving cells so their contents remain operand data.
    parts = _split_unquoted(condition, r" AND ")
    if len(parts) > 1:
        return all(_eval_single(p.strip(), values) for p in parts)

    parts = _split_unquoted(condition, r" OR ")
    if len(parts) > 1:
        return any(_eval_single(p.strip(), values) for p in parts)

    return _eval_single(condition, values)


def _split_unquoted(expr: str, pattern: str, maxsplit: int = 0, flags: int = 0) -> list[str]:
    """Split operators outside quoted literals and {column} references."""
    separator = re.compile(pattern, flags)
    parts = []
    start = i = 0
    quote = None
    while i < len(expr):
        char = expr[i]
        if quote:
            if char == "\\":
                i += 2
                continue
            if char == quote:
                quote = None
        elif char in ('"', "'") and (
            i == 0 or expr[i - 1].isspace() or expr[i - 1] in "(=<>!"
        ):
            quote = char
        elif char == "{" and (end := expr.find("}", i + 1)) != -1:
            i = end + 1
            continue
        else:
            match = separator.match(expr, i)
            if match:
                parts.append(expr[start:i])
                start = i = match.end()
                if maxsplit and len(parts) == maxsplit:
                    break
                continue
        i += 1
    return parts + [expr[start:]]


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


def _eval_single(expr: str, values: Dict[str, str]) -> bool:
    """Evaluate a single comparison expression.

    Supported: ==, !=, >, <, >=, <=, contains, not_empty, is_empty
    """
    expr = expr.strip()

    # Special functions
    if expr.lower().startswith("not_empty("):
        val = _extract_paren(_resolve_placeholders(expr, values))
        return val not in ("", '""', "None", "null", "N/A")

    if expr.lower().startswith("is_empty("):
        val = _extract_paren(_resolve_placeholders(expr, values))
        return val in ("", '""', "None", "null", "N/A")

    # Comparison operators (order matters — check >= before >)
    for op in ["!=", ">=", "<=", "==", ">", "<"]:
        parts = _split_unquoted(expr, re.escape(op), maxsplit=1)
        if len(parts) == 2:
            left = _clean_value(_resolve_placeholders(parts[0], values))
            right = _clean_value(_resolve_placeholders(parts[1], values))
            return _compare(left, right, op)

    # "contains" keyword
    parts = _split_unquoted(expr, r'\s+contains\s+', flags=re.IGNORECASE)
    if len(parts) == 2:
        left = _clean_value(_resolve_placeholders(parts[0], values))
        right = _clean_value(_resolve_placeholders(parts[1], values))
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
