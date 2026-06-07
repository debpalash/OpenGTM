"""
Lead de-duplication — normalize + 3-pass match + best-record merge.

Ported from nimajnebrevilo/GTM-Engine (src/dedup/{matcher,normalizer}.ts) +
Revgrowth1/claude-code-skills (tam-map/dedup_engine.py). See
docs/clay-alternatives-ingestion-catalog.md (top-10, dedup).

Operates on plain lead dicts (workbook rows / leads DB / CSV import all use
dicts). Three passes, strongest signal first:
  1. exact domain
  2. normalized company name (legal-suffix + diacritic stripped)
  3. fuzzy company name (rapidfuzz token ratio >= threshold)
Within each duplicate cluster the most COMPLETE record wins and the others
field-fill it (keep a value the winner is missing).
"""

import logging
import re
import unicodedata
from typing import Any, Dict, List, Optional, Tuple

logger = logging.getLogger("leadgen.dedup")

try:
    from rapidfuzz import fuzz
    _HAS_RAPIDFUZZ = True
except Exception:  # pragma: no cover
    _HAS_RAPIDFUZZ = False

_LEGAL_SUFFIX_RE = re.compile(
    r"\b(inc|incorporated|llc|l\.l\.c|ltd|limited|corp|corporation|co|company|gmbh|"
    r"ag|sarl|sas|sa|bv|plc|pvt|pte|llp|group|holdings?|technologies|technology|"
    r"solutions|services|systems|labs?)\b\.?",
    re.IGNORECASE,
)


def strip_diacritics(s: str) -> str:
    return "".join(c for c in unicodedata.normalize("NFKD", s) if not unicodedata.combining(c))


def normalize_company(name: str) -> str:
    """Lowercase, strip diacritics + legal suffixes + punctuation → comparable key."""
    if not name:
        return ""
    n = strip_diacritics(name).lower()
    n = _LEGAL_SUFFIX_RE.sub(" ", n)
    n = re.sub(r"[^a-z0-9]+", "", n)
    return n


def normalize_company_tokens(name: str) -> str:
    """Like normalize_company but space-separated (for token-based fuzzy match)."""
    if not name:
        return ""
    n = strip_diacritics(name).lower()
    n = _LEGAL_SUFFIX_RE.sub(" ", n)
    n = re.sub(r"[^a-z0-9]+", " ", n)
    return re.sub(r"\s+", " ", n).strip()


def normalize_domain(website: str) -> str:
    if not website:
        return ""
    d = strip_diacritics(website).strip().lower()
    for p in ("https://", "http://", "www."):
        d = d.removeprefix(p)
    d = d.split("/")[0].split("?")[0]
    parts = [x for x in d.split(".") if x]
    return ".".join(parts[-2:]) if len(parts) >= 2 else d


# Fields considered for completeness scoring (presence = +1).
_VALUE_FIELDS = (
    "company", "website", "email", "phone", "linkedin_url", "city",
    "contact_person", "contact_title", "company_size", "description",
)


def completeness_score(record: Dict[str, Any]) -> int:
    score = 0
    for f in _VALUE_FIELDS:
        v = record.get(f)
        if v not in (None, "", "N/A", "nan", []):
            score += 1
    return score


def _domain_of(record: Dict[str, Any]) -> str:
    return normalize_domain(record.get("website") or record.get("domain") or "")


def _merge_into(primary: Dict[str, Any], other: Dict[str, Any]) -> Dict[str, Any]:
    """Field-fill: keep primary's values; take other's only where primary is empty."""
    for k, v in other.items():
        if v in (None, "", "N/A", "nan", []):
            continue
        cur = primary.get(k)
        if cur in (None, "", "N/A", "nan", []):
            primary[k] = v
    return primary


def dedupe(
    records: List[Dict[str, Any]],
    *,
    fuzzy_threshold: float = 92.0,
) -> Tuple[List[Dict[str, Any]], Dict[str, int]]:
    """De-duplicate a list of lead dicts. Returns (deduped_records, stats).

    stats: {input, output, merged_by_domain, merged_by_name, merged_by_fuzzy}.
    """
    stats = {"input": len(records), "output": 0,
             "merged_by_domain": 0, "merged_by_name": 0, "merged_by_fuzzy": 0}

    # ── Pass 1 + 2: bucket by domain, else by normalized company name ──
    buckets: Dict[str, List[Dict[str, Any]]] = {}
    name_index: Dict[str, str] = {}   # normalized name → bucket key
    unkeyed: List[Dict[str, Any]] = []

    for rec in records:
        dom = _domain_of(rec)
        nname = normalize_company(rec.get("company") or "")
        if dom:
            key = f"d:{dom}"
            if key in buckets:
                stats["merged_by_domain"] += 1
            buckets.setdefault(key, []).append(rec)
            if nname:
                name_index.setdefault(nname, key)
        elif nname:
            key = name_index.get(nname) or f"n:{nname}"
            if key in buckets:
                stats["merged_by_name"] += 1
            buckets.setdefault(key, []).append(rec)
            name_index.setdefault(nname, key)
        else:
            unkeyed.append(rec)

    # collapse each bucket to its most-complete record, field-filled
    primaries: List[Tuple[str, Dict[str, Any]]] = []
    for key, group in buckets.items():
        group.sort(key=completeness_score, reverse=True)
        primary = dict(group[0])
        for other in group[1:]:
            _merge_into(primary, other)
        primaries.append((key, primary))

    # ── Pass 3: fuzzy company-name merge across surviving primaries ──
    survivors: List[Dict[str, Any]] = []
    tokens: List[str] = []
    for _key, prim in primaries:
        tname = normalize_company_tokens(prim.get("company") or "")
        matched = False
        if tname and _HAS_RAPIDFUZZ:
            for i, existing_tok in enumerate(tokens):
                if not existing_tok:
                    continue
                if fuzz.token_sort_ratio(tname, existing_tok) >= fuzzy_threshold:
                    _merge_into(survivors[i], prim)
                    stats["merged_by_fuzzy"] += 1
                    matched = True
                    break
        if not matched:
            survivors.append(prim)
            tokens.append(tname)

    survivors.extend(unkeyed)
    stats["output"] = len(survivors)
    return survivors, stats
