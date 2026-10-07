"""Workbook conditions distinguish operator syntax from literal/cell data."""

import pytest

from apps.api.services.workbook.conditions import evaluate_condition


@pytest.mark.parametrize("value", [
    "Research AND Development",
    "Sales OR Marketing",
    "price != available",
    "score >= target",
    "score <= target",
    "status == active",
    "score > target",
    "score < target",
    "text contains words",
])
@pytest.mark.parametrize("matches", [True, False])
def test_quoted_operator_text_is_compared_as_data(value, matches):
    cells = {"company": {"value": value if matches else "Other"}}
    assert evaluate_condition(f'{{company}} == "{value}"', cells, []) is matches


@pytest.mark.parametrize("value", [
    'He said "ready AND done"',
    "Research AND Development",
    "Sales OR Marketing",
    "price != available",
    "C:\\reports\\",
    "O'Reilly OR Acme",
])
@pytest.mark.parametrize("matches", [True, False])
def test_substituted_values_cannot_introduce_operators(value, matches):
    cells = {"company": {"value": value}, "expected": {"value": value if matches else "Other"}}
    assert evaluate_condition("{company} == {expected}", cells, []) is matches


@pytest.mark.parametrize("quote", ['"', "'"])
def test_literal_operator_text_without_cells(quote):
    value = "Research AND Development OR Sales != Marketing"
    assert evaluate_condition(f"{quote}{value}{quote} == {quote}{value}{quote}", {}, [])
    assert not evaluate_condition(f"{quote}{value}{quote} == {quote}Other{quote}", {}, [])


@pytest.mark.parametrize("joiner", ["AND", "OR"])
def test_compound_conditions_still_use_real_operators(joiner):
    cells = {"company": {"value": "Research AND Development"}, "score": {"value": 50}}
    first = '{company} == "Research AND Development"'
    assert evaluate_condition(f"{first} {joiner} {{score}} >= 50", cells, [])
    assert evaluate_condition(f"{first} {joiner} {{score}} > 50", cells, []) is (joiner == "OR")
    assert not evaluate_condition('{company} == "Other" OR {score} > 50', cells, [])


@pytest.mark.parametrize("op, expected", [
    ("!=", True), (">=", True), ("<=", False),
    ("==", False), (">", True), ("<", False),
])
def test_numeric_comparison_operators(op, expected):
    assert evaluate_condition(f"{{score}} {op} 50", {"score": {"value": 75}}, []) is expected


def test_contains_ignores_operator_text_inside_values():
    cells = {"company": {"value": "Sales OR Marketing != Finance contains Growth"}}
    assert evaluate_condition('{company} CoNtAiNs "MARKETING != FINANCE"', cells, [])
    assert not evaluate_condition('{company} contains "Research AND Development"', cells, [])
    assert evaluate_condition('"a contains b" contains "contains"', {}, [])


@pytest.mark.parametrize("value", ["Research AND Development", 'He said "ready OR done"'])
def test_empty_functions_keep_cell_contents_as_data(value):
    cells = {"company": {"value": value}}
    assert evaluate_condition("not_empty({company})", cells, [])
    assert not evaluate_condition("is_empty({company})", cells, [])


def test_operator_text_inside_column_name_is_not_syntax():
    cells = {"company": {"value": "Acme"}}
    columns = [{"id": "company", "name": "Sales OR Marketing != Finance"}]
    assert evaluate_condition('{Sales OR Marketing != Finance} == "Acme"', cells, columns)


def test_unquoted_apostrophes_keep_legacy_string_comparisons():
    assert evaluate_condition("O'Reilly == O'Reilly", {}, [])
    assert not evaluate_condition("O'Reilly == Other", {}, [])


def test_column_lookup_and_empty_values_are_preserved():
    cells = {"company": {"value": "Acme"}, "score": {"value": None}}
    columns = [{"id": "company", "name": "Company Name"}]
    assert evaluate_condition('{COMPANY NAME} == "Acme"', cells, columns)
    assert evaluate_condition('{missing} == "" AND is_empty({score})', cells, columns)
    assert not evaluate_condition("not_empty({missing})", cells, columns)


@pytest.mark.parametrize("expression, expected", [
    ("", True), ("always", True), ("true", True), ("never", False),
    ("false", False), ("unknown expression", False),
    ('"literal AND text"', False), ('"literal != text"', False),
])
def test_shortcuts_and_unparseable_conditions(expression, expected):
    assert evaluate_condition(expression, {}, []) is expected
