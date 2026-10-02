"""Formula columns preserve selected values and skip unused boolean operands."""
import asyncio

import pytest

from apps.api.services.workbook.formula_column import execute_formula_column


@pytest.mark.parametrize("formula,primary,secondary,expected", [
    ("{Email} or {Backup}", "first@example.invalid", "backup@example.invalid", "first@example.invalid"),
    ("{Email} or {Backup}", "", "backup@example.invalid", "backup@example.invalid"),
    ("{Email} and {Backup}", "first@example.invalid", "backup@example.invalid", "backup@example.invalid"),
    ("{Email} and {Backup}", "", "backup@example.invalid", ""),
])
def test_formula_boolean_operator_keeps_the_selected_contact_value(formula, primary, secondary, expected):
    result = asyncio.run(execute_formula_column(
        {"type": "formula", "formula": formula},
        {"email": primary, "secondary_email": secondary},
        [{"id": "email", "name": "Email", "type": "lead_field"},
         {"id": "secondary_email", "name": "Backup", "type": "lead_field"}],
    ))
    expected_result = ({"success": True, "value": expected, "error": None} if expected
                       else {"success": False, "value": None, "error": "empty_result"})
    assert result == expected_result


@pytest.mark.parametrize("formula,expected", [
    ("int({Count}) != 0 and 10 / int({Count}) > 2", "false"),
    ("{Email} != '' or 1 / 0 > 0", "true"),
])
def test_formula_boolean_guard_does_not_evaluate_the_unused_branch(formula, expected):
    result = asyncio.run(execute_formula_column(
        {"type": "formula", "formula": formula},
        {"count": "0", "email": "first@example.invalid"},
        [{"id": "count", "name": "Count", "type": "lead_field"},
         {"id": "email", "name": "Email", "type": "lead_field"}],
    ))
    assert result == {"success": True, "value": expected, "error": None}
