from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.middleware.gzip import GZipMiddleware
from fastapi.staticfiles import StaticFiles
from slowapi import _rate_limit_exceeded_handler
from slowapi.errors import RateLimitExceeded
from apps.api.core.config import settings
from apps.api.core.ratelimit import limiter
from apps.api.database import Base, engine, check_and_migrate_db
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

# Database Migration
check_and_migrate_db()

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

Base.metadata.create_all(bind=engine)

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
    await queue_service.start_worker()
    # Kick off the recurring signal scan (idempotent; no-op if already pending).
    try:
        bootstrap_signal_scan()
    except Exception as e:
        logger.warning(f"signal_scan bootstrap skipped: {e}")
    print("✓ Queue Worker Started")
    print("✓ Yupcha Engine v3.0 Ready")
    yield
    # ── Shutdown ──
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

