"""Regression: /api/auth/workspace-context must not wedge under concurrency.

The M0 benchmark found that with two uvicorn workers the authenticated route
stopped answering at 64 connections (32 per worker, more than the 30
connections a worker's SQLAlchemy pool can lend). The cause was an async/sync
boundary bug, not load: the async auth dependencies ran blocking calls (the
user query on the request Session, and the workspace metadata lookups) directly
on the event loop. A Session holds its pooled connection until the ``get_db``
dependency closes it, and that close can only run after the event loop sends the
response. Once more requests than the pool could lend had reached their first
query, the event loop itself blocked in ``Pool.connect`` waiting for a
connection that only the (now blocked) event loop could release, until
``pool_timeout`` expired, and the queue behind it repeated the stall.

This test drives the real dependency chain in a single event loop (exactly as
uvicorn does) through a pool far smaller than the number of in-flight requests.
Before the fix a large share of requests failed with a pool timeout and the run
took many multiples of ``pool_timeout``; afterwards every request succeeds
because waiting for a connection no longer blocks the loop.
"""

import asyncio
import time
from pathlib import Path

import httpx
import pytest
from fastapi import FastAPI
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import QueuePool

from apps.api.auth import create_access_token
from apps.api.database import Base, get_db
from apps.api.models import User
from apps.api.routers.auth import api_router
from apps.api.services.workspace import manager

POOL_SIZE = 2
POOL_TIMEOUT = 3
REQUESTS = 24  # many times what the pool can lend at once


@pytest.fixture()
def app_and_ws(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    engine = create_engine(
        f"sqlite:///{tmp_path}/users.db",
        poolclass=QueuePool,
        pool_size=POOL_SIZE,
        max_overflow=0,
        pool_timeout=POOL_TIMEOUT,
        connect_args={"check_same_thread": False},
    )
    Base.metadata.create_all(engine, tables=[User.__table__])
    Session = sessionmaker(bind=engine, autocommit=False, autoflush=False)
    with Session() as db:
        db.add(User(id=1, username="bench", hashed_password="x", is_active=True))
        db.commit()
    ws = manager.create_workspace("Bench", owner_id=1)

    def _db():  # same shape as apps.api.database.get_db
        db = Session()
        try:
            yield db
        finally:
            db.close()

    app = FastAPI()
    app.include_router(api_router)
    app.dependency_overrides[get_db] = _db
    yield app, ws
    engine.dispose()


def test_workspace_context_does_not_wedge_when_requests_exceed_the_pool(app_and_ws):
    app, ws = app_and_ws
    headers = {"Authorization": f"Bearer {create_access_token({'sub': 'bench', 'amr': ['pwd']})}"}

    async def drive():
        transport = httpx.ASGITransport(app=app, raise_app_exceptions=False)
        async with httpx.AsyncClient(transport=transport, base_url="http://t", timeout=60) as client:
            return await asyncio.gather(
                *(client.get("/api/auth/workspace-context", headers=headers) for _ in range(REQUESTS))
            )

    started = time.monotonic()
    responses = asyncio.run(drive())
    elapsed = time.monotonic() - started

    statuses = [r.status_code for r in responses]
    assert statuses == [200] * REQUESTS, f"non-200 responses: {sorted(set(statuses))} ({statuses.count(500)} x 500)"
    assert all(r.json()["workspace_id"] == ws.id for r in responses)
    # No request may have sat out a pool timeout.
    assert elapsed < POOL_TIMEOUT, f"took {elapsed:.1f}s; the event loop was blocked waiting for pool connections"
