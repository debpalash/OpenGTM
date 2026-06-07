"""
Formula action column (Phase 4, PR B): safe AST evaluator.
Includes SECURITY tests proving no code execution / attribute escape is possible.
"""
import asyncio
import pytest

from apps.api.services.workbook.formula_column import (
    evaluate_formula, execute_formula_column, FormulaError,
)

ROW = {
    "email": "elon@spacex.com", "first_name": "Elon", "last_name": "Musk",
    "company": "SpaceX", "company_size": "500", "secondary_email": "e@x.com",
    "Email": "elon@spacex.com",  # name alias
}


# ── functional ────────────────────────────────────────────────────

def test_email_domain_extraction():
    assert evaluate_formula('{Email}.split("@")[1]', ROW) == "spacex.com"

def test_string_concat_and_upper():
    assert evaluate_formula('upper({first_name}) + " " + {last_name}', ROW) == "ELON Musk"

def test_default_picks_first_nonempty():
    assert evaluate_formula('default({missing}, {email})', ROW) == "elon@spacex.com"
    assert evaluate_formula('default({missing}, {also_missing})', ROW) == ""

def test_arithmetic():
    assert evaluate_formula('int({company_size}) * 2', ROW) == 1000

def test_replace_and_lower():
    assert evaluate_formula('lower(replace({company}, "Space", "Blue"))', ROW) == "bluex"

def test_slice_and_index():
    assert evaluate_formula('{first_name}[0]', ROW) == "E"
    assert evaluate_formula('{company}[0:4]', ROW) == "Spac"

def test_ternary():
    assert evaluate_formula('"big" if int({company_size}) > 100 else "small"', ROW) == "big"

def test_unknown_column_resolves_empty():
    assert evaluate_formula('default({nope}, "fallback")', ROW) == "fallback"


# ── security: these MUST raise, never execute ─────────────────────

@pytest.mark.parametrize("expr", [
    '__import__("os").system("echo pwned")',
    '{email}.__class__',
    '{email}.__class__.__mro__',
    'open("/etc/passwd").read()',
    'eval("1+1")',
    'exec("x=1")',
    '().__class__.__bases__',
    '{email}.encode().decode',
    'globals()',
    'lambda: 1',
])
def test_dangerous_expressions_rejected(expr):
    with pytest.raises(FormulaError):
        evaluate_formula(expr, ROW)


# ── column wrapper ────────────────────────────────────────────────

def test_execute_formula_column_ok():
    res = asyncio.run(execute_formula_column(
        {"type": "formula", "formula": '{Email}.split("@")[1]'},
        {"email": "elon@spacex.com", "Email": "elon@spacex.com"},
        [{"id": "email", "name": "Email", "type": "lead_field"}],
    ))
    assert res["success"] and res["value"] == "spacex.com"

def test_execute_formula_column_no_formula():
    res = asyncio.run(execute_formula_column({"type": "formula"}, {}, []))
    assert res["success"] is False and res["error"] == "no_formula"

def test_execute_formula_column_bad_expr_errors_gracefully():
    res = asyncio.run(execute_formula_column(
        {"type": "formula", "formula": '__import__("os")'}, {}, []))
    assert res["success"] is False and res["error"]
