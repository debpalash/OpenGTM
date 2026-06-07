"""
AI-column prompt library (#8): preset shape + lookup + placeholder hygiene.
"""
import re
from apps.api.services.workbook.ai_column_presets import (
    PRESETS, list_presets, get_preset, categories, INSUFFICIENT,
)

_VALID_COL_TYPES = {"ai_formula", "research", "agent"}


def test_presets_nonempty_and_well_formed():
    assert len(PRESETS) >= 5
    ids = [p["id"] for p in PRESETS]
    assert len(ids) == len(set(ids))  # unique ids
    for p in PRESETS:
        assert {"id", "name", "category", "column_type", "prompt", "output_format", "description"} <= set(p)
        assert p["column_type"] in _VALID_COL_TYPES
        assert p["prompt"].strip()

def test_every_prompt_has_fallback_clause():
    # the 5-part structure requires an explicit FALLBACK so a missing-data answer
    # is graceful (sentinel or an explicit instruction)
    for p in PRESETS:
        assert "FALLBACK:" in p["prompt"], p["id"]

def test_filterable_presets_use_sentinel():
    # presets meant for sort/filter should emit a clean sentinel/token
    for pid in ("icp_fit_score", "company_pain", "opening_line", "company_one_liner"):
        assert INSUFFICIENT in get_preset(pid)["prompt"]
    assert "UNKNOWN" in get_preset("seniority_tier")["prompt"]

def test_placeholders_are_known_columns():
    known = {"company", "website", "city", "company_size", "description",
             "contact_person", "contact_title", "email", "phone", "linkedin_url"}
    for p in PRESETS:
        for ph in re.findall(r"\{([^}]+)\}", p["prompt"]):
            assert ph in known, f"{p['id']} uses unknown placeholder {{{ph}}}"

def test_list_and_filter_by_category():
    cats = categories()
    assert "qualification" in cats and "research" in cats
    qual = list_presets("qualification")
    assert qual and all(p["category"] == "qualification" for p in qual)
    assert len(list_presets()) == len(PRESETS)

def test_get_preset():
    assert get_preset("icp_fit_score")["output_format"] == "number"
    assert get_preset("nope") is None
