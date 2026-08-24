"""
Export Module — Export leads to CSV, JSON, or print summary.
"""

import csv
import json
from pathlib import Path
from typing import List, Optional

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.db import LeadDB


def export_csv(
    db: LeadDB,
    output_path: str = "data/leads_export.csv",
    score_min: Optional[int] = None,
    status: Optional[str] = None,
    city: Optional[str] = None,
    score_tier: Optional[str] = None,
) -> str:
    """Export filtered leads to CSV."""
    leads = db.get_leads(
        score_min=score_min,
        status=status,
        city=city,
        score_tier=score_tier,
        limit=10000,
    )

    output_path = Path(output_path)
    output_path.parent.mkdir(parents=True, exist_ok=True)

    headers = [
        "Company", "Website", "Email", "Phone", "City", "Specialization",
        "Company Size", "LinkedIn", "Contact Person", "Contact Title",
        "Score", "Tier", "Status", "Source", "Notes",
        "OpenGTM Value Prop", "Company Need",
    ]

    with open(output_path, "w", newline="", encoding="utf-8") as f:
        writer = csv.writer(f)
        writer.writerow(headers)
        for lead in leads:
            writer.writerow([
                lead.company, lead.website, lead.email, lead.phone,
                lead.city, lead.specialization, lead.company_size,
                lead.linkedin_url, lead.contact_person, lead.contact_title,
                lead.score, lead.score_tier, lead.status, lead.source,
                lead.notes, lead.yupcha_value_prop, lead.company_need,
            ])

    print(f"  📤 Exported {len(leads)} leads to {output_path}")
    return str(output_path)


def export_json(
    db: LeadDB,
    output_path: str = "data/leads_export.json",
    score_min: Optional[int] = None,
    status: Optional[str] = None,
    city: Optional[str] = None,
    score_tier: Optional[str] = None,
) -> str:
    """Export filtered leads to JSON."""
    leads = db.get_leads(
        score_min=score_min,
        status=status,
        city=city,
        score_tier=score_tier,
        limit=10000,
    )

    output_path = Path(output_path)
    output_path.parent.mkdir(parents=True, exist_ok=True)

    data = [lead.to_dict() for lead in leads]

    with open(output_path, "w", encoding="utf-8") as f:
        json.dump(data, f, indent=2, ensure_ascii=False)

    print(f"  📤 Exported {len(leads)} leads to {output_path}")
    return str(output_path)


def print_stats(db: LeadDB) -> None:
    """Print a formatted summary of the lead database."""
    stats = db.get_stats()

    print("\n" + "=" * 60)
    print("  📊 YUPCHA LEAD DATABASE — SUMMARY")
    print("=" * 60)

    print(f"\n  Total Leads: {stats['total']}")
    print(f"  Average Score: {stats['enrichment']['avg_score']}")

    print("\n  ── By Tier ────────────────────────────────")
    for tier in ["hot", "warm", "cold", "unqualified"]:
        count = stats["by_tier"].get(tier, 0)
        icon = {"hot": "🔥", "warm": "🟡", "cold": "🔵", "unqualified": "⚪"}.get(tier, "")
        bar = "█" * min(count, 40)
        print(f"  {icon} {tier.capitalize():14s} {count:4d}  {bar}")

    print("\n  ── By Status ──────────────────────────────")
    for status, count in stats["by_status"].items():
        print(f"    {status:14s} {count:4d}")

    print("\n  ── By Source ──────────────────────────────")
    for source, count in stats["by_source"].items():
        print(f"    {source:18s} {count:4d}")

    print("\n  ── By City (Top 10) ───────────────────────")
    for city, count in list(stats["by_city"].items())[:10]:
        print(f"    {city:18s} {count:4d}")

    enr = stats["enrichment"]
    total = enr["total"] or 1
    print("\n  ── Enrichment Coverage ────────────────────")
    print(f"    With Website:  {enr['with_website']:4d} ({100*enr['with_website']//total}%)")
    print(f"    With Email:    {enr['with_email']:4d} ({100*enr['with_email']//total}%)")
    print(f"    With Phone:    {enr['with_phone']:4d} ({100*enr['with_phone']//total}%)")
    print(f"    With LinkedIn: {enr['with_linkedin']:4d} ({100*enr['with_linkedin']//total}%)")
    print(f"    With Contact:  {enr['with_contact']:4d} ({100*enr['with_contact']//total}%)")

    print("\n" + "=" * 60)
