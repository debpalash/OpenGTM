"""Normalization must never turn an overlong number into a different contact."""

import pytest

from apps.api.services.leadgen.enrichment.normalize import normalize_fields, normalize_phone


@pytest.mark.parametrize("phone", [
    "+1234567890123456",
    "+12345678901234567",
    "+1 (234) 567-890123456",
    "001234567890123456",
    "0012345678901234567",
    "00 1 (234) 567-890123456",
])
def test_overlong_international_number_is_preserved(phone):
    assert normalize_phone(phone) == phone
    assert normalize_fields({"phone": phone, "mobile_phone": phone}) == {
        "phone": phone,
        "mobile_phone": phone,
    }


@pytest.mark.parametrize(("phone", "expected"), [
    ("+123456789012345", "+123456789012345"),
    ("+1 (415) 555-0132", "+14155550132"),
    ("0044 20 7946 0958", "+442079460958"),
    ("00123456789012345", "+123456789012345"),
    ("00 1 (234) 567-89012345", "+123456789012345"),
    ("415.555.0132", "4155550132"),
])
def test_valid_phone_normalization_is_unchanged(phone, expected):
    assert normalize_phone(phone) == expected
    assert normalize_fields({"phone": phone, "mobile_phone": phone, "name": "unchanged"}) == {
        "phone": expected,
        "mobile_phone": expected,
        "name": "unchanged",
    }


@pytest.mark.parametrize("value", [None, 123, True, [], {}, "", "not a phone"])
def test_unparseable_and_non_string_values_are_preserved(value):
    assert normalize_phone(value) == value
    assert normalize_fields({"phone": value, "mobile_phone": value}) == {
        "phone": value,
        "mobile_phone": value,
    }
