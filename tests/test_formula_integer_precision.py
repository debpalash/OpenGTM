import pytest

from apps.api.services.workbook.formula_column import evaluate_formula


@pytest.mark.parametrize('value', ['9007199254740993', '-9007199254740993', '123456789012345678901234567890'])
def test_int_preserves_integer_identifier_digits(value):
    assert evaluate_formula('int({identifier})', {'identifier': value}) == int(value)


@pytest.mark.parametrize('value, expected', [('3.75', 3), ('-3.75', -3), ('1e3', 1000), ('', 0), ('None', 0), (' 42 ', 42)])
def test_int_preserves_existing_decimal_scientific_and_empty_inputs(value, expected):
    assert evaluate_formula('int({value})', {'value': value}) == expected


def test_int_preserves_large_numeric_integer_values():
    value = 9007199254740993
    assert evaluate_formula('int({identifier})', {'identifier': value}) == value
