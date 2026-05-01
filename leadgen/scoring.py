"""
Lead Scoring Engine — Score leads 0-100 based on Yupcha's ICP.

Each lead gets a quality score based on data completeness, company fit,
and contact availability. Scores determine the tier: Hot, Warm, Cold, Unqualified.
"""

from typing import List

from leadgen.models import Lead
from config import ICP, SCORING_WEIGHTS, SCORE_TIERS


def score_lead(lead: Lead) -> int:
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

    # Company size signal
    if lead.company_size in ("51-200", "201-500", "500+"):
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

    # City signal
    if lead.city in ICP.get("tier1_cities", []):
        score += SCORING_WEIGHTS["tier1_city"]
    elif lead.city in ICP.get("target_cities", []):
        score += SCORING_WEIGHTS["tier1_city"] // 2

    # Decision maker signal
    if lead.has_contact_person:
        score += SCORING_WEIGHTS["decision_maker_found"]

    # Multiple contacts signal
    contact_count = sum([
        lead.has_email,
        lead.has_phone,
        lead.has_linkedin,
        bool(lead.twitter_url),
    ])
    if contact_count >= 3:
        score += SCORING_WEIGHTS["multiple_contacts"]

    # Cap at 100
    return min(score, 100)


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
