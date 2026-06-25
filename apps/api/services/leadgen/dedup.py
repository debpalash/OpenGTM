"""
Lead de-duplication — normalize + multi-key union + best-record merge.

Ported from nimajnebrevilo/GTM-Engine (src/dedup/{matcher,normalizer}.ts) +
Revgrowth1/claude-code-skills (tam-map/dedup_engine.py). See
docs/clay-alternatives-ingestion-catalog.md (top-10, dedup).

Operates on plain lead dicts (workbook rows / leads DB / CSV import all use
dicts). Records are clustered with a union-find over several exact blocking
keys — strongest signal first — so matches are transitive (A~B by email,
B~C by domain ⇒ A,B,C are one cluster):
  1. exact email
  2. exact registrable domain
  3. normalized company name (legal-suffix + diacritic stripped)
Then a final fuzzy company-name pass merges near-identical names. Fuzzy uses
two confidence bands to avoid false merges: a high band merges on the name
alone (typos), and a lowered band merges only when a second signal corroborates
(same city / business email-domain / phone). Within each cluster the most
COMPLETE record wins and the others field-fill it.
"""

import logging
import re
import unicodedata
from collections import defaultdict
from typing import Any, Dict, List, Tuple

logger = logging.getLogger("leadgen.dedup")

try:
    from rapidfuzz import fuzz

    def _token_sort_ratio(a: str, b: str) -> float:
        return fuzz.token_sort_ratio(a, b)
except Exception:  # pragma: no cover - rapidfuzz is a declared dep; stdlib fallback
    import difflib

    logger.warning("rapidfuzz unavailable — falling back to difflib for fuzzy dedup")

    def _token_sort_ratio(a: str, b: str) -> float:
        # token_sort: compare with tokens sorted so word order doesn't matter
        sa = " ".join(sorted(a.split()))
        sb = " ".join(sorted(b.split()))
        return difflib.SequenceMatcher(None, sa, sb).ratio() * 100.0


_LEGAL_SUFFIX_RE = re.compile(
    r"\b(inc|incorporated|llc|l\.l\.c|ltd|limited|corp|corporation|co|company|gmbh|"
    r"ag|sarl|sas|sa|bv|plc|pvt|pte|llp|group|holdings?|technologies|technology|"
    r"solutions|services|systems|labs?)\b\.?",
    re.IGNORECASE,
)

# Second-level public suffixes (registrable domain is the 3rd-from-last label).
# Without this, "foo.co.uk" and "bar.co.uk" both normalize to "co.uk" and FALSE
# MERGE into one company. Covers the common ccSLDs; unknown ones fall back to
# the last two labels (correct for plain gTLDs like .com/.io/.ai).
_MULTI_PART_SUFFIXES = frozenset({
    "co.uk", "org.uk", "ac.uk", "gov.uk", "me.uk", "ltd.uk", "plc.uk", "net.uk",
    "com.au", "net.au", "org.au", "edu.au", "gov.au", "id.au",
    "co.in", "net.in", "org.in", "gen.in", "firm.in", "ind.in", "ac.in", "gov.in", "edu.in",
    "co.nz", "net.nz", "org.nz", "govt.nz", "ac.nz",
    "co.za", "org.za", "web.za", "net.za",
    "com.br", "net.br", "org.br", "gov.br",
    "com.sg", "edu.sg", "gov.sg", "org.sg", "net.sg",
    "com.cn", "net.cn", "org.cn", "gov.cn",
    "co.jp", "or.jp", "ne.jp", "go.jp", "ac.jp",
    "com.mx", "com.tr", "com.tw", "com.hk", "com.my", "com.ph",
    "co.id", "co.kr", "co.th", "com.ar", "com.co", "co.il", "com.sa",
    "com.ua", "com.pk", "com.ng", "com.eg", "com.bd", "com.vn",
})

# Free / personal email providers — their domain does NOT identify a company,
# so it must not be used as a dedup or corroboration signal.
_FREE_EMAIL_DOMAINS = frozenset({
    "gmail.com", "googlemail.com", "yahoo.com", "yahoo.co.in", "yahoo.co.uk",
    "hotmail.com", "outlook.com", "live.com", "msn.com", "icloud.com", "me.com",
    "aol.com", "proton.me", "protonmail.com", "gmx.com", "zoho.com", "mail.com",
    "ymail.com", "rediffmail.com", "yandex.com", "qq.com", "163.com", "126.com",
})


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
    """Extract the registrable domain (eTLD+1) from a URL/host.

    Handles multi-part public suffixes so e.g. acme.co.uk → acme.co.uk (not
    co.uk). Plain gTLDs fall back to the last two labels.
    """
    if not website:
        return ""
    d = strip_diacritics(website).strip().lower()
    for p in ("https://", "http://", "www."):
        d = d.removeprefix(p)
    d = d.split("/")[0].split("?")[0].split("#")[0]
    d = d.split("@")[-1]  # tolerate an email being passed in
    d = d.split(":")[0]   # strip :port
    parts = [x for x in d.split(".") if x]
    if len(parts) >= 3 and ".".join(parts[-2:]) in _MULTI_PART_SUFFIXES:
        return ".".join(parts[-3:])
    return ".".join(parts[-2:]) if len(parts) >= 2 else d


def normalize_email(email: str) -> str:
    """Lowercased, trimmed email — the exact-match dedup key."""
    if not email:
        return ""
    e = strip_diacritics(email).strip().lower()
    return e if "@" in e and "." in e.split("@")[-1] else ""


def _email_domain(email: str) -> str:
    e = normalize_email(email)
    if not e:
        return ""
    dom = e.split("@")[-1]
    return "" if dom in _FREE_EMAIL_DOMAINS else dom


def normalize_phone(phone: str) -> str:
    """Digits only, last 10 (drops country code / formatting). '' if too short."""
    if not phone:
        return ""
    digits = re.sub(r"\D+", "", str(phone))
    return digits[-10:] if len(digits) >= 7 else ""


# Fields considered for completeness scoring (presence = +1).
_VALUE_FIELDS = (
    "company", "website", "email", "phone", "linkedin_url", "city",
    "contact_person", "contact_title", "company_size", "description",
)

_EMPTY = (None, "", "N/A", "nan", [])


def completeness_score(record: Dict[str, Any]) -> int:
    score = 0
    for f in _VALUE_FIELDS:
        if record.get(f) not in _EMPTY:
            score += 1
    return score


def _domain_of(record: Dict[str, Any]) -> str:
    return normalize_domain(record.get("website") or record.get("domain") or "")


def _norm_city(record: Dict[str, Any]) -> str:
    c = record.get("city") or ""
    return re.sub(r"[^a-z0-9]+", "", strip_diacritics(str(c)).lower())


def _merge_into(primary: Dict[str, Any], other: Dict[str, Any]) -> Dict[str, Any]:
    """Field-fill: keep primary's values; take other's only where primary is empty."""
    for k, v in other.items():
        if v in _EMPTY:
            continue
        if primary.get(k) in _EMPTY:
            primary[k] = v
    return primary


class _DSU:
    """Union-find for clustering records by index."""

    def __init__(self, n: int):
        self.parent = list(range(n))

    def find(self, x: int) -> int:
        root = x
        while self.parent[root] != root:
            root = self.parent[root]
        while self.parent[x] != root:  # path compression
            self.parent[x], x = root, self.parent[x]
        return root

    def union(self, a: int, b: int) -> bool:
        ra, rb = self.find(a), self.find(b)
        if ra == rb:
            return False
        self.parent[rb] = ra
        return True


def _corroborated(a: Dict[str, Any], b: Dict[str, Any]) -> bool:
    """A second non-name signal that two records are the same company."""
    ca, cb = _norm_city(a), _norm_city(b)
    if ca and ca == cb:
        return True
    ea, eb = _email_domain(a.get("email") or ""), _email_domain(b.get("email") or "")
    if ea and ea == eb:
        return True
    pa, pb = normalize_phone(a.get("phone") or ""), normalize_phone(b.get("phone") or "")
    if pa and pa == pb:
        return True
    return False


def dedupe(
    records: List[Dict[str, Any]],
    *,
    fuzzy_threshold: float = 92.0,
) -> Tuple[List[Dict[str, Any]], Dict[str, int]]:
    """De-duplicate a list of lead dicts. Returns (deduped_records, stats).

    stats: {input, output, merged_by_email, merged_by_domain, merged_by_name,
    merged_by_fuzzy}. `fuzzy_threshold` is the high-confidence band (merge on
    name alone); a lower corroborated band (threshold-8, floored at 82) merges
    only when city/business-email-domain/phone also matches.
    """
    stats = {"input": len(records), "output": 0, "merged_by_email": 0,
             "merged_by_domain": 0, "merged_by_name": 0, "merged_by_fuzzy": 0}
    n = len(records)
    if n == 0:
        return [], stats

    dsu = _DSU(n)

    # ── Exact blocking, strongest signal first (email → domain → name) ──
    def _block(keyfn, stat_key: str) -> None:
        groups: Dict[str, List[int]] = defaultdict(list)
        for i, rec in enumerate(records):
            k = keyfn(rec)
            if k:
                groups[k].append(i)
        for idxs in groups.values():
            anchor = idxs[0]
            for j in idxs[1:]:
                if dsu.union(anchor, j):
                    stats[stat_key] += 1

    _block(lambda r: normalize_email(r.get("email") or ""), "merged_by_email")
    _block(_domain_of, "merged_by_domain")
    _block(lambda r: normalize_company(r.get("company") or ""), "merged_by_name")

    # collapse each exact cluster → most-complete primary, field-filled
    clusters: Dict[int, List[int]] = defaultdict(list)
    for i in range(n):
        clusters[dsu.find(i)].append(i)

    prim_idx: List[int] = []
    primary_of: Dict[int, Dict[str, Any]] = {}
    for root, members in clusters.items():
        members.sort(key=lambda i: completeness_score(records[i]), reverse=True)
        primary = dict(records[members[0]])
        for j in members[1:]:
            _merge_into(primary, records[j])
        primary_of[root] = primary
        prim_idx.append(root)

    # ── Fuzzy company-name pass over surviving primaries ──
    high = float(fuzzy_threshold)
    low = max(82.0, high - 8.0)
    tokens = {r: normalize_company_tokens(primary_of[r].get("company") or "") for r in prim_idx}

    # Candidate pairs via cheap blocking: shared name-prefix (catches typos),
    # shared sorted-token prefix (catches word-order), and shared corroboration
    # keys (catches lowered-band matches). Avoids O(n^2) on large inputs.
    blocks: Dict[str, List[int]] = defaultdict(list)
    for r in prim_idx:
        tok = tokens[r]
        if not tok:
            continue
        flat = tok.replace(" ", "")
        if len(flat) >= 4:
            blocks[f"p:{flat[:6]}"].append(r)
            blocks[f"s:{''.join(sorted(tok.split()))[:6]}"].append(r)
        prim = primary_of[r]
        cy, ed, ph = _norm_city(prim), _email_domain(prim.get("email") or ""), normalize_phone(prim.get("phone") or "")
        if cy:
            blocks[f"c:{cy}"].append(r)
        if ed:
            blocks[f"e:{ed}"].append(r)
        if ph:
            blocks[f"h:{ph}"].append(r)

    seen_pairs = set()
    fuzzy = _DSU(n)
    for members in blocks.values():
        if len(members) < 2:
            continue
        for x in range(len(members)):
            for y in range(x + 1, len(members)):
                a, b = members[x], members[y]
                pair = (a, b) if a < b else (b, a)
                if pair in seen_pairs:
                    continue
                seen_pairs.add(pair)
                ta, tb = tokens[a], tokens[b]
                if not ta or not tb:
                    continue
                if fuzzy.find(a) == fuzzy.find(b):
                    continue
                score = _token_sort_ratio(ta, tb)
                if score >= high or (score >= low and _corroborated(primary_of[a], primary_of[b])):
                    if fuzzy.union(a, b):
                        stats["merged_by_fuzzy"] += 1

    # collapse fuzzy clusters over the primaries
    fuzzy_clusters: Dict[int, List[int]] = defaultdict(list)
    for r in prim_idx:
        fuzzy_clusters[fuzzy.find(r)].append(r)

    survivors: List[Dict[str, Any]] = []
    for root, members in fuzzy_clusters.items():
        members.sort(key=lambda r: completeness_score(primary_of[r]), reverse=True)
        primary = primary_of[members[0]]
        for r in members[1:]:
            _merge_into(primary, primary_of[r])
        survivors.append(primary)

    stats["output"] = len(survivors)
    return survivors, stats
