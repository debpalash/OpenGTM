"""Company-size heuristic — derive an employee band from signals already on a Lead.

Pure, in-memory, zero-network helpers used by the ``company_size_heuristic``
enrichment provider and by ``scoring.py``. NO I/O, NO new paid APIs — every input
is a field the sourcing/enrichment pipeline already populates on the in-memory
:class:`~apps.api.services.leadgen.models.Lead` (job-posting volume, funding
stage, registry decision-makers, technographic footprint, company age).

Two public entry points:

* :func:`normalize_band` — coerce ANY incoming ``company_size`` value (raw int,
  ``"10-50"``, ``"5,000"``, ``"501-1000"``, ``"500+"``) into one of the four
  canonical bands, else ``""``. This fixes a latent scoring bug where non-canonical
  formats (LeadMagic/JSON-LD integers, ad-hoc ranges) silently scored zero.
* :func:`infer_company_size` — when size is unknown, vote a band from the raw
  signals and return a :class:`SizeEstimate` (band + confidence + basis).

The band vocabulary is deliberately the SAME four bands ``scoring.py`` already
checks, so scoring weights and existing filters are untouched.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Optional

# Canonical bands — identical to the strings scoring.py:45 checks. Keep in order.
SIZE_BANDS = ["1-50", "51-200", "201-500", "500+"]

# Bands that earn the company_size_large scoring credit (mirrors scoring.py).
_SCORED_BANDS = ("51-200", "201-500", "500+")

# Basis provenance values written to Lead.company_size_basis.
BASIS_EXACT = "exact"            # from a precise known headcount
BASIS_ESTIMATED_PREFIX = "estimated"  # "estimated:<signal>+<signal>" from the heuristic


def band_from_count(count) -> str:
    """Map a numeric headcount to a canonical band. Returns "" for <=0 / non-numeric."""
    try:
        n = int(count)
    except (TypeError, ValueError):
        return ""
    if n <= 0:
        return ""
    if n <= 50:
        return "1-50"
    if n <= 200:
        return "51-200"
    if n <= 500:
        return "201-500"
    return "500+"


def normalize_band(value) -> str:
    """Coerce any company_size value into a canonical band, else "".

    Examples
    --------
    >>> normalize_band("51-200")      # already canonical
    '51-200'
    >>> normalize_band(120)            # raw int
    '51-200'
    >>> normalize_band("10-50")        # ad-hoc range -> lower bound
    '1-50'
    >>> normalize_band("5,000")        # thousands separator
    '500+'
    >>> normalize_band("501-1000")     # range above 500
    '500+'
    >>> normalize_band("1000+")        # open-ended
    '500+'
    >>> normalize_band("unknown")      # garbage
    ''
    """
    if value is None:
        return ""
    s = str(value).strip()
    if not s:
        return ""
    # Already one of the four canonical bands — return verbatim (no re-derivation,
    # so a valid band can never be downgraded).
    if s in SIZE_BANDS:
        return s
    # A bare negative number is nonsensical for a headcount → reject (a range like
    # "10-50" still parses below; the '-' there is a separator, not a sign).
    if re.fullmatch(r"-\s*\d[\d,]*", s):
        return ""
    # Pull every integer (handles "10-50", "5,000", "501-1000", "1000+", "120").
    nums = re.findall(r"\d[\d,]*", s)
    if not nums:
        return ""
    ints = [int(n.replace(",", "")) for n in nums]
    # For a RANGE use the lower bound (conservative); for a single value use it.
    n = min(ints) if len(ints) > 1 else ints[0]
    return band_from_count(n)


@dataclass
class SizeEstimate:
    """Output of :func:`infer_company_size`."""
    band: str = ""          # canonical band or ""
    confidence: float = 0.0  # [0.3, 0.6] for heuristic; 0.95 for exact; 0.0 when none
    basis: str = ""         # "exact" | "estimated:<signals>" | ""

    @property
    def is_estimated(self) -> bool:
        return self.basis.startswith(BASIS_ESTIMATED_PREFIX)


# ── Signal parsers (defensive: never raise on partial/empty leads) ──────────

def _hiring_vote(lead) -> Optional[tuple]:
    """(band_index, weight, label) from hiring_signals JSON, else None."""
    raw = getattr(lead, "hiring_signals", "") or ""
    if not raw:
        return None
    try:
        data = json.loads(raw) if isinstance(raw, str) else dict(raw)
    except (json.JSONDecodeError, TypeError, ValueError):
        return None
    if not isinstance(data, dict):
        return None
    growth = str(data.get("growth_signal") or "").lower()
    try:
        total = int(data.get("total_jobs") or 0)
    except (TypeError, ValueError):
        total = 0
    # hypergrowth / 10+ open roles ⇒ mid-large; scale down from there.
    if growth == "hypergrowth" or total >= 10:
        return (2, 3, "jobs")   # 201-500
    if growth == "rapid_growth" or total >= 5:
        return (1, 3, "jobs")   # 51-200
    if growth == "growing" or total >= 2:
        return (1, 2, "jobs")   # 51-200 (lean)
    if growth == "hiring" or total >= 1:
        return (0, 2, "jobs")   # 1-50
    return None


def _funding_amount(lead):
    """Best-effort numeric funding amount from any field the lead may carry."""
    for attr in ("funding_amount", "last_funding_amount", "funding"):
        v = getattr(lead, attr, None)
        if v in (None, "", 0):
            continue
        nums = re.findall(r"\d[\d,.]*", str(v))
        if nums:
            try:
                return float(nums[0].replace(",", ""))
            except ValueError:
                continue
    return 0.0


def _funding_vote(lead) -> Optional[tuple]:
    """(band_index, weight, label) from funding stage / amount, else None."""
    amount = _funding_amount(lead)
    if amount >= 50_000_000:
        return (3, 3, "funding")
    if amount >= 10_000_000:
        return (2, 3, "funding")
    if amount >= 1_000_000:
        return (1, 2, "funding")

    stage = str(getattr(lead, "funding_stage", "") or "").lower()
    if not stage:
        return None
    if any(k in stage for k in ("ipo", "public", "series d", "series e", "series f", "series c")):
        return (3, 3, "funding")
    if "series b" in stage:
        return (2, 3, "funding")
    if "series a" in stage:
        return (1, 2, "funding")
    if any(k in stage for k in ("seed", "angel", "pre-seed", "bootstrap", "grant")):
        return (0, 1, "funding")
    return None


def _registry_vote(lead) -> Optional[tuple]:
    """(band_index, weight, label) from registry decision-maker count, else None."""
    raw = getattr(lead, "decision_makers", "") or ""
    if not raw:
        return None
    try:
        people = json.loads(raw) if isinstance(raw, str) else list(raw)
    except (json.JSONDecodeError, TypeError, ValueError):
        return None
    if not isinstance(people, list) or not people:
        return None
    n = len(people)
    # Director/PSC/officer count is a weak footprint proxy (more named officers ⇒
    # larger/older entity) — keep low weight so it only nudges.
    if n >= 8:
        return (2, 2, "registry")
    if n >= 4:
        return (1, 1, "registry")
    return (0, 1, "registry")


def _footprint_vote(lead) -> Optional[tuple]:
    """(band_index, weight, label) — weak tie-breaker from tech + age + description."""
    score = 0
    techs = str(getattr(lead, "technologies", "") or "")
    tech_count = len([t for t in re.split(r"[,;|]", techs) if t.strip()])
    if tech_count >= 10:
        score += 1
    if len(str(getattr(lead, "description", "") or "")) >= 600:
        score += 1
    fy = str(getattr(lead, "founded_year", "") or "")
    m = re.search(r"\d{4}", fy)
    if m:
        age = datetime.now(timezone.utc).year - int(m.group(0))
        if age >= 20:
            score += 1
    if score == 0:
        return None
    # A broad footprint nudges toward 51-200, weakly.
    return (1, 1, "footprint")


def infer_company_size(lead) -> SizeEstimate:
    """Infer an employee band for a lead from signals already on the record.

    Deterministic, zero-network, never raises on partial/empty input. Returns an
    empty :class:`SizeEstimate` (band="") when no signal fires.

    Precedence:
      1. A precise known headcount (``employee_count_exact``) wins outright →
         basis="exact", confidence 0.95.
      2. Otherwise a small weighted vote over hiring volume, funding, registry
         footprint, and technographic/age footprint → basis="estimated:<signals>",
         confidence in [0.3, 0.6] (deliberately below real providers so it loses
         the waterfall whenever a real source fires).
    """
    # 1. Exact known headcount.
    try:
        exact = int(getattr(lead, "employee_count_exact", 0) or 0)
    except (TypeError, ValueError):
        exact = 0
    if exact > 0:
        return SizeEstimate(band=band_from_count(exact), confidence=0.95, basis=BASIS_EXACT)

    # 2. Weighted vote over derived signals.
    votes = []
    for fn in (_hiring_vote, _funding_vote, _registry_vote, _footprint_vote):
        v = fn(lead)
        if v is not None:
            votes.append(v)

    if not votes:
        return SizeEstimate(band="", confidence=0.0, basis="")

    weighted_sum = sum(idx * w for idx, w, _ in votes)
    total_weight = sum(w for _, w, _ in votes)
    avg_index = weighted_sum / total_weight
    band_index = max(0, min(len(SIZE_BANDS) - 1, round(avg_index)))
    band = SIZE_BANDS[band_index]

    # Confidence grows with how many distinct signals agree, capped well below
    # real providers (0.6 < the typical 0.7+ scrape/paid default_confidence).
    labels = []
    for _, _, label in votes:
        if label not in labels:
            labels.append(label)
    confidence = min(0.6, 0.3 + 0.1 * len(labels))
    basis = f"{BASIS_ESTIMATED_PREFIX}:" + "+".join(labels)

    return SizeEstimate(band=band, confidence=round(confidence, 3), basis=basis)


# ── Secondary helper: revenue band (NOT wired into scoring/filtering in v1) ──

# Coarse revenue bands, deferred per Decision 3 — exposed as a helper only so a
# future revenue-signal feature has a single canonical mapping to build on.
REVENUE_BANDS = ["<$1M", "$1M-$10M", "$10M-$50M", "$50M+"]


def revenue_band_from_funding(funding_amount) -> str:
    """Map a raw funding amount (USD) to a coarse revenue band, else "".

    Secondary/deferred: provided as a pure helper only — NOT scored or filtered
    in v1. A funding raise is a loose proxy for revenue scale, used here purely to
    give downstream consumers one place to derive a band when they opt in.
    """
    nums = re.findall(r"\d[\d,.]*", str(funding_amount or ""))
    if not nums:
        return ""
    try:
        amt = float(nums[0].replace(",", ""))
    except ValueError:
        return ""
    if amt <= 0:
        return ""
    if amt < 1_000_000:
        return REVENUE_BANDS[0]
    if amt < 10_000_000:
        return REVENUE_BANDS[1]
    if amt < 50_000_000:
        return REVENUE_BANDS[2]
    return REVENUE_BANDS[3]
