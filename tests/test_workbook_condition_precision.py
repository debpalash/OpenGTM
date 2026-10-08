"""Public workbook conditions preserve numeric distinctions in actual row values."""

import pytest

from apps.api.services.workbook.conditions import evaluate_condition


@pytest.mark.parametrize("value, threshold, operator, expected", [
    ("9007199254740993", "9007199254740992", "==", False),
    ("9007199254740993", "9007199254740992", "!=", True),
    ("9007199254740993", "9007199254740992", ">", True),
    ("9007199254740993", "9007199254740992", "<", False),
    ("9007199254740993", "9007199254740992", ">=", True),
    ("9007199254740993", "9007199254740992", "<=", False),
    ("-9007199254740993", "-9007199254740992", "==", False),
    ("-9007199254740993", "-9007199254740992", "!=", True),
    ("-9007199254740993", "-9007199254740992", ">", False),
    ("-9007199254740993", "-9007199254740992", "<", True),
    ("-9007199254740993", "-9007199254740992", ">=", False),
    ("-9007199254740993", "-9007199254740992", "<=", True),
    ("9007199254740992.5", "9007199254740992.25", "==", False),
    ("9007199254740992.5", "9007199254740992.25", "!=", True),
    ("9007199254740992.5", "9007199254740992.25", ">", True),
    ("9007199254740992.5", "9007199254740992.25", "<", False),
    ("9007199254740992.5", "9007199254740992.25", ">=", True),
    ("9007199254740992.5", "9007199254740992.25", "<=", False),
    (9007199254740993, "9007199254740992", "==", False),
    (-9007199254740993, "-9007199254740992", "<", True),
    ("3.50", "3.5", "==", True),
    ("1e3", "1000", "==", True),
    ("009", "9", "==", True),
    ("-4.25", "-4", "<", True),
    ("Acme", '"Acme"', "==", True),
    ("Other", '"Acme"', "!=", True),
])
def test_numeric_conditions_keep_original_operand_distinctions(
    value, threshold, operator, expected,
):
    cells = {"score": {"value": value, "status": "complete"}}
    expression = f"{{score}} {operator} {threshold}"
    assert evaluate_condition(expression, cells, []) is expected
