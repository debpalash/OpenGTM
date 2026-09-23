"""LinkedIn search-result parsing: regressions seen in a live PayPal run."""

import pytest

from apps.api.services.leadgen.enrichment.providers.crosslinked import (
    CrossLinkedProvider, _parse_linkedin_name, _parse_linkedin_title,
)


@pytest.mark.parametrize("title, name", [
    ("Jane Smith - Head of Partnerships at PayPal | LinkedIn", "Jane Smith"),
    ("Setu A. - Head of Program Mgmt Office - PayPal | LinkedIn", "Setu A."),
    ("Mary-Kate O'Neil, CTO - Acme", "Mary-Kate O'Neil"),
    ("VP - PayPal | LinkedIn", ""),            # a role, not a person
    ("Director of Partnerships - PayPal", ""),
    ("Madonna - Singer | LinkedIn", ""),       # single words are not accepted as names
])
def test_names_are_people_not_roles(title, name):
    assert _parse_linkedin_name(title) == name


@pytest.mark.parametrize("title, job", [
    ("Jane Smith - Head of Partnerships at PayPal | LinkedIn", "Head of Partnerships"),
    ("Jane Smith - LinkedIn", ""),
    ("Jane Smith – LinkedIn India", ""),
])
def test_linkedin_is_never_a_job_title(title, job):
    assert _parse_linkedin_title(title) == job


def test_body_keyword_fallback_matches_whole_words_only():
    person = CrossLinkedProvider()._parse_result({
        "href": "https://www.linkedin.com/in/john-smith",
        "title": "John Smith | LinkedIn",
        "body": "Director, Global Partnerships at PayPal. Previously at Visa.",
    })
    assert person is not None
    assert person["title"] == "Director"   # was "ctor" (CTO matched inside Director)
