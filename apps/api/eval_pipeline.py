#!/usr/bin/env python3
"""
Full Pipeline Evaluation — CLI Lead Sourcing Quality Audit

Runs the actual JobRunner pipeline for multiple verticals,
captures every stage's output, and produces a quality report.

Run: uv run python3 apps/api/eval_pipeline.py

Verticals tested:
  1. IT staffing companies in bangalore
  2. packaging manufacturers in mumbai  
  3. digital marketing agencies in delhi
"""

import asyncio
import json
import os
import sys
import time
from dataclasses import asdict
from datetime import datetime, timezone
from typing import Dict, List

PROJECT_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "../.."))
sys.path.insert(0, PROJECT_ROOT)

OUTPUT_DIR = os.path.join(PROJECT_ROOT, "data", "pipeline_eval")
os.makedirs(OUTPUT_DIR, exist_ok=True)

# ── Test Verticals ─────────────────────────────────────────────────────
VERTICALS = [
    {"query": "IT staffing companies in bangalore", "label": "IT Staffing — Bangalore"},
    {"query": "packaging machine manufacturers in mumbai", "label": "Packaging — Mumbai"},
    {"query": "digital marketing agencies in delhi", "label": "Digital Marketing — Delhi"},
]


class PipelineCapture:
    """Capture SSE progress events from the pipeline."""

    def __init__(self):
        self.events: List[Dict] = []
        self.stage_times: Dict[str, float] = {}
        self._stage_start: Dict[str, float] = {}

    def on_event(self, event_type: str, data: dict):
        self.events.append({"type": event_type, "data": data, "ts": time.time()})

        stage = data.get("stage", "")
        if stage and stage not in self._stage_start:
            self._stage_start[stage] = time.time()

        if event_type in ("job_completed", "job_failed"):
            # Close all open stages
            now = time.time()
            for s, t in self._stage_start.items():
                if s not in self.stage_times:
                    self.stage_times[s] = round(now - t, 2)


def score_quality(leads: list) -> dict:
    """Compute quality metrics from a list of Lead objects."""
    total = len(leads)
    if total == 0:
        return {"total": 0}

    has_website = sum(1 for l in leads if l.has_website)
    has_email = sum(1 for l in leads if l.has_email)
    has_phone = sum(1 for l in leads if l.has_phone)
    has_linkedin = sum(1 for l in leads if l.has_linkedin)
    has_contact = sum(1 for l in leads if l.has_contact_person)
    has_description = sum(1 for l in leads if l.description and len(l.description) > 20)

    # Score distribution
    tiers = {"hot": 0, "warm": 0, "cold": 0, "unqualified": 0}
    for l in leads:
        t = l.score_tier or "unqualified"
        tiers[t] = tiers.get(t, 0) + 1

    # Source distribution
    sources = {}
    for l in leads:
        s = l.source.split(":")[0] if ":" in l.source else l.source
        sources[s] = sources.get(s, 0) + 1

    # Email confidence
    email_conf = {}
    for l in leads:
        if l.email_confidence:
            email_conf[l.email_confidence] = email_conf.get(l.email_confidence, 0) + 1

    # Completeness score (0-100)
    field_scores = [
        has_website / total,
        has_email / total,
        has_phone / total,
        has_description / total,
        has_contact / total,
    ]
    completeness = round(sum(field_scores) / len(field_scores) * 100, 1)

    return {
        "total": total,
        "has_website": has_website,
        "has_email": has_email,
        "has_phone": has_phone,
        "has_linkedin": has_linkedin,
        "has_contact_person": has_contact,
        "has_description": has_description,
        "pct_website": round(has_website / total * 100, 1),
        "pct_email": round(has_email / total * 100, 1),
        "pct_phone": round(has_phone / total * 100, 1),
        "pct_contact": round(has_contact / total * 100, 1),
        "completeness_score": completeness,
        "tiers": tiers,
        "sources": sources,
        "email_confidence": email_conf,
        "avg_score": round(sum(l.score for l in leads) / total, 1),
    }


async def run_vertical(vertical: dict, idx: int) -> dict:
    """Run the full pipeline for one vertical and capture results."""
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.services.leadgen.job_runner import JobRunner
    from apps.api.services.leadgen.progress import progress

    query = vertical["query"]
    label = vertical["label"]

    print(f"\n{'━'*72}")
    print(f"  [{idx}] {label}")
    print(f"  Query: {query}")
    print(f"{'━'*72}")

    # Create a separate DB for evaluation (avoid polluting main DB)
    eval_db_path = os.path.join(OUTPUT_DIR, f"eval_{idx}.db")
    if os.path.exists(eval_db_path):
        os.remove(eval_db_path)

    db = LeadDB(db_path=eval_db_path)
    runner = JobRunner(db=db)

    # Capture progress events
    capture = PipelineCapture()
    original_emit = progress.emit

    def capturing_emit(event_type, data):
        capture.on_event(event_type, data)
        msg = data.get("message", "")
        if msg:
            print(f"    {msg}")
        original_emit(event_type, data)

    progress.emit = capturing_emit

    t0 = time.time()
    try:
        job_id = await runner.submit(query)
    except Exception as e:
        job_id = "error"
        print(f"    ❌ Pipeline error: {e}")
    elapsed = round(time.time() - t0, 1)

    # Restore original emit
    progress.emit = original_emit

    # Fetch all leads from the eval DB
    leads = db.get_leads(limit=1000)

    # Quality analysis
    quality = score_quality(leads)

    # Build result object
    result = {
        "label": label,
        "query": query,
        "job_id": job_id,
        "elapsed_seconds": elapsed,
        "stages": [],
        "quality": quality,
        "leads_sample": [],
        "events_count": len(capture.events),
    }

    # Extract stage info from events
    stages_seen = {}
    for ev in capture.events:
        stage = ev["data"].get("stage", "")
        if stage and stage not in stages_seen:
            stages_seen[stage] = {
                "name": stage,
                "messages": [],
            }
        if stage:
            msg = ev["data"].get("message", "")
            if msg:
                stages_seen[stage]["messages"].append(msg)

    result["stages"] = list(stages_seen.values())

    # Sample leads (first 10)
    for lead in leads[:10]:
        result["leads_sample"].append({
            "company": lead.company,
            "website": lead.website,
            "email": lead.email,
            "email_confidence": lead.email_confidence,
            "phone": lead.phone,
            "city": lead.city,
            "score": lead.score,
            "score_tier": lead.score_tier,
            "source": lead.source,
            "contact_person": lead.contact_person,
            "description": lead.description[:100] if lead.description else "",
        })

    # Print summary
    print(f"\n    ┌─────────────────────────────────────────────┐")
    print(f"    │  Results: {quality['total']:3d} leads in {elapsed}s")
    print(f"    │  Website: {quality.get('pct_website', 0):5.1f}%  Email: {quality.get('pct_email', 0):5.1f}%  Phone: {quality.get('pct_phone', 0):5.1f}%")
    print(f"    │  Contact: {quality.get('pct_contact', 0):5.1f}%  Avg Score: {quality.get('avg_score', 0)}")
    print(f"    │  Tiers: {quality.get('tiers', {})}")
    print(f"    │  Completeness: {quality.get('completeness_score', 0)}%")
    print(f"    └─────────────────────────────────────────────┘")

    # Save per-vertical results
    fname = os.path.join(OUTPUT_DIR, f"vertical_{idx}_{label.split('—')[0].strip().lower().replace(' ', '_')}.json")
    with open(fname, "w") as f:
        json.dump(result, f, indent=2, default=str)

    # Cleanup eval DB
    try:
        db.conn.close()
    except Exception:
        pass

    return result


async def main():
    print(f"{'═'*72}")
    print(f"  PIPELINE EVALUATION — Full Lead Sourcing Quality Audit")
    print(f"  {datetime.now().isoformat()}")
    print(f"  Verticals: {len(VERTICALS)}")
    print(f"  Output: {OUTPUT_DIR}")
    print(f"{'═'*72}")

    all_results = []
    t0 = time.time()

    for idx, vertical in enumerate(VERTICALS, 1):
        result = await run_vertical(vertical, idx)
        all_results.append(result)

    total_elapsed = round(time.time() - t0, 1)

    # ── Aggregate Report ───────────────────────────────────────────────
    report = {
        "evaluated_at": datetime.now().isoformat(),
        "total_elapsed_seconds": total_elapsed,
        "verticals_count": len(VERTICALS),
        "aggregate": {
            "total_leads": sum(r["quality"]["total"] for r in all_results),
            "avg_completeness": round(
                sum(r["quality"].get("completeness_score", 0) for r in all_results) / len(all_results), 1
            ) if all_results else 0,
            "avg_score": round(
                sum(r["quality"].get("avg_score", 0) for r in all_results) / len(all_results), 1
            ) if all_results else 0,
        },
        "per_vertical": [
            {
                "label": r["label"],
                "query": r["query"],
                "leads": r["quality"]["total"],
                "elapsed": r["elapsed_seconds"],
                "completeness": r["quality"].get("completeness_score", 0),
                "avg_score": r["quality"].get("avg_score", 0),
                "tiers": r["quality"].get("tiers", {}),
                "pct_website": r["quality"].get("pct_website", 0),
                "pct_email": r["quality"].get("pct_email", 0),
                "pct_phone": r["quality"].get("pct_phone", 0),
                "pct_contact": r["quality"].get("pct_contact", 0),
            }
            for r in all_results
        ],
    }

    # Save master report
    with open(os.path.join(OUTPUT_DIR, "_evaluation_report.json"), "w") as f:
        json.dump(report, f, indent=2)

    # ── Print Final Report ─────────────────────────────────────────────
    print(f"\n{'═'*72}")
    print(f"  FINAL EVALUATION REPORT")
    print(f"{'═'*72}")
    print(f"  Total time:       {total_elapsed}s")
    print(f"  Total leads:      {report['aggregate']['total_leads']}")
    print(f"  Avg completeness: {report['aggregate']['avg_completeness']}%")
    print(f"  Avg score:        {report['aggregate']['avg_score']}")
    print()

    for v in report["per_vertical"]:
        print(f"  ┌── {v['label']}")
        print(f"  │   Leads: {v['leads']:3d}  |  Time: {v['elapsed']}s  |  Score: {v['avg_score']}")
        print(f"  │   Website: {v['pct_website']}%  Email: {v['pct_email']}%  Phone: {v['pct_phone']}%  Contact: {v['pct_contact']}%")
        print(f"  │   Tiers: {v['tiers']}")
        print(f"  │   Completeness: {v['completeness']}%")
        print(f"  └───")

    print(f"\n  Saved to: {OUTPUT_DIR}/")
    print(f"{'═'*72}")


if __name__ == "__main__":
    asyncio.run(main())
