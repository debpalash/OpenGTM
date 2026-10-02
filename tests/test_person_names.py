"""Name comparison used to decide whether two records can be one person."""

import pytest

from apps.api.services.person_names import name_tokens, names_compatible


@pytest.mark.parametrize("a, b", [
    ("Alice Smith", "alice smith"),
    ("Alice Smith", "Smith, Alice"),
    ("Alice", "Alice Smith"),                 # a short name inside a longer one
    ("A. Smith", "Alice Smith"),              # an initial matches what it abbreviates
    ("Dr. Alice Smith", "Alice Smith"),
    ("José Núñez", "Jose Nunez"),             # accents do not matter
    ("Alice Smith", ""),                      # a blank name carries no evidence
    ("", ""),
    ("Alice Smith", "(unknown)"),
    ("Alice Smith", "N/A"),
])
def test_compatible_names(a, b):
    assert names_compatible(a, b) and names_compatible(b, a)


@pytest.mark.parametrize("a, b", [
    ("Synthetic Alice", "Synthetic Bob"),     # a shared word is not agreement
    ("Alice Smith", "Alice Jones"),
    ("Alice Smith", "Bob Smith"),
    ("A. Smith", "Bob Smith"),
    ("J. Smith", "K. Smith"),
])
def test_incompatible_names(a, b):
    assert not names_compatible(a, b) and not names_compatible(b, a)


def test_tokens_drop_placeholders_and_punctuation():
    assert name_tokens("  Alice  O'Neil-Smith ") == ("alice", "o", "neil", "smith")
    assert name_tokens("N/A") == () and name_tokens(None) == ()
