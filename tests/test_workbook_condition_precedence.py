"""Mixed column conditions use AND precedence over OR."""

import itertools

import pytest

from apps.api.services.workbook.conditions import evaluate_condition


@pytest.mark.parametrize("a,b,c", list(itertools.product([False, True], repeat=3)))
@pytest.mark.parametrize("expression,order", [
    ('{a} == "yes" OR {b} == "yes" AND {c} == "yes"', "or-and"),
    ('{a} == "yes" AND {b} == "yes" OR {c} == "yes"', "and-or"),
])
def test_mixed_conditions_follow_boolean_truth_table(a, b, c, expression, order):
    cells = {key: {"value": "yes" if value else "no"} for key, value in zip("abc", (a, b, c))}
    expected = (a or (b and c)) if order == "or-and" else ((a and b) or c)
    assert evaluate_condition(expression, cells, []) is expected


def test_mixed_conditions_keep_operator_text_inside_literals_and_cells():
    cells = {"name": {"value": "Sales OR Marketing AND Research"}, "score": {"value": 10}}
    assert evaluate_condition(
        '{name} == "Sales OR Marketing AND Research" OR {score} > 20 AND {score} < 0', cells, []
    )
    assert not evaluate_condition(
        '{name} == "Other" OR {score} > 20 AND {name} == "Sales OR Marketing AND Research"', cells, []
    )
