"""
Lead dedup: normalizer + 3-pass match + best-record merge.
See docs/research/clay-alternatives-ingestion-catalog.md.
"""
from apps.api.services.leadgen.dedup import (
    normalize_company, normalize_domain, normalize_email, normalize_phone,
    completeness_score, dedupe,
)


def test_normalize_company():
    assert normalize_company("Acme Robotics, Inc.") == "acmerobotics"
    assert normalize_company("Acme Technologies LLC") == "acme"
    assert normalize_company("Café Group GmbH") == "cafe"
    assert normalize_company("Acme") == normalize_company("ACME  ")

def test_normalize_domain():
    assert normalize_domain("https://www.acme.com/about") == "acme.com"
    # multi-part public suffix: registrable domain is eTLD+1, NOT the bare suffix
    assert normalize_domain("acme.co.uk") == "acme.co.uk"
    assert normalize_domain("https://shop.acme.com.au/x") == "acme.com.au"
    assert normalize_domain("foo.co.in:8080") == "foo.co.in"
    # distinct companies under the same ccSLD must NOT collapse
    assert normalize_domain("foo.co.uk") != normalize_domain("bar.co.uk")


def test_normalize_email_and_phone():
    assert normalize_email("  Info@Acme.COM ") == "info@acme.com"
    assert normalize_email("not-an-email") == ""
    assert normalize_phone("+1 (555) 123-4567") == "5551234567"
    assert normalize_phone("123") == ""  # too short

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


def test_dedupe_by_exact_email():
    # same email, different (missing) company names → still one company
    recs = [
        {"company": "Acme", "email": "contact@acme.io"},
        {"company": "Acme Worldwide", "email": "contact@acme.io", "phone": "555-9000"},
    ]
    out, stats = dedupe(recs)
    assert stats["output"] == 1
    assert stats["merged_by_email"] == 1
    assert out[0]["phone"] == "555-9000"


def test_dedupe_transitive_across_keys():
    # A~B by email, B~C by domain ⇒ all one cluster
    recs = [
        {"company": "Acme", "email": "hi@acme.io"},
        {"company": "Acme Labs", "email": "hi@acme.io", "website": "acme.io"},
        {"company": "Acme Labs Inc", "website": "https://acme.io/contact"},
    ]
    out, stats = dedupe(recs)
    assert stats["output"] == 1


def test_dedupe_corroborated_lowband_merges_same_city():
    # ~88% name match (low band), different domains: merges ONLY because city corroborates
    recs = [
        {"company": "Acme Robotics", "website": "a.com", "city": "Bangalore"},
        {"company": "Acme Robotix", "website": "b.com", "city": "Bangalore"},
    ]
    out, stats = dedupe(recs)
    assert stats["output"] == 1
    assert stats["merged_by_fuzzy"] == 1


def test_dedupe_no_false_merge_without_corroboration():
    # same ~88% name but DIFFERENT cities and no shared signal → stay apart (no false merge)
    recs = [
        {"company": "Acme Robotics", "website": "a.com", "city": "Bangalore"},
        {"company": "Acme Robotix", "website": "b.com", "city": "Mumbai"},
    ]
    out, stats = dedupe(recs)
    assert stats["output"] == 2


def test_dedupe_free_email_domain_not_a_signal():
    # two different companies that both happen to use gmail must NOT merge
    recs = [
        {"company": "Alpha Traders", "email": "alpha.traders@gmail.com", "city": "Pune"},
        {"company": "Beta Exports", "email": "beta.exports@gmail.com", "city": "Pune"},
    ]
    out, stats = dedupe(recs)
    assert stats["output"] == 2
