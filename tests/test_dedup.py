"""
Lead dedup: normalizer + 3-pass match + best-record merge.
See docs/clay-alternatives-ingestion-catalog.md.
"""
from apps.api.services.leadgen.dedup import (
    normalize_company, normalize_domain, completeness_score, dedupe,
)


def test_normalize_company():
    assert normalize_company("Acme Robotics, Inc.") == "acmerobotics"
    assert normalize_company("Acme Technologies LLC") == "acme"
    assert normalize_company("Café Group GmbH") == "cafe"
    assert normalize_company("Acme") == normalize_company("ACME  ")

def test_normalize_domain():
    assert normalize_domain("https://www.acme.com/about") == "acme.com"
    assert normalize_domain("acme.co.uk") == "co.uk"  # last-2 heuristic (acceptable)

def test_completeness_score():
    a = {"company": "X", "email": "a@x.com", "phone": "123"}
    b = {"company": "X"}
    assert completeness_score(a) > completeness_score(b)

def test_dedupe_by_domain_keeps_most_complete_and_merges():
    recs = [
        {"company": "Acme Inc", "website": "acme.com", "email": "info@acme.com"},
        {"company": "Acme", "website": "https://www.acme.com", "phone": "+1 555 1234"},
    ]
    out, stats = dedupe(recs)
    assert stats["output"] == 1
    assert stats["merged_by_domain"] == 1
    merged = out[0]
    # winner had email; field-filled the phone from the other
    assert merged["email"] == "info@acme.com"
    assert merged["phone"] == "+1 555 1234"

def test_dedupe_by_normalized_name_no_domain():
    recs = [
        {"company": "Acme Technologies LLC"},
        {"company": "Acme Technologies, Inc."},
    ]
    out, stats = dedupe(recs)
    assert stats["output"] == 1
    assert stats["merged_by_name"] == 1

def test_dedupe_fuzzy_name():
    recs = [
        {"company": "International Business Machines", "website": "ibm.com"},
        {"company": "Internationl Business Machne", "website": "ibm-typo.com"},  # typo, diff domain
    ]
    out, stats = dedupe(recs, fuzzy_threshold=85.0)
    # fuzzy collapses the misspelling even across different domains
    assert stats["output"] == 1
    assert stats["merged_by_fuzzy"] == 1

def test_dedupe_keeps_distinct():
    recs = [
        {"company": "Acme", "website": "acme.com"},
        {"company": "Globex", "website": "globex.com"},
    ]
    out, stats = dedupe(recs)
    assert stats["output"] == 2

def test_dedupe_empty_and_unkeyed():
    out, stats = dedupe([])
    assert stats["output"] == 0
    out2, stats2 = dedupe([{"email": "x@y.com"}])  # no company/domain
    assert stats2["output"] == 1
