from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.middleware.gzip import GZipMiddleware
from fastapi.staticfiles import StaticFiles
from slowapi import Limiter, _rate_limit_exceeded_handler
from slowapi.util import get_remote_address
from slowapi.errors import RateLimitExceeded
from apps.api.core.config import settings
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
from apps.api.services.queue_service import queue_service
from apps.api.workers.download import handle_download_link

logger = logging.getLogger(__name__)

# Database Migration
check_and_migrate_db()

# Import all models so Base.metadata knows about them
from apps.api.services.workbook.models import Workbook, WorkbookEnrichment  # noqa: E402

Base.metadata.create_all(bind=engine)

# Initialize Limiter
limiter = Limiter(key_func=get_remote_address)


# Lifespan — replaces deprecated @app.on_event("startup") / @app.on_event("shutdown")
@asynccontextmanager
async def lifespan(app: FastAPI):
    # ── Startup ──
    if "INSECURE_FALLBACK" in settings.SECRET_KEY:
        logger.warning(
            "⚠ Using INSECURE default SECRET_KEY! Set SECRET_KEY in .env for production."
        )
    queue_service.register_handler("download_link", handle_download_link)
    await queue_service.start_worker()
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
app.include_router(workspace_router)
app.include_router(jobs_router)
app.include_router(events_router)
app.include_router(search_router)
app.include_router(copilotkit_router)
app.include_router(campaigns_router)
app.include_router(workbooks_router)

# Include Routers — Outreach
from apps.api.routers.outreach import router as outreach_router
app.include_router(outreach_router)

# Include Routers — CRM
from apps.api.routers.hubspot import router as hubspot_router
app.include_router(hubspot_router)

# Include Routers — Signals
from apps.api.routers.signals import router as signals_router
app.include_router(signals_router)

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

