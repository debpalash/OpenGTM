import pytest

from apps.api.services.workbook.formula_column import FormulaError, evaluate_formula


@pytest.mark.parametrize(
    "expression, expected",
    [
        ('"Hello {Name}"', "Hello {Name}"),
        ("'{Name}'", "{Name}"),
        ('concat("{Name}: ", {Name})', "{Name}: Rudy"),
        ('"""Line one\n{Name}""" + {Name}', "Line one\n{Name}Rudy"),
        ('r"{Name}\\folder"', "{Name}\\folder"),
        ('"escaped \\"{Name}\\""', 'escaped "{Name}"'),
        ('"é {Name}" + {Name}', "é {Name}Rudy"),
        ('replace({Name}, "Rudy", "{Name}")', "{Name}"),
    ],
)
def test_quoted_braces_are_literal_text(expression, expected):
    assert evaluate_formula(expression, {"Name": "Rudy"}) == expected


def test_reference_names_can_contain_a_quote():
    assert (
        evaluate_formula(
            "{Owner's Name} + \"{Owner's Name}\"", {"Owner's Name": "Rudy"}
        )
        == "Rudy{Owner's Name}"
    )


def test_unquoted_references_still_resolve_case_insensitively():
    assert evaluate_formula("upper({name}) + { Missing }", {"Name": "Rudy"}) == "RUDY"


def test_unclosed_quoted_literal_remains_a_formula_error():
    with pytest.raises(FormulaError, match="syntax error"):
        evaluate_formula('"broken {Name}', {"Name": "Rudy"})
