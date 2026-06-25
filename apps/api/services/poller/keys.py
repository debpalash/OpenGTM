"""Frozen dedup-key construction + target normalization (spec §8).

KEY_SCHEMA_VERSION=1 is permanent (a change requires a backfill/migration). The
signal-event natural id and the deterministic ``signals.id`` it produces guarantee
EXACTLY-ONCE emission: ``PgLeadStore.add_signal`` does ``s.get(SignalRow, id)``
and inserts only when absent, firing ``on_signal`` only on the inserted path — so
re-polling never re-emits, re-fires, or re-charges (spec §8.2).

Key invariants (spec review notes):
  * CIK supremacy — once ``resolved_cik`` is known the funding/exec/hiring/tech
    keys use the CIK, never the mutable display name.
  * ``lead_id`` is EXCLUDED from the key (it is mutable; it routes the row only).
  * Hiring/tech keys are intent/period aware (band-week / intent-tier).
"""

from __future__ import annotations

import hashlib
import re

from apps.api.services.leadgen.enrichment.providers.sec_edgar import _norm_name, _pad_cik
from apps.api.services.poller.models import KEY_SCHEMA_VERSION

# C-level / VP role tiers for executive_hired keying (coarse, stable). Order
# matters: "director" is matched BEFORE the short C-level abbreviations so
# "direCTOr" doesn't false-match "cto". Abbreviations are matched as whole tokens
# (word boundaries) to avoid substring collisions.
_ROLE_TIER_WORD_RULES = (
    ("director", ("director", "board")),
    ("c_level", ("chief", "president", "ceo", "cfo", "cto", "coo", "cmo", "cro", "ciso", "cco", "cpo")),
    ("vp", ("vp", "vice president", "head of")),
)


def normalize_target(target: str) -> str:
    """Frozen target normalization (spec §8.1).

    ``sec_cik:<cik>`` → canonical 10-digit CIK. URLs → scheme/``www.``/trailing-
    slash stripped, lowercased. Company names → ``sec_edgar._norm_name`` (lower,
    strip legal suffixes + punctuation, collapse whitespace).
    """
    t = (target or "").strip()
    if not t:
        return ""
    low = t.lower()
    if low.startswith("sec_cik:"):
        cik = _pad_cik(low.split(":", 1)[1].strip())
        return cik or low
    if low.startswith("http://") or low.startswith("https://"):
        u = re.sub(r"^https?://", "", low)
        u = re.sub(r"^www\.", "", u)
        return u.rstrip("/")
    return _norm_name(t)


def stable_company_id(resolved_cik: str | None, target: str) -> str:
    """The stable company identity used in every key: CIK when known, else the
    normalized target (spec §8.2). Never the mutable lead_id."""
    if resolved_cik:
        return _pad_cik(resolved_cik) or resolved_cik
    return normalize_target(target)


def role_tier(title: str) -> str:
    """Coarse role tier for executive_hired keying (C-level / director / vp).

    Matches needles as whole words (``\\b...\\b``) so short abbreviations like
    ``cto`` never substring-collide with ``director``; rule order resolves
    overlap (director first)."""
    t = (title or "").lower()
    for tier, needles in _ROLE_TIER_WORD_RULES:
        for n in needles:
            if re.search(r"\b" + re.escape(n) + r"\b", t):
                return tier
    return "other"


def is_executive_title(title: str) -> bool:
    """True when the related-person title is a C-level / director / VP role
    (executive_hired is narrow — Form-D related persons only, spec review note)."""
    return role_tier(title) in ("c_level", "director", "vp")


def normalize_exec_name(name: str) -> str:
    return re.sub(r"\s+", " ", (name or "").strip().lower())


def normalize_tech(tech: str) -> str:
    return re.sub(r"\s+", " ", (tech or "").strip().lower())


def signal_event_id(
    workspace_id: str,
    kind: str,
    stable_company: str,
    signal_type: str,
    natural_event_id: str,
) -> str:
    """Deterministic sha256 hex id for a signal event (spec §8.2).

        sha256(f"v{V}|{ws}|{kind}|{stable_company}|{signal_type}|{natural_event_id}")
    """
    raw = (
        f"v{KEY_SCHEMA_VERSION}|{workspace_id}|{kind}|{stable_company}"
        f"|{signal_type}|{natural_event_id}"
    )
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()
