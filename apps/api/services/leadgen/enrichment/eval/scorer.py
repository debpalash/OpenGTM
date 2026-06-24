"""Provider accuracy + freshness scorer (offline, deterministic).

Given a golden dataset (ground-truth field values per lead identity) and RECORDED
provider outputs for those same identities, compute for each provider:

  - accuracy: fraction of the values it asserted that match the golden truth
    (normalized, field-aware). A provider that confidently returns WRONG data is
    penalised here even if its hit-rate is high — this is the signal the planner
    is missing today.
  - coverage: fraction of golden field slots it actually answered (informational;
    coverage is what hit-rate already captures, so the planner keeps using
    hit-rate for that and this module focuses on correctness).
  - freshness: how recent the asserted values are (decays with age of `asof`).
  - score: the combined per-provider CORRECTNESS PRIOR fed to the planner.

No network. Pure stdlib + project normalizers.
"""

from __future__ import annotations

import json
import os
import re
from dataclasses import dataclass, field as dc_field
from datetime import date, datetime
from typing import Any, Dict, List, Optional

# Where the seeded datasets live (overridable for tests / alt environments).
_REPO_ROOT = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "..", "..", "..", "..", "..")
)
GOLDEN_PATH = os.path.join(_REPO_ROOT, "data", "provider_eval", "golden.json")
RECORDED_PATH = os.path.join(_REPO_ROOT, "data", "provider_eval", "recorded_outputs.json")

# A correct value loses this much freshness weight per year of age. A value with
# no `asof` is treated as fully fresh (we can't penalise what we can't date).
FRESHNESS_DECAY_PER_YEAR = 0.15
# Freshness contributes at most this weight to the final score; accuracy is the
# dominant term (a fresh-but-wrong answer must never beat a stale-but-right one).
FRESHNESS_WEIGHT = 0.25


# ── normalization ────────────────────────────────────────────────

_WWW_RE = re.compile(r"^(https?://)?(www\.)?", re.I)


def _normalize(field_name: str, value: Any) -> str:
    """Field-aware normalization so trivially-equal values compare equal
    (case, scheme, www, trailing slash, surrounding whitespace)."""
    if value is None:
        return ""
    s = str(value).strip().lower()
    if not s:
        return ""
    if field_name in ("website", "domain"):
        s = _WWW_RE.sub("", s).rstrip("/")
        # keep only the host for a domain-style field
        s = s.split("/")[0]
    elif field_name.endswith("_url") or s.startswith(("http://", "https://")):
        s = _WWW_RE.sub("", s).rstrip("/")
    elif field_name in ("founding_year", "founded_year"):
        m = re.search(r"\d{4}", s)
        s = m.group(0) if m else s
    return s


def values_match(field_name: str, got: Any, expected: Any) -> bool:
    a, b = _normalize(field_name, got), _normalize(field_name, expected)
    if not a or not b:
        return False
    if a == b:
        return True
    # For free-text-ish fields, accept containment either way (e.g. golden
    # "aerospace" vs provider "aerospace & defense").
    if field_name in ("industry_tags", "description"):
        return a in b or b in a
    return False


def _freshness(asof: Optional[str], today: date) -> float:
    """1.0 for a value dated today, decaying with age. Undated → 1.0."""
    if not asof:
        return 1.0
    try:
        d = datetime.strptime(asof, "%Y-%m-%d").date()
    except (ValueError, TypeError):
        return 1.0
    years = max(0.0, (today - d).days / 365.25)
    return max(0.0, 1.0 - FRESHNESS_DECAY_PER_YEAR * years)


# ── result types ─────────────────────────────────────────────────

@dataclass
class FieldVerdict:
    case_id: str
    field: str
    got: Any
    expected: Any
    correct: bool
    freshness: float = 1.0


@dataclass
class ProviderScore:
    provider: str
    asserted: int = 0          # field-values the provider returned (& we had truth for)
    correct: int = 0           # of those, how many matched golden
    golden_slots: int = 0      # total golden field-values it COULD have answered
    freshness_sum: float = 0.0 # sum of freshness over correct assertions
    verdicts: List[FieldVerdict] = dc_field(default_factory=list)

    @property
    def accuracy(self) -> float:
        """Correctness of what it asserted. The key anti-confident-wrong signal."""
        return (self.correct / self.asserted) if self.asserted else 0.0

    @property
    def coverage(self) -> float:
        return (self.asserted / self.golden_slots) if self.golden_slots else 0.0

    @property
    def freshness(self) -> float:
        return (self.freshness_sum / self.correct) if self.correct else 0.0

    @property
    def score(self) -> float:
        """Combined per-provider correctness prior in [0, 1].

        Accuracy dominates; freshness is a small multiplier. Coverage is left to
        the planner's existing hit-rate term, so we don't double-count it.
        """
        return self.accuracy * (1.0 - FRESHNESS_WEIGHT + FRESHNESS_WEIGHT * self.freshness)

    def to_api(self) -> dict:
        return {
            "provider": self.provider,
            "accuracy": round(self.accuracy, 3),
            "coverage": round(self.coverage, 3),
            "freshness": round(self.freshness, 3),
            "score": round(self.score, 3),
            "asserted": self.asserted,
            "correct": self.correct,
            "golden_slots": self.golden_slots,
        }


# ── loading ──────────────────────────────────────────────────────

def load_golden(path: str = GOLDEN_PATH) -> List[dict]:
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh).get("cases", [])


def load_recorded_outputs(path: str = RECORDED_PATH) -> Dict[str, Dict[str, dict]]:
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh).get("outputs", {})


# ── scoring ──────────────────────────────────────────────────────

def score_outputs(
    golden: List[dict],
    outputs: Dict[str, Dict[str, dict]],
    today: Optional[date] = None,
) -> Dict[str, ProviderScore]:
    """Score recorded provider outputs against golden ground truth.

    Args:
        golden: list of cases [{id, input, expected:{field: value}, expected_asof?}]
        outputs: {case_id: {provider: {fields:{field:value}, asof?:'YYYY-MM-DD'}}}
        today: reference date for freshness (defaults to date.today()).

    Returns: {provider: ProviderScore}
    """
    today = today or date.today()
    scores: Dict[str, ProviderScore] = {}
    golden_by_id = {c["id"]: c for c in golden}

    for case_id, by_provider in outputs.items():
        case = golden_by_id.get(case_id)
        if not case:
            continue
        expected = case.get("expected", {})
        for provider, payload in by_provider.items():
            ps = scores.setdefault(provider, ProviderScore(provider=provider))
            fields = (payload or {}).get("fields", {}) or {}
            asof = (payload or {}).get("asof")
            fresh = _freshness(asof, today)
            for fname, exp_val in expected.items():
                ps.golden_slots += 1
                if fname not in fields or fields.get(fname) in (None, "", []):
                    continue
                got = fields[fname]
                ps.asserted += 1
                ok = values_match(fname, got, exp_val)
                if ok:
                    ps.correct += 1
                    ps.freshness_sum += fresh
                ps.verdicts.append(
                    FieldVerdict(case_id, fname, got, exp_val, ok, fresh if ok else 0.0)
                )

    return scores


def rank_providers(scores: Dict[str, ProviderScore]) -> List[ProviderScore]:
    """Highest correctness score first; ties broken by accuracy then volume."""
    return sorted(
        scores.values(),
        key=lambda s: (s.score, s.accuracy, s.asserted),
        reverse=True,
    )


def run_eval(
    golden_path: str = GOLDEN_PATH,
    recorded_path: str = RECORDED_PATH,
    today: Optional[date] = None,
) -> List[ProviderScore]:
    """Convenience: load seeded data, score, return ranked providers."""
    golden = load_golden(golden_path)
    outputs = load_recorded_outputs(recorded_path)
    return rank_providers(score_outputs(golden, outputs, today=today))
