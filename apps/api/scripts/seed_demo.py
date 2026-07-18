"""
First-run demo seed — idempotent.

Goal: after `docker compose up`, a brand-new user can log in and immediately
land on a POPULATED, self-filling demo workbook using ONLY zero-key (free)
providers. No API keys required.

What this script does (all guarded so re-running / restarts never duplicate):
  1. Ensure the schema exists (Base.metadata.create_all).
  2. Ensure an admin user exists so the login screen is usable
     (default: admin / admin — override with SEED_ADMIN_USERNAME /
     SEED_ADMIN_PASSWORD). Printed once on first creation.
  3. Ensure the "main" workspace exists and is owned by that admin
     (reuses the app's own workspace manager + tenancy backfill).
  4. Create a demo workbook (~25 real companies) wired to zero-key waterfall
     columns + a research column. Columns leave `waterfall` empty so the
     engine's DEFAULT_WATERFALLS (free OSS scrapers first) apply.
  5. Enqueue a `run_workbook` job so the API's in-process queue worker fills
     the grid automatically — the user watches cells populate live.

Run:
    python -m apps.api.scripts.seed_demo

Invoked automatically by the compose `seed` service. Safe to run by hand.
"""

import logging
import os
import sys

# Make `apps.api...` importable when run as a module or a file.
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "..")))

logging.basicConfig(level=logging.INFO, format="%(asctime)s [seed] %(levelname)s: %(message)s")
log = logging.getLogger("seed_demo")

DEMO_WORKBOOK_NAME = "Demo — Zero-Key Enrichment"
# Stable id so re-running never creates a second copy even if the name changes.
DEMO_WORKBOOK_ID = "demo-zero-key-firstrun"


# ── 25 well-known companies with public sites (great for free scrapers) ──────
DEMO_COMPANIES = [
    ("Stripe", "stripe.com"),
    ("Notion", "notion.so"),
    ("Figma", "figma.com"),
    ("Vercel", "vercel.com"),
    ("Linear", "linear.app"),
    ("Supabase", "supabase.com"),
    ("HashiCorp", "hashicorp.com"),
    ("GitLab", "gitlab.com"),
    ("Cloudflare", "cloudflare.com"),
    ("MongoDB", "mongodb.com"),
    ("Datadog", "datadoghq.com"),
    ("Snowflake", "snowflake.com"),
    ("Twilio", "twilio.com"),
    ("Atlassian", "atlassian.com"),
    ("Shopify", "shopify.com"),
    ("DigitalOcean", "digitalocean.com"),
    ("Elastic", "elastic.co"),
    ("Confluent", "confluent.io"),
    ("PagerDuty", "pagerduty.com"),
    ("Auth0", "auth0.com"),
    ("Segment", "segment.com"),
    ("Algolia", "algolia.com"),
    ("Sentry", "sentry.io"),
    ("Postman", "postman.com"),
    ("Asana", "asana.com"),
]


def _demo_columns() -> list:
    """Zero-key column set. Enrichment columns leave `waterfall` empty so the
    engine's DEFAULT_WATERFALLS (free OSS scrapers first) drive each target.

    Every target_field below resolves to a chain whose leading providers need
    NO API key: deep_scraper, jsonld_firmographics, website_scraper,
    email_harvester, ddg_*, holehe, company_intel, wikidata, local_business,
    ats_hiring, social_finder, crosslinked, decision_maker.
    """
    return [
        {"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company", "width": 160},
        {"id": "website", "name": "Website", "type": "lead_field", "lead_field": "website", "width": 180},
        # Email — website_scraper / email_harvester / ddg_email (all keyless).
        {"id": "find_email", "name": "Email", "type": "waterfall", "target_field": "email", "width": 220},
        # Email verification — holehe (120+ site presence check), keyless.
        {"id": "verify_email", "name": "Email Verify", "type": "waterfall",
         "target_field": "email_verify", "width": 140},
        # Phone — website_scraper / local_business / ddg_company (keyless).
        {"id": "find_phone", "name": "Phone", "type": "waterfall", "target_field": "phone", "width": 150},
        # Firmographics description — jsonld_firmographics / company_intel / wikidata.
        {"id": "company_desc", "name": "Description", "type": "waterfall",
         "target_field": "description", "width": 280},
        # Company size — website_scraper / company_intel / wikidata (keyless).
        {"id": "company_size", "name": "Size", "type": "waterfall",
         "target_field": "company_size", "width": 120},
        # LinkedIn — jsonld_firmographics / social_finder / wikidata (keyless).
        {"id": "linkedin", "name": "LinkedIn", "type": "waterfall",
         "target_field": "linkedin_url", "width": 200},
        # Hiring signals — ats_hiring / jobspy (keyless public ATS boards).
        {"id": "hiring", "name": "Hiring Signals", "type": "waterfall",
         "target_field": "hiring_signals", "width": 180},
        # Research column — web-research agent answers a question per row.
        {"id": "research_hq", "name": "HQ Location (research)", "type": "research",
         "prompt": "What city and country is {company} ({website}) headquartered in? Answer with just the city and country.",
         "output_format": "text", "max_steps": 3, "width": 220},
    ]


def ensure_admin(db) -> int:
    """Create a default admin so the login screen works out of the box.

    Idempotent: if ANY user already exists, do nothing. Returns an admin id
    (existing or newly created)."""
    from apps.api.models import User
    from apps.api.auth import get_password_hash

    existing_admin = (
        db.query(User)
        .filter((User.is_admin == True) | (User.role.in_(["admin", "superadmin"])))  # noqa: E712
        .order_by(User.id.asc())
        .first()
    )
    if existing_admin:
        return existing_admin.id

    if db.query(User).count() > 0:
        # Users exist but none are admin — promote the first one rather than
        # injecting credentials into someone else's deployment.
        first = db.query(User).order_by(User.id.asc()).first()
        first.is_admin = True
        first.role = "admin"
        db.commit()
        log.info(f"Promoted existing user '{first.username}' to admin.")
        return first.id

    username = os.getenv("SEED_ADMIN_USERNAME", "admin")
    password = os.getenv("SEED_ADMIN_PASSWORD", "admin")
    user = User(
        username=username,
        hashed_password=get_password_hash(password),
        is_active=True,
        is_admin=True,
        role="admin",
    )
    db.add(user)
    db.commit()
    db.refresh(user)
    log.info("=" * 60)
    log.info(f"  Created demo admin login → username: {username}  password: {password}")
    log.info("  Change this immediately for any non-local deployment.")
    log.info("=" * 60)
    return user.id


def ensure_main_workspace(admin_id: int) -> str:
    """Ensure the 'main' workspace exists and is owned by the admin. Returns its id."""
    from apps.api.services.workspace import manager as ws

    # _get_db() creates the 'main' workspace if missing.
    conn = ws._get_db()
    row = conn.execute("SELECT id FROM workspaces WHERE slug = 'main'").fetchone()
    conn.close()
    main_id = row["id"]

    # Make the admin the owner + member of every existing workspace (idempotent).
    ws.ensure_tenancy_backfill(admin_id)
    return main_id


def seed_demo_workbook(db, workspace_id: str) -> bool:
    """Create the demo workbook + rows if it doesn't already exist.

    Returns True if a NEW workbook was created (caller then enqueues a run)."""
    from apps.api.services.workbook.models import Workbook, WorkbookRow

    if db.query(Workbook).filter(Workbook.id == DEMO_WORKBOOK_ID).first():
        log.info("Demo workbook already present — skipping seed (idempotent).")
        return False

    wb = Workbook(
        id=DEMO_WORKBOOK_ID,
        name=DEMO_WORKBOOK_NAME,
        description="Auto-seeded first-run demo. Every column uses FREE, zero-key "
                    "providers — no API keys required. Click any cell to see the "
                    "provider waterfall.",
        status="draft",
        workspace_id=workspace_id,
        source_type="empty",
        columns_config=_demo_columns(),
        total_rows=len(DEMO_COMPANIES),
        completed_rows=0,
        sync_to_leads=False,
    )
    db.add(wb)
    db.flush()  # ensure FK target exists before rows

    for i, (name, domain) in enumerate(DEMO_COMPANIES):
        db.add(WorkbookRow(
            workbook_id=DEMO_WORKBOOK_ID,
            workspace_id=workspace_id,
            position=i,
            data={"company": name, "website": domain},
            enrichments={},
        ))
    db.commit()
    log.info(f"Seeded demo workbook '{DEMO_WORKBOOK_NAME}' with {len(DEMO_COMPANIES)} rows.")
    return True


def trigger_first_run(db, workspace_id: str) -> None:
    """Enqueue a run_workbook job so the API's in-process queue worker fills
    the grid automatically. Guarded against duplicate pending runs."""
    from apps.api.models import Job
    from apps.api.services.workbook.models import Workbook

    wb = db.query(Workbook).filter(Workbook.id == DEMO_WORKBOOK_ID).first()
    if not wb:
        return

    # Don't stack runs if one is already queued/running for this workbook.
    pending = (
        db.query(Job)
        .filter(Job.type == "run_workbook", Job.status.in_(["pending", "processing"]))
        .all()
    )
    for j in pending:
        if (j.payload or {}).get("workbook_id") == DEMO_WORKBOOK_ID:
            log.info("A run for the demo workbook is already queued — not re-enqueuing.")
            return

    enrichment_col_ids = [
        c["id"] for c in (wb.columns_config or [])
        if c.get("type") in ("enrichment", "waterfall", "ai_formula", "output",
                             "research", "agent", "http", "formula")
    ]
    from apps.api.services.queue_service import queue_service
    queue_service.add_job(
        db,
        "run_workbook",
        {
            "workbook_id": DEMO_WORKBOOK_ID,
            # OD-4: stamp the tenant into the payload so the worker enters the
            # right workspace_scope — handle_run_workbook fails loud without it.
            "workspace_id": workspace_id,
            "column_ids": enrichment_col_ids,
            "row_ids": None,
            "lead_ids": None,
            "concurrency": int(os.getenv("WORKER_CONCURRENCY", "5")),
            "max_providers": 0,
            "retry_passes": 1,
            "provider_workers": 4,
            "provider_timeout": 15,
            "fill_missing": False,
        },
    )
    wb.status = "running"
    db.commit()
    log.info("Enqueued first enrichment run — the demo grid will fill automatically.")


def main() -> int:
    # 1. Schema. Use the SAME path the app boots with (db_init.init_db) so the
    #    DB is brought up via Alembic (and gets the alembic_version stamp).
    #    Calling Base.metadata.create_all() directly here was a bug: when the
    #    seed runs on a FRESH Postgres BEFORE the API has booted (the documented
    #    "safe to run by hand" path, or any deploy that seeds first), create_all
    #    builds every table WITHOUT stamping alembic_version. A later
    #    `alembic upgrade head` (e.g. the API booting afterwards) then fails with
    #    DuplicateTable because it tries to CREATE tables that already exist.
    #    init_db() runs `alembic upgrade head` (idempotent on an already-migrated
    #    DB) and only falls back to create_all() when Alembic is unavailable.
    from apps.api.database import SessionLocal
    from apps.api.db_init import init_db

    init_db()
    log.info("Schema ensured (db_init / alembic upgrade head).")

    db = SessionLocal()
    try:
        admin_id = ensure_admin(db)
        workspace_id = ensure_main_workspace(admin_id)
        created = seed_demo_workbook(db, workspace_id)
        if created or os.getenv("SEED_FORCE_RUN") == "1":
            trigger_first_run(db, workspace_id)
    finally:
        db.close()

    log.info("Seed complete.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
