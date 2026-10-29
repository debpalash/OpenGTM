"""Generate people_search config vectors from the REAL Python validate_people_search_config.

Run from legacy/python:  PYTHONPATH=. uv run python ../../apps/backend-go/internal/runplan/testdata/gen_peoplesearch_vectors.py
Writes people_search_vectors.json next to this file. Only the inputs are hand-written; every expected value (the
normalised config, or the ValueError text) is Python's answer. Never hand-edit the output.
"""
import json
import pathlib

from apps.api.services.workbook.people_search import validate_people_search_config

HERE = pathlib.Path(__file__).resolve().parent

CASES = [
    ("empty", {}),
    ("none_cfg_like_empty", {"companies": None, "from_column": None}),
    ("companies_only", {"companies": ["Acme"]}),
    ("from_column_only", {"from_column": "company"}),
    ("from_column_whitespace_only", {"from_column": "   "}),
    ("companies_blank_entries", {"companies": ["", "  ", "Acme ", None, "stripe.com"]}),
    ("companies_only_blank", {"companies": ["", "   "]}),
    ("companies_numbers_and_bool", {"companies": [1, 2.5, True, False, 0, "x"]}),
    ("companies_string_iterates_chars", {"companies": "Ab"}),
    ("titles_trimmed", {"companies": ["A"], "titles": [" CTO ", "", "Founder", None, "  "]}),
    ("seniority_geo_trimmed", {"companies": ["A"], "seniority": "  senior ", "geo": " Berlin\t"}),
    ("seniority_falsy_zero", {"companies": ["A"], "seniority": 0, "geo": False}),
    ("seniority_number", {"companies": ["A"], "seniority": 5, "geo": 1.5}),
    ("unicode_strip", {"companies": [" Acme "], "from_column": "　col　"}),
    ("both_companies_and_from_column", {"companies": ["A"], "from_column": "c"}),
    ("default_ints", {"companies": ["A"]}),
    ("max_per_company_5", {"companies": ["A"], "max_per_company": 5}),
    ("max_per_company_cap", {"companies": ["A"], "max_per_company": 26}),
    ("max_per_company_huge", {"companies": ["A"], "max_per_company": 10 ** 30}),
    ("max_per_company_zero", {"companies": ["A"], "max_per_company": 0}),
    ("max_per_company_negative", {"companies": ["A"], "max_per_company": -4}),
    ("max_per_company_empty_string", {"companies": ["A"], "max_per_company": ""}),
    ("max_per_company_null", {"companies": ["A"], "max_per_company": None}),
    ("max_per_company_str_int", {"companies": ["A"], "max_per_company": " 7 "}),
    ("max_per_company_str_plus", {"companies": ["A"], "max_per_company": "+8"}),
    ("max_per_company_str_underscore", {"companies": ["A"], "max_per_company": "1_2"}),
    ("max_per_company_str_bad_underscore", {"companies": ["A"], "max_per_company": "1__2"}),
    ("max_per_company_str_float", {"companies": ["A"], "max_per_company": "7.5"}),
    ("max_per_company_str_word", {"companies": ["A"], "max_per_company": "abc"}),
    ("max_per_company_whitespace_str", {"companies": ["A"], "max_per_company": "  "}),
    ("max_per_company_float", {"companies": ["A"], "max_per_company": 7.9}),
    ("max_per_company_float_negative", {"companies": ["A"], "max_per_company": -3.2}),
    ("max_per_company_float_huge", {"companies": ["A"], "max_per_company": 1e30}),
    ("max_per_company_true", {"companies": ["A"], "max_per_company": True}),
    ("max_per_company_false", {"companies": ["A"], "max_per_company": False}),
    ("max_per_company_list", {"companies": ["A"], "max_per_company": [3]}),
    ("max_per_company_dict", {"companies": ["A"], "max_per_company": {"a": 1}}),
    ("max_searches_default", {"companies": ["A"]}),
    ("max_searches_50", {"companies": ["A"], "max_searches": 50}),
    ("max_searches_cap", {"companies": ["A"], "max_searches": 101}),
    ("max_searches_zero", {"companies": ["A"], "max_searches": 0}),
    ("max_searches_bad", {"companies": ["A"], "max_searches": "many"}),
    ("both_ints_bad_first_wins", {"companies": ["A"], "max_per_company": "x", "max_searches": "y"}),
    ("missing_both_beats_bad_int", {"max_per_company": "x"}),
    ("full", {"companies": ["Acme Corp", "stripe.com"], "from_column": "company", "titles": ["CTO", "Founder"],
              "seniority": "senior", "geo": "Berlin", "max_per_company": 10, "max_searches": 100}),
    ("unknown_keys_ignored", {"companies": ["A"], "extra": 1, "kind": "people_search"}),
]


def run(cfg):
    try:
        return {"value": validate_people_search_config(cfg)}
    except Exception as exc:  # noqa: BLE001 - the exception type and text are the vector
        return {"error": type(exc).__name__, "message": str(exc)}


out = [{"name": n, "input": c, **run(c)} for n, c in CASES]
# 10**30 and floats do not survive json round trips as ints; keep them as tagged raw JSON text.
text = json.dumps({"cases": out}, indent=1, ensure_ascii=False, allow_nan=False)
(HERE / "people_search_vectors.json").write_text(text + "\n", encoding="utf-8")
print(f"wrote people_search_vectors.json: {len(out)} cases")
