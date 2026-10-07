"""Formula slice strides follow Python's existing slice syntax."""

import asyncio

import pytest

from apps.api.services.workbook.formula_column import evaluate_formula, execute_formula_column


@pytest.mark.parametrize("expression, values, expected", [
    ("{name}[::2]", {"name": "Acme"}, "Am"),
    ("{name}[::-1]", {"name": "Acme"}, "emcA"),
    ("{name}[1:5:2]", {"name": "abcdef"}, "bd"),
    ("{name}[4:0:-2]", {"name": "abcdef"}, "ec"),
    ("{name}[::int({step})]", {"name": "abcdef", "step": "3"}, "ad"),
    ("[1, 2, 3, 4][::2]", {}, [1, 3]),
    ("{name}[1:3]", {"name": "Acme"}, "cm"),
])
def test_slice_step(expression, values, expected):
    assert evaluate_formula(expression, values) == expected


def test_formula_column_wrapper_returns_strided_value():
    result = asyncio.run(execute_formula_column(
        {"type": "formula", "formula": "{name}[::-1]"},
        {"name": "Acme"}, [{"id": "name", "name": "Name", "type": "lead_field"}],
    ))
    assert result == {"success": True, "value": "emcA", "error": None}
