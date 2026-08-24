#!/usr/bin/env python3
"""
OpenGTM — CLI Entry Point

Usage:
  uv run python -m apps.api.cli server       Launch FastAPI server
  uv run python -m apps.api.cli dashboard    Launch lead dashboard (legacy Flask)
  uv run python -m apps.api.cli collect "Q"  Run stealth collection query
  uv run python -m apps.api.cli scrape       Run all scrapers
  uv run python -m apps.api.cli enrich       Enrich leads with missing data
  uv run python -m apps.api.cli score        Re-score all leads
  uv run python -m apps.api.cli pipeline     Full pipeline (scrape+enrich+score)
  uv run python -m apps.api.cli export       Export to CSV
  uv run python -m apps.api.cli stats        Print database stats
  uv run python -m apps.api.cli cleanup      Purge invalid leads
  uv run python -m apps.api.cli jobs         List job queue
"""

import argparse
import asyncio
import sys
import os

# Ensure project root is in path
ROOT_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
if ROOT_DIR not in sys.path:
    sys.path.insert(0, ROOT_DIR)


def cmd_server(args):
    """Launch FastAPI server."""
    import uvicorn
    print(f"\n  ◉ OpenGTM v3.0 — http://0.0.0.0:{args.port}")
    uvicorn.run(
        "apps.api.main:app",
        host="0.0.0.0",
        port=args.port,
        reload=args.reload,
    )


def cmd_import(args):
    """Import existing CSV files into database."""
    from apps.api.services.leadgen.scrapers.csv_import import import_all_csvs
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.services.leadgen.scoring import score_and_update_db

    leads = import_all_csvs(".")
    db = LeadDB()
    count = db.bulk_upsert(leads)
    print(f"\n  💾 Imported {count} leads into database")
    score_and_update_db(db)
    db.close()


def cmd_scrape(args):
    """Run scrapers."""
    from apps.api.services.leadgen.pipeline import run_scrapers, deduplicate_leads
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.services.leadgen.scoring import score_and_update_db
    from apps.api.services.leadgen.config import ICP

    sources = [args.source] if args.source else None
    cities = args.city.split(",") if args.city else None

    raw = asyncio.run(run_scrapers(sources=sources, cities=cities))
    unique = deduplicate_leads(raw)

    for l in unique:
        if not l.yupcha_value_prop:
            l.yupcha_value_prop = ICP["value_proposition"]

    db = LeadDB()
    db.bulk_upsert(unique)
    score_and_update_db(db)
    db.close()
    print(f"\n  ✅ Scraped and stored {len(unique)} leads")


def cmd_enrich(args):
    """Run enrichment pipeline."""
    from apps.api.services.leadgen.pipeline import run_enrichment
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.services.leadgen.scoring import score_and_update_db

    db = LeadDB()
    asyncio.run(run_enrichment(db, limit=args.limit))
    score_and_update_db(db)
    db.close()


def cmd_score(args):
    """Re-score all leads."""
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.services.leadgen.scoring import score_and_update_db

    db = LeadDB()
    score_and_update_db(db)
    db.close()


def cmd_pipeline(args):
    """Run full pipeline."""
    from apps.api.services.leadgen.pipeline import run_full_pipeline
    sources = [args.source] if args.source else None
    asyncio.run(run_full_pipeline(sources=sources, enrich=not args.no_enrich))


def cmd_export(args):
    """Export leads."""
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.services.leadgen.export import export_csv, export_json

    db = LeadDB()
    output = args.output or f"data/leads_export.{args.format}"

    if args.format == "json":
        export_json(db, output, score_min=args.min_score,
                    status=args.status, city=args.city, score_tier=args.tier)
    else:
        export_csv(db, output, score_min=args.min_score,
                   status=args.status, city=args.city, score_tier=args.tier)
    db.close()


def cmd_stats(args):
    """Print database stats."""
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.services.leadgen.export import print_stats

    db = LeadDB()
    print_stats(db)
    db.close()


def cmd_dashboard(args):
    """Launch FastAPI server serving the lead dashboard."""
    cmd_server(args)


def cmd_collect(args):
    """Submit and run a stealth collection query."""
    from apps.api.services.leadgen.job_runner import JobRunner
    from apps.api.services.leadgen.db import LeadDB

    workspace_id = ""
    if args.workspace:
        db = LeadDB()
        workspaces = db.get_workspaces()
        match = [w for w in workspaces if w["name"].lower() == args.workspace.lower()]
        if match:
            workspace_id = match[0]["id"]
        else:
            workspace_id = db.create_workspace(args.workspace)
            print(f"  📁 Created workspace: {args.workspace} ({workspace_id})")
        db.close()

    runner = JobRunner()
    print(f"\n  🔍 Collecting: '{args.query}'")
    if workspace_id:
        print(f"  📁 Workspace: {args.workspace} ({workspace_id})")
    asyncio.run(runner.submit(args.query, workspace_id=workspace_id))


def cmd_cleanup(args):
    """Purge invalid/garbage leads from database."""
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.services.leadgen.lead_validator import validate_lead

    db = LeadDB()
    leads = db.get_leads(limit=10000)
    print(f"\n  🔍 Validating {len(leads)} leads...")

    marked_dead = 0
    cleaned_emails = 0
    reasons = {}

    for lead in leads:
        if lead.status == "dead":
            continue

        is_valid, reason = validate_lead(lead)
        if not is_valid:
            db.update_status(lead.id, "dead", note=f"cleanup:{reason}")
            marked_dead += 1
            reasons[reason] = reasons.get(reason, 0) + 1
        else:
            orig = db.get_lead(lead.id)
            if orig and orig.email != lead.email:
                db.update_lead_fields(lead.id, {"email": lead.email})
                cleaned_emails += 1
            if orig and orig.phone != lead.phone:
                db.update_lead_fields(lead.id, {"phone": lead.phone})

    print(f"\n  🗑️  Marked {marked_dead} leads as dead:")
    for reason, count in sorted(reasons.items(), key=lambda x: -x[1]):
        print(f"      {reason}: {count}")
    if cleaned_emails:
        print(f"  🧹 Cleaned {cleaned_emails} publisher emails")
    print(f"  ✅ {len(leads) - marked_dead} leads remain active")
    db.close()


def cmd_jobs(args):
    """List job queue status."""
    from apps.api.services.leadgen.db import LeadDB
    db = LeadDB()
    jobs = db.get_jobs(status=args.status)
    db.close()
    if not jobs:
        print("  No jobs found")
        return
    print(f"\n  {'ID':<10} {'Status':<10} {'Leads':<7} {'Query'}")
    print(f"  {'─'*10} {'─'*10} {'─'*7} {'─'*30}")
    for j in jobs:
        print(f"  {j['id']:<10} {j['status']:<10} {j['leads_found']:<7} {j['query'][:40]}")


def main():
    parser = argparse.ArgumentParser(description="OpenGTM — Go-to-Market Agents for atomic teams")
    sub = parser.add_subparsers(dest="command", help="Command to run")

    # server
    p = sub.add_parser("server", help="Launch FastAPI server")
    p.add_argument("--port", type=int, default=8000)
    p.add_argument("--reload", action="store_true")

    # import
    sub.add_parser("import", help="Import existing CSVs")

    # scrape
    p = sub.add_parser("scrape", help="Run scrapers")
    p.add_argument("--source", help="Specific source")
    p.add_argument("--city", help="Comma-separated cities")

    # enrich
    p = sub.add_parser("enrich", help="Enrich leads")
    p.add_argument("--limit", type=int, default=None)

    # score
    sub.add_parser("score", help="Re-score all leads")

    # pipeline
    p = sub.add_parser("pipeline", help="Full pipeline")
    p.add_argument("--source", help="Specific source")
    p.add_argument("--no-enrich", action="store_true")

    # export
    p = sub.add_parser("export", help="Export leads")
    p.add_argument("--format", choices=["csv", "json"], default="csv")
    p.add_argument("--output", help="Output file path")
    p.add_argument("--min-score", type=int)
    p.add_argument("--status")
    p.add_argument("--city")
    p.add_argument("--tier")

    # stats
    sub.add_parser("stats", help="Show database stats")

    # dashboard
    p = sub.add_parser("dashboard", help="Launch dashboard server")
    p.add_argument("--port", type=int, default=8000)
    p.add_argument("--reload", action="store_true")

    # collect
    p = sub.add_parser("collect", help="Run stealth collection query")
    p.add_argument("query", help="Search query")
    p.add_argument("--workspace", "-w", help="Workspace name")

    # jobs
    p = sub.add_parser("jobs", help="List collection jobs")
    p.add_argument("--status")

    # cleanup
    sub.add_parser("cleanup", help="Purge bad/invalid leads")

    args = parser.parse_args()
    if not args.command:
        parser.print_help()
        return

    cmds = {
        "server": cmd_server, "import": cmd_import, "scrape": cmd_scrape,
        "enrich": cmd_enrich, "score": cmd_score, "pipeline": cmd_pipeline,
        "export": cmd_export, "stats": cmd_stats, "dashboard": cmd_dashboard,
        "collect": cmd_collect, "jobs": cmd_jobs, "cleanup": cmd_cleanup,
    }
    cmds[args.command](args)


if __name__ == "__main__":
    main()
