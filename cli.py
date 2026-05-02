#!/usr/bin/env python3
"""
Yupcha Lead Pipeline — CLI Entry Point

Usage:
  python cli.py import              Import existing CSVs into database
  python cli.py scrape              Run all scrapers
  python cli.py scrape --source X   Run specific scraper
  python cli.py enrich              Enrich leads with missing data
  python cli.py enrich --limit 10   Enrich limited batch
  python cli.py score               Re-score all leads
  python cli.py pipeline            Full pipeline (scrape+enrich+score)
  python cli.py export              Export to CSV
  python cli.py export --format json --min-score 75
  python cli.py stats               Print database stats
  python cli.py dashboard           Launch web dashboard
"""

import argparse
import asyncio
import sys
import os

# Add project root to path
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))


def cmd_import(args):
    """Import existing CSV files into database."""
    from leadgen.scrapers.csv_import import import_all_csvs
    from leadgen.db import LeadDB
    from leadgen.scoring import score_and_update_db

    leads = import_all_csvs(".")
    db = LeadDB()
    count = db.bulk_upsert(leads)
    print(f"\n  💾 Imported {count} leads into database")
    score_and_update_db(db)
    db.close()


def cmd_scrape(args):
    """Run scrapers."""
    from leadgen.pipeline import run_scrapers
    from leadgen.db import LeadDB
    from leadgen.pipeline import deduplicate_leads
    from leadgen.scoring import score_and_update_db
    from config import ICP

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
    from leadgen.pipeline import run_enrichment
    from leadgen.db import LeadDB
    from leadgen.scoring import score_and_update_db

    db = LeadDB()
    asyncio.run(run_enrichment(db, limit=args.limit))
    score_and_update_db(db)
    db.close()


def cmd_score(args):
    """Re-score all leads."""
    from leadgen.db import LeadDB
    from leadgen.scoring import score_and_update_db

    db = LeadDB()
    score_and_update_db(db)
    db.close()


def cmd_pipeline(args):
    """Run full pipeline."""
    from leadgen.pipeline import run_full_pipeline
    sources = [args.source] if args.source else None
    asyncio.run(run_full_pipeline(sources=sources, enrich=not args.no_enrich))


def cmd_export(args):
    """Export leads."""
    from leadgen.db import LeadDB
    from leadgen.export import export_csv, export_json

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
    from leadgen.db import LeadDB
    from leadgen.export import print_stats

    db = LeadDB()
    print_stats(db)
    db.close()


def cmd_dashboard(args):
    """Launch web dashboard."""
    from dashboard.app import create_app
    app = create_app()
    print(f"\n  🌐 Dashboard: http://127.0.0.1:{args.port}")
    app.run(host="127.0.0.1", port=args.port, debug=args.debug)


def cmd_collect(args):
    """Submit and run a stealth collection query."""
    from leadgen.job_runner import JobRunner
    from leadgen.db import LeadDB

    workspace_id = ""
    if args.workspace:
        db = LeadDB()
        # Find or create workspace
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
    from leadgen.db import LeadDB
    from leadgen.lead_validator import validate_lead

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
            # Check if email/phone was cleaned by validator
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
    from leadgen.db import LeadDB
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
    parser = argparse.ArgumentParser(description="Yupcha Lead Generation Pipeline")
    sub = parser.add_subparsers(dest="command", help="Command to run")

    # import
    sub.add_parser("import", help="Import existing CSVs")

    # scrape
    p = sub.add_parser("scrape", help="Run scrapers")
    p.add_argument("--source", help="Specific source: csv, google_maps, directories, linkedin, job_boards, review_dirs, google_search, news")
    p.add_argument("--city", help="Comma-separated cities")

    # enrich
    p = sub.add_parser("enrich", help="Enrich leads")
    p.add_argument("--limit", type=int, default=None, help="Limit leads to enrich")

    # score
    sub.add_parser("score", help="Re-score all leads")

    # pipeline
    p = sub.add_parser("pipeline", help="Full pipeline")
    p.add_argument("--source", help="Specific source")
    p.add_argument("--no-enrich", action="store_true", help="Skip enrichment")

    # export
    p = sub.add_parser("export", help="Export leads")
    p.add_argument("--format", choices=["csv", "json"], default="csv")
    p.add_argument("--output", help="Output file path")
    p.add_argument("--min-score", type=int, help="Minimum score filter")
    p.add_argument("--status", help="Status filter")
    p.add_argument("--city", help="City filter")
    p.add_argument("--tier", help="Score tier filter: hot, warm, cold")

    # stats
    sub.add_parser("stats", help="Show database stats")

    # dashboard
    p = sub.add_parser("dashboard", help="Launch web dashboard")
    p.add_argument("--port", type=int, default=5050)
    p.add_argument("--debug", action="store_true")

    # collect
    p = sub.add_parser("collect", help="Run stealth collection query")
    p.add_argument("query", help="Search query, e.g. 'HR staffing agency Bangalore'")
    p.add_argument("--workspace", "-w", help="Workspace name to collect into")

    # jobs
    p = sub.add_parser("jobs", help="List collection jobs")
    p.add_argument("--status", help="Filter by status: pending, running, done, failed")

    # cleanup
    sub.add_parser("cleanup", help="Purge bad/invalid leads from database")

    args = parser.parse_args()
    if not args.command:
        parser.print_help()
        return

    cmds = {
        "import": cmd_import, "scrape": cmd_scrape, "enrich": cmd_enrich,
        "score": cmd_score, "pipeline": cmd_pipeline, "export": cmd_export,
        "stats": cmd_stats, "dashboard": cmd_dashboard,
        "collect": cmd_collect, "jobs": cmd_jobs, "cleanup": cmd_cleanup,
    }
    cmds[args.command](args)


if __name__ == "__main__":
    main()

