from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.middleware.gzip import GZipMiddleware
from fastapi.staticfiles import StaticFiles
from slowapi import _rate_limit_exceeded_handler
from slowapi.errors import RateLimitExceeded
from apps.api.core.config import settings
from apps.api.core.ratelimit import limiter
from apps.api.database import check_and_migrate_db
from contextlib import asynccontextmanager
import logfire
import logging
import os

# Routers
from apps.api.routers import auth, users, tasks, crm, system, scraper, websockets, person_intel
from apps.api.routers import settings as settings_router
from apps.api.routers.analytics import router as analytics_router
from apps.api.routers.leads import router as leads_router, workspace_router, jobs_router, events_router, search_router
from apps.api.routers.copilotkit import router as copilotkit_router
from apps.api.routers.campaigns import router as campaigns_router
from apps.api.routers.workbooks import router as workbooks_router
from apps.api.routers.entities import router as entities_router
from apps.api.services.queue_service import queue_service
from apps.api.workers.download import handle_download_link

logger = logging.getLogger(__name__)

# Suppress noisy 3rd-party search engine logs (DDG tries 7 engines, logs every failure)
logging.getLogger("ddgs").setLevel(logging.WARNING)
logging.getLogger("ddgs.ddgs").setLevel(logging.WARNING)
logging.getLogger("primp").setLevel(logging.WARNING)

# Import all models so Base.metadata knows about them
from apps.api.services.workbook.models import Workbook, WorkbookEnrichment, WorkbookRow  # noqa: E402
# Pillar 1: canonical entity graph tables
from apps.api.services.entities import models as _entity_models  # noqa: E402,F401
# Pillar 2: provider performance ledger
from apps.api.services.workbook import planner_models as _planner_models  # noqa: E402,F401
# Pillar 3: living-workbook activity feed
from apps.api.services.workbook import activity_models as _activity_models  # noqa: E402,F401
# Pillar 4: agent column reasoning traces
from apps.api.services.workbook import trace_models as _trace_models  # noqa: E402,F401
# Outreach RLS-hardened tenant tables (+ non-RLS ticker mirror).
from apps.api.services.outreach import orm_models as _outreach_models  # noqa: E402,F401
# Intent-poller RLS-hardened tenant tables (+ non-RLS schedule mirror).
from apps.api.services.poller import models as _poller_models  # noqa: E402,F401

# Schema evolution is owned by Alembic: `alembic upgrade head` creates a fresh
# schema AND applies pending migrations on an existing DB. create_all() is only
# a guarded dev/test fallback (it never ALTERs existing tables). See db_init.py
# and MIGRATIONS.md.
from apps.api.db_init import init_db  # noqa: E402

init_db()

# Legacy SQLite-file migration. Now redundant for fresh DBs (Alembic's baseline
# already includes every column it adds) but kept idempotent + guarded so a
# pre-Alembic SQLite file picked up before this migration still gets its missing
# columns. Runs AFTER init_db so the tables it inspects already exist.
check_and_migrate_db()

# Tenancy fail-fast: when the shared Postgres lead store is active, verify the
# connection role is NOT superuser/BYPASSRLS — otherwise Row-Level Security is
# silently inert and the store would ship with isolation OFF. Honour the
# PG_RLS_REQUIRE_SAFE_ROLE toggle (default True = refuse to boot). On SQLite or
# when PG_LEAD_STORE is off this is a no-op.
from apps.api.database import IS_SQLITE as _IS_SQLITE  # noqa: E402

if not _IS_SQLITE and settings.PG_LEAD_STORE:
    from apps.api.services.leadgen.store import assert_rls_role  # noqa: E402

    # Raises RlsRoleError (refuse to boot) when strict and the role is unsafe;
    # otherwise logs CRITICAL and returns False (PG store disables itself).
    assert_rls_role(strict=settings.PG_RLS_REQUIRE_SAFE_ROLE)

# Lifespan — replaces deprecated @app.on_event("startup") / @app.on_event("shutdown")
@asynccontextmanager
async def lifespan(app: FastAPI):
    # ── Startup ──
    # In a real deployment get_settings() has already raised on the insecure
    # default key (fail-closed). Reaching here with it means we're in a tolerated
    # dev/test/local environment — warn loudly so it's never shipped silently.
    if settings.secret_key_is_insecure:
        logger.warning(
            "⚠ Using INSECURE default SECRET_KEY (APP_ENV=%s)! Set SECRET_KEY "
            "in .env before deploying to production.",
            settings.APP_ENV,
        )
    # Tenancy backfill — assign owner-less workspaces to the first admin so
    # existing data stays accessible after per-workspace isolation is enabled.
    try:
        from apps.api.database import SessionLocal
        from apps.api.models import User
        from apps.api.services.workspace.manager import ensure_tenancy_backfill

        _db = SessionLocal()
        try:
            admin = (
                _db.query(User)
                .filter((User.is_admin == True) | (User.role.in_(["admin", "superadmin"])))  # noqa: E712
                .order_by(User.id.asc())
                .first()
            )
            if admin:
                ensure_tenancy_backfill(admin.id)
                # Assign owner-less workbooks to the admin's main workspace.
                from apps.api.services.workspace.manager import _get_db as _ws_db
                from sqlalchemy import text as _text

                _wsconn = _ws_db()
                _main = _wsconn.execute(
                    "SELECT id FROM workspaces WHERE slug = 'main'"
                ).fetchone()
                _wsconn.close()
                if _main:
                    _db.execute(
                        _text("UPDATE workbooks SET workspace_id = :wid WHERE workspace_id IS NULL"),
                        {"wid": _main["id"]},
                    )
                    _db.commit()
            else:
                logger.warning("No admin user found — skipping tenancy backfill until one exists.")
        finally:
            _db.close()
    except Exception as e:
        logger.warning(f"Tenancy backfill skipped: {e}")
    queue_service.register_handler("download_link", handle_download_link)
    # Workbook enrichment runs on the durable queue worker (P-1): concurrent,
    # heartbeat-tracked, reaper-recoverable. See workbook/enrichment.py.
    from apps.api.services.workbook.enrichment import handle_run_workbook
    queue_service.register_handler("run_workbook", handle_run_workbook)
    # Source columns (P0): materialize rows from the source engine on the queue.
    from apps.api.services.workbook.source_engine import handle_source_workbook
    queue_service.register_handler("source_workbook", handle_source_workbook)
    # Living workbooks (P3): recurring refresh + signal-triggered refresh.
    from apps.api.services.workbook.refresh import handle_refresh_workbook, handle_signal_scan, bootstrap_signal_scan
    queue_service.register_handler("refresh_workbook", handle_refresh_workbook)
    queue_service.register_handler("signal_scan", handle_signal_scan)
    # Automations / Trigger Engine: tenant-scoped rule evaluation on the queue.
    from apps.api.services.automations.engine import handle_trigger_eval, bootstrap_schedules
    queue_service.register_handler("trigger_eval", handle_trigger_eval)
    # Outreach: tenant-scoped email send on the queue (at-most-once §6.2.1).
    from apps.api.services.outreach.sending import handle_send, bootstrap_outreach_schedules
    queue_service.register_handler("send", handle_send)
    # Outreach inbound: per-workspace IMAP poll for async DSN/complaint ingestion
    # (default OFF behind OUTREACH_INBOUND_POLL_ENABLED + per-ws IMAP creds).
    from apps.api.services.outreach.inbound import handle_inbound_poll, bootstrap_inbound_schedules
    queue_service.register_handler("outreach_inbound_poll", handle_inbound_poll)
    # Intent poller: tenant-scoped watch poll on the queue (default OFF).
    from apps.api.services.poller.engine import handle_watch_poll, bootstrap_watch_schedules
    queue_service.register_handler("watch_poll", handle_watch_poll)

    # Horizontal scaling: job processing is now safe to run in a SEPARATE worker
    # process (apps/api/worker.py) with an atomic FOR UPDATE SKIP LOCKED claim.
    # The in-API background worker is OPT-IN via RUN_INLINE_WORKER so we can turn
    # it OFF in production (where the standalone worker replicas own processing)
    # while keeping the single-process dev/test experience working by default.
    #   RUN_INLINE_WORKER unset / "1" / "true" → run the in-API worker (default)
    #   RUN_INLINE_WORKER "0" / "false"        → don't; rely on the worker service
    _inline = os.getenv("RUN_INLINE_WORKER", "1").strip().lower()
    run_inline_worker = _inline not in ("0", "false", "no", "off")
    if run_inline_worker:
        await queue_service.start_worker()
        print("✓ Queue Worker Started (in-API)")
    else:
        print("✓ In-API worker disabled (RUN_INLINE_WORKER=0) — using separate worker process")

    # Kick off the recurring signal scan (idempotent; no-op if already pending).
    # Safe regardless of who processes it — it just enqueues a durable job that
    # the in-API worker OR a standalone worker replica will pick up.
    try:
        bootstrap_signal_scan()
    except Exception as e:
        logger.warning(f"signal_scan bootstrap skipped: {e}")
    # Cold-start the on_schedule automations from the non-RLS mirror (no-op when
    # AUTOMATIONS_ENABLED is off). Survives restarts; single-flight guarded.
    try:
        bootstrap_schedules()
    except Exception as e:
        logger.warning(f"trigger schedule bootstrap skipped: {e}")
    # Cold-start the autonomous outreach ticker from its non-RLS mirror (no-op
    # when AUTOMATIONS_ENABLED is off). Survives restarts; single-flight guarded.
    try:
        bootstrap_outreach_schedules()
    except Exception as e:
        logger.warning(f"outreach schedule bootstrap skipped: {e}")
    # Cold-start the inbound bounce/complaint IMAP poller from its non-RLS mirror
    # (no-op when OUTREACH_INBOUND_POLL_ENABLED is off). Survives restarts.
    try:
        bootstrap_inbound_schedules()
    except Exception as e:
        logger.warning(f"outreach inbound bootstrap skipped: {e}")
    # Cold-start the intent poller from its non-RLS mirror (no-op when
    # INTENT_POLLER_ENABLED is off / not PG). Survives restarts; single-flight.
    try:
        bootstrap_watch_schedules()
    except Exception as e:
        logger.warning(f"intent poller bootstrap skipped: {e}")
    print("✓ Yupcha Engine v3.0 Ready")
    yield
    # ── Shutdown ──
    if run_inline_worker:
        await queue_service.stop_worker()
        print("✓ Queue Worker Stopped")


app = FastAPI(title="Yupcha Engine", version="3.0.0", lifespan=lifespan)

app.state.limiter = limiter
app.add_exception_handler(RateLimitExceeded, _rate_limit_exceeded_handler)

logfire.instrument_fastapi(app)

# Middleware
app.add_middleware(GZipMiddleware, minimum_size=1000)
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=False,
    allow_methods=["*"],
    allow_headers=["*"],
)

# Include Routers — Existing
app.include_router(auth.router)
app.include_router(users.router)
app.include_router(tasks.router)
app.include_router(crm.router)
app.include_router(system.router)
app.include_router(scraper.router)
app.include_router(websockets.router)
app.include_router(person_intel.router)
app.include_router(settings_router.router)
app.include_router(analytics_router)

# Include Routers — Lead Pipeline
app.include_router(leads_router)
# NOTE: leads_router.workspace_router (legacy, leads.db-backed) is intentionally
# NOT mounted — the canonical /api/workspaces is ws_manager_router (tenant-aware,
# workspaces.db with ownership + membership). See routers/workspace_manager.py.
app.include_router(jobs_router)
app.include_router(events_router)
app.include_router(search_router)
app.include_router(copilotkit_router)
app.include_router(campaigns_router)
app.include_router(workbooks_router)
app.include_router(entities_router)

# Include Routers — Outreach
from apps.api.routers.outreach import router as outreach_router
app.include_router(outreach_router)

# Include Routers — CRM
from apps.api.routers.hubspot import router as hubspot_router
app.include_router(hubspot_router)

# Include Routers — Signals
from apps.api.routers.signals import router as signals_router
app.include_router(signals_router)

# Include Routers — Workspace Manager
from apps.api.routers.workspace_manager import router as ws_manager_router
app.include_router(ws_manager_router)

# Include Routers — Templates & Functions
from apps.api.routers.templates import router as templates_router
from apps.api.routers.functions import router as functions_router
app.include_router(templates_router)
app.include_router(functions_router)

# Include Routers — Data Sources
from apps.api.routers.ambitionbox import router as ambitionbox_router
app.include_router(ambitionbox_router)

# Billing — credit ledger + Stripe top-ups (WI-9)
from apps.api.routers.billing import router as billing_router
app.include_router(billing_router)

# Automations / Trigger Engine — tenant-scoped rules (router 404s when disabled)
from apps.api.routers.automations import router as automations_router
app.include_router(automations_router)

# Intent-Signal Poller — tenant-scoped watch subscriptions (404s when disabled)
from apps.api.routers.watches import router as watches_router
app.include_router(watches_router)

# Meta — per-workspace role + feature flags for proactive UI gating
from apps.api.routers.meta import router as meta_router
app.include_router(meta_router)


@app.get("/api")
def api_root():
    return {"status": "ok", "engine": "Yupcha Engine", "version": "3.0.0"}


@app.get("/health")
def health_check():
    return {"status": "healthy", "engine": "Yupcha Engine", "version": "3.0.0"}


# Serve frontend static files (if built) — MUST be after all API routes
# because it mounts at "/" and would swallow unmatched paths
web_dist = os.path.join(os.path.dirname(__file__), "..", "web", "dist")
if os.path.isdir(web_dist):
    app.mount("/", StaticFiles(directory=web_dist, html=True), name="frontend")

