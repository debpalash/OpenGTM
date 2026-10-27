from __future__ import annotations

import asyncio
from contextlib import asynccontextmanager

import httpx
import pytest
import pytest_asyncio

from browser_service.app import create_app
from browser_service.config import Settings

TOKEN = "t" * 40
AUTH = {"Authorization": f"Bearer {TOKEN}"}


class FakeResponse:
    def __init__(self, status=200, headers=None):
        self.status = status
        self.headers = headers or {}


class EmptyLocator:
    async def count(self):
        return 0

    async def all(self):
        return []


class FakePage:
    """Just enough of playwright's Page for the engines."""

    def __init__(self, pool, session):
        self.pool = pool
        self.session = session
        self.url = ""

    async def goto(self, url, wait_until=None, timeout=None):
        self.pool.calls.append(("goto", url, wait_until))
        if self.pool.goto_delay:
            await asyncio.sleep(self.pool.goto_delay)
        if self.pool.goto_error is not None:
            if self.pool.block_on_error:
                self.session.record_blocked("evil.test")
            raise self.pool.goto_error
        self.url = self.pool.final_url or url
        return FakeResponse(self.pool.status, self.pool.headers)

    async def wait_for_timeout(self, ms):
        self.pool.calls.append(("wait", ms))

    async def evaluate(self, js):
        self.pool.calls.append(("evaluate", js))
        return 1000

    def locator(self, selector):
        self.pool.calls.append(("locator", selector))
        return EmptyLocator()

    async def content(self):
        return self.pool.html

    async def screenshot(self, **kw):
        self.pool.calls.append(("screenshot", kw))
        return self.pool.image


class FakePool:
    def __init__(self):
        self.ready = True
        self.calls = []
        self.html = (
            "<html><head><title>Hello</title><meta name='description' content='Desc'></head>"
            "<body><h1>Head</h1><p>Body text</p><script>x()</script></body></html>"
        )
        self.status = 200
        self.headers = {}
        self.final_url = None
        self.goto_error = None
        self.block_on_error = False
        self.goto_delay = 0.0
        self.image = b"\xff\xd8jpeg"
        self.page_kwargs = []
        self.sessions = []

    async def start(self):
        pass

    async def stop(self):
        pass

    @asynccontextmanager
    async def page(self, session, **kw):
        self.page_kwargs.append(kw)
        self.sessions.append(session)
        yield FakePage(self, session)


def make_settings(**kw) -> Settings:
    base = dict(token=TOKEN, max_timeout_s=30.0, queue_wait_s=0.2)
    base.update(kw)
    return Settings(**base)


@pytest.fixture
def pool():
    return FakePool()


@pytest_asyncio.fixture
async def make_client(pool):
    """Factory: an httpx client on the ASGI app with its lifespan running."""
    stack = []

    async def factory(search_fn=None, deps_kw=None, **settings_kw):
        app = create_app(make_settings(**settings_kw), pool=pool, search_fn=search_fn, **(deps_kw or {}))
        cm = app.router.lifespan_context(app)
        await cm.__aenter__()
        client = httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://svc")
        stack.append((cm, client))
        return client

    yield factory
    for cm, client in stack:
        await client.aclose()
        await cm.__aexit__(None, None, None)
