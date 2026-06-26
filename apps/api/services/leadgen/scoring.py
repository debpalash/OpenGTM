"""
Lead Scoring Engine — Score leads 0-100 based on Yupcha's ICP.

Each lead gets a quality score based on data completeness, company fit,
and contact availability. Scores determine the tier: Hot, Warm, Cold, Unqualified.
"""

import os
from typing import List, Optional

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.config import ICP, SCORING_WEIGHTS, SCORE_TIERS
from apps.api.services.leadgen.enrichment.size_heuristic import normalize_band


# How far source-reliability may nudge a base score. The nudge is
# RELIABILITY_SWING * (r - 0.5), so r∈[0,1] yields at most ±(SWING/2) points.
# Default 8 → ±4 pts: enough to break ties / cross a tier boundary, never enough
# to override ICP-fit weights (10–20) — ICP must keep dominating.
RELIABILITY_SWING = float(os.getenv("SOURCE_RELIABILITY_SWING", "8"))


def _apply_source_reliability(score: int, r: Optional[float]) -> int:
    """Bounded last-step nudge of a base score by source reliability ``r∈[0,1]``.

    ``r is None`` (source below MIN_SAMPLES, or flag off) → identity, so the
    default path is byte-for-byte unchanged. Otherwise:
        adjusted = clamp(round(base + RELIABILITY_SWING*(r-0.5)), 0, 100)
    Reliable sources (r>0.5) nudge up, noisy ones (r<0.5) down, r==0.5 → no-op.
    Always clamped to [0,100]; the swing alone can never zero a lead.
    """
    if r is None:
        return score
    adjusted = round(score + RELIABILITY_SWING * (r - 0.5))
    return max(0, min(100, adjusted))


def score_lead(lead: Lead, source_reliability: Optional[float] = None) -> int:
    """
    Calculate a quality score (0-100) for a lead based on ICP fit.

    Scoring signals:
    - Has website:            +10
    - Has email:              +10
    - Has phone:              +10
    - Has LinkedIn:           +5
    - Company size > 50:      +15
    - Specialization match:   +20
    - Tier-1 city:            +10
    - Decision maker found:   +15
    - Multiple contacts:      +5
    """
    score = 0

    # Data completeness signals
    if lead.has_website:
        score += SCORING_WEIGHTS["has_website"]

    if lead.has_email:
        score += SCORING_WEIGHTS["has_email"]

    if lead.has_phone:
        score += SCORING_WEIGHTS["has_phone"]

    if lead.has_linkedin:
        score += SCORING_WEIGHTS["has_linkedin"]

    # Company size signal. normalize_band coerces non-canonical formats (raw int,
    # "10-50", "5,000", "501-1000") into one of the four bands BEFORE the
    # membership test — a strict-widening bug fix: previously-unmatched formats
    # now credit correctly, and a value that's already a valid band is returned
    # verbatim so no existing score can drop. Estimated bands earn the same credit
    # as known size (provenance lives in company_size_basis, not the score).
    if normalize_band(lead.company_size) in ("51-200", "201-500", "500+"):
        score += SCORING_WEIGHTS["company_size_large"]

    # Specialization match
    if lead.specialization:
        spec_lower = lead.specialization.lower()
        for pref in ICP["preferred_specializations"]:
            if pref.lower() in spec_lower or spec_lower in pref.lower():
                score += SCORING_WEIGHTS["specialization_match"]
                break
        else:
            # Partial match against broader industry list
            for ind in ICP["target_industries"]:
                if ind.lower() in spec_lower or spec_lower in ind.lower():
                    score += SCORING_WEIGHTS["specialization_match"] // 2
                    break

    # Industry tags match (bonus for multiple tag matches)
    if lead.industry_tags:
        tags_lower = lead.industry_tags.lower()
        tag_matches = sum(
            1 for pref in ICP["preferred_specializations"]
            if pref.lower() in tags_lower
        )
        if tag_matches >= 2:
            score += 5  # Bonus for multi-tag match

    # City signal
    if lead.city in ICP.get("tier1_cities", []):
        score += SCORING_WEIGHTS["tier1_city"]
    elif lead.city in ICP.get("target_cities", []):
        score += SCORING_WEIGHTS["tier1_city"] // 2

    # Decision maker signal
    if lead.has_contact_person:
        score += SCORING_WEIGHTS["decision_maker_found"]

    # Decision makers JSON (structured contacts = higher quality)
    if lead.decision_makers:
        try:
            import json
            dms = json.loads(lead.decision_makers)
            if len(dms) >= 2:
                score += 5  # Multiple decision makers found
        except (json.JSONDecodeError, TypeError):
            pass

    # Multiple contacts signal
    contact_count = sum([
        lead.has_email,
        lead.has_phone,
        lead.has_linkedin,
        bool(lead.twitter_url),
    ])
    if contact_count >= 3:
        score += SCORING_WEIGHTS["multiple_contacts"]

    # Established company signals
    if lead.founded_year:
        score += 3  # Knowing founding year = more data = better lead

    if lead.revenue_range:
        score += 3  # Revenue data available

    if lead.glassdoor_rating:
        score += 2  # Has review presence

    # Cap ICP score at 100, THEN apply the bounded source-reliability nudge as
    # the last step (identity when source_reliability is None → default path
    # unchanged). ICP fit is fully computed and dominant before this runs.
    return _apply_source_reliability(min(score, 100), source_reliability)


def get_tier(score: int) -> str:
    """Determine the lead tier based on score."""
    for tier, (low, high) in SCORE_TIERS.items():
        if low <= score <= high:
            return tier
    return "unqualified"


def score_leads(leads: List[Lead]) -> List[Lead]:
    """
    Score all leads and assign tiers.

    Updates leads in-place and returns them.
    """
    print(f"  📊 Scoring {len(leads)} leads...")

    tier_counts = {"hot": 0, "warm": 0, "cold": 0, "unqualified": 0}

    for lead in leads:
        lead.score = score_lead(lead)
        lead.score_tier = get_tier(lead.score)
        tier_counts[lead.score_tier] += 1

    print(f"  🔥 Hot: {tier_counts['hot']} | "
          f"🟡 Warm: {tier_counts['warm']} | "
          f"🔵 Cold: {tier_counts['cold']} | "
          f"⚪ Unqualified: {tier_counts['unqualified']}")

    return leads


def score_and_update_db(db) -> dict:
    """
    Re-score all leads in the database and update their scores.

    Returns tier count summary.
    """
    leads = db.get_leads(limit=10000)
    print(f"  📊 Re-scoring {len(leads)} leads in database...")

    tier_counts = {"hot": 0, "warm": 0, "cold": 0, "unqualified": 0}

    for lead in leads:
        new_score = score_lead(lead)
        new_tier = get_tier(new_score)
        tier_counts[new_tier] += 1

        if lead.score != new_score or lead.score_tier != new_tier:
            db.update_lead_fields(lead.id, {
                "score": new_score,
                "score_tier": new_tier,
            })

    print(f"  🔥 Hot: {tier_counts['hot']} | "
          f"🟡 Warm: {tier_counts['warm']} | "
          f"🔵 Cold: {tier_counts['cold']} | "
          f"⚪ Unqualified: {tier_counts['unqualified']}")

    return tier_counts
