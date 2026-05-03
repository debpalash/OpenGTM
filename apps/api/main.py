from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.middleware.gzip import GZipMiddleware
from fastapi.staticfiles import StaticFiles
from slowapi import Limiter, _rate_limit_exceeded_handler
from slowapi.util import get_remote_address
from slowapi.errors import RateLimitExceeded
from apps.api.core.config import settings
from apps.api.database import Base, engine, check_and_migrate_db
import logfire
import os

# Routers
from apps.api.routers import auth, users, tasks, crm, system, scraper, websockets, person_intel, settings
from apps.api.routers.leads import router as leads_router, workspace_router, jobs_router, events_router, search_router
from apps.api.routers.copilotkit import router as copilotkit_router
from apps.api.services.queue_service import queue_service
from apps.api.workers.download import handle_download_link

# Database Migration
check_and_migrate_db()
Base.metadata.create_all(bind=engine)

# Initialize Limiter
limiter = Limiter(key_func=get_remote_address)
app = FastAPI(title="Yupcha Engine", version="3.0.0")

app.state.limiter = limiter
app.add_exception_handler(RateLimitExceeded, _rate_limit_exceeded_handler)

logfire.instrument_fastapi(app)

# Middleware
app.add_middleware(GZipMiddleware, minimum_size=1000)
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)


# Startup
@app.on_event("startup")
async def startup_event():
    # Register Workers
    queue_service.register_handler("download_link", handle_download_link)
    await queue_service.start_worker()
    print("✓ Queue Worker Started")
    print("✓ Yupcha Engine v3.0 Ready")


@app.on_event("shutdown")
async def shutdown_event():
    await queue_service.stop_worker()
    print("✓ Queue Worker Stopped")


# Include Routers — Existing
app.include_router(auth.router)
app.include_router(users.router)
app.include_router(tasks.router)
app.include_router(crm.router)
app.include_router(system.router)
app.include_router(scraper.router)
app.include_router(websockets.router)
app.include_router(person_intel.router)
app.include_router(settings.router)

# Include Routers — Lead Pipeline
app.include_router(leads_router)
app.include_router(workspace_router)
app.include_router(jobs_router)
app.include_router(events_router)
app.include_router(search_router)
app.include_router(copilotkit_router)


# Serve frontend static files (if built)
web_dist = os.path.join(os.path.dirname(__file__), "..", "web", "dist")
if os.path.isdir(web_dist):
    app.mount("/", StaticFiles(directory=web_dist, html=True), name="frontend")


@app.get("/api")
def api_root():
    return {"status": "ok", "engine": "Yupcha Engine", "version": "3.0.0"}

