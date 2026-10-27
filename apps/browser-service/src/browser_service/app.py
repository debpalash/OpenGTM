"""HTTP API (FastAPI). Everything under /v1 requires the shared bearer token;
/healthz is an unauthenticated liveness probe that reveals nothing.

Caps: request bodies (BROWSER_MAX_REQUEST_BYTES), concurrent browser/search
work (BROWSER_MAX_CONCURRENCY, callers beyond it wait up to
BROWSER_QUEUE_WAIT_S and then get 429 busy), per-request deadline (the
request's timeout_s, clamped to BROWSER_MAX_TIMEOUT_S) and response size
(BROWSER_MAX_RESPONSE_BYTES)."""

from __future__ import annotations

import asyncio
import hmac
import json
import logging
from collections.abc import Awaitable, Callable
from contextlib import asynccontextmanager
from typing import Any, TypeVar

from fastapi import APIRouter, Depends, FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.openapi.utils import get_openapi
from fastapi.responses import JSONResponse
from starlette.exceptions import HTTPException as StarletteHTTPException

from . import __version__, engines
from .browser import BrowserPool
from .config import Settings
from .egress import EgressPolicy, EgressProxy
from .engines import Deadline, Deps
from .errors import ServiceError
from .models import (
    CrunchbaseRequest,
    ErrorEnvelope,
    GoogleMapsRequest,
    GoogleMapsResult,
    GoogleSearchRequest,
    Health,
    JobBoardsRequest,
    JustDialPageRequest,
    JustDialPageResult,
    LinkedInRequest,
    Liveness,
    RenderRequest,
    RenderResult,
    ReviewDirectoriesRequest,
    ScreenshotRequest,
    ScreenshotResult,
    SearchResult,
    WebDirectoriesRequest,
)
from .scrapers import google_maps, justdial, search

logger = logging.getLogger("browser_service.app")

T = TypeVar("T")
GRACE_S = 2.0


class Limiter:
    def __init__(self, capacity: int, wait_s: float) -> None:
        self.capacity = capacity
        self.wait_s = wait_s
        self.active = 0
        self._sem = asyncio.Semaphore(capacity)

    @asynccontextmanager
    async def slot(self, deadline: Deadline):
        try:
            await asyncio.wait_for(self._sem.acquire(), timeout=min(self.wait_s, deadline.left()))
        except TimeoutError as e:
            raise ServiceError("busy") from e
        self.active += 1
        try:
            yield
        finally:
            self.active -= 1
            self._sem.release()


class BodyLimit:
    """Pure ASGI: refuse request bodies above the cap (declared or streamed)."""

    def __init__(self, app: Any, max_bytes: int) -> None:
        self.app = app
        self.max_bytes = max_bytes

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http":
            return await self.app(scope, receive, send)
        for name, value in scope.get("headers", []):
            if name == b"content-length":
                try:
                    too_big = int(value) > self.max_bytes
                except ValueError:
                    too_big = True
                if too_big:
                    resp = JSONResponse(ServiceError("too_large").body(), status_code=413)
                    return await resp(scope, receive, send)
        seen = 0

        async def limited():
            nonlocal seen
            msg = await receive()
            if msg["type"] == "http.request":
                seen += len(msg.get("body", b""))
                if seen > self.max_bytes:
                    raise ServiceError("too_large")
            return msg

        return await self.app(scope, limited, send)


ERROR_RESPONSES: dict[int | str, dict[str, Any]] = {
    code: {"model": ErrorEnvelope} for code in (400, 401, 403, 413, 429, 500, 502, 503, 504)
}


def create_app(
    settings: Settings,
    *,
    pool: Any = None,
    search_fn: Any = None,
    policy: EgressPolicy | None = None,
) -> FastAPI:
    policy = policy or EgressPolicy(allow_cidrs=settings.egress_allow_cidrs)
    proxy = EgressProxy(policy)
    pool = pool if pool is not None else BrowserPool(proxy)
    deps = Deps(settings=settings, policy=policy, proxy=proxy, pool=pool, search=search_fn)
    limiter = Limiter(settings.max_concurrency, settings.queue_wait_s)
    token = settings.token.encode()

    @asynccontextmanager
    async def lifespan(_app: FastAPI):
        await proxy.start()
        if settings.launch_browser:
            await pool.start()
        try:
            yield
        finally:
            await pool.stop()
            await proxy.close()

    app = FastAPI(
        title="OpenGTM browser service",
        version=__version__,
        lifespan=lifespan,
        docs_url=None,
        redoc_url=None,
        openapi_url=None,
    )
    app.state.deps = deps
    app.state.limiter = limiter
    app.add_middleware(BodyLimit, max_bytes=settings.max_request_bytes)

    async def require_token(request: Request) -> None:
        scheme, _, presented = request.headers.get("authorization", "").partition(" ")
        if scheme.lower() != "bearer" or not hmac.compare_digest(presented.strip().encode(), token):
            raise ServiceError("unauthorized")

    async def run(timeout_s: float, fn: Callable[[Deadline], Awaitable[T]]) -> T:
        budget = min(timeout_s, settings.max_timeout_s)
        deadline = Deadline(budget)
        async with limiter.slot(deadline):
            try:
                async with asyncio.timeout(budget + GRACE_S):
                    return await fn(deadline)
            except TimeoutError as e:
                raise ServiceError("timeout") from e

    @app.exception_handler(ServiceError)
    async def _service_error(_r: Request, exc: ServiceError):
        return JSONResponse(exc.body(), status_code=exc.status)

    @app.exception_handler(RequestValidationError)
    async def _validation(_r: Request, exc: RequestValidationError):
        body = ServiceError("bad_request").body()
        body["error"]["fields"] = [
            ".".join(str(p) for p in e.get("loc", ()) if p != "body") or "body" for e in exc.errors()[:10]
        ]
        return JSONResponse(body, status_code=400)

    @app.exception_handler(StarletteHTTPException)
    async def _http(_r: Request, exc: StarletteHTTPException):
        code = {404: "not_found", 405: "method_not_allowed", 413: "too_large"}.get(exc.status_code, "bad_request")
        err = ServiceError(code)
        return JSONResponse(err.body(), status_code=err.status)

    @app.exception_handler(Exception)
    async def _unhandled(_r: Request, exc: Exception):
        logger.error("unhandled error: %s", type(exc).__name__)
        return JSONResponse(ServiceError("internal").body(), status_code=500)

    @app.get("/healthz", response_model=Liveness, include_in_schema=True)
    async def healthz() -> Liveness:
        return Liveness(status="ok")

    v1 = APIRouter(prefix="/v1", dependencies=[Depends(require_token)], responses=ERROR_RESPONSES)

    @v1.get("/health", response_model=Health)
    async def health() -> Health:
        if not settings.launch_browser:
            browser = "disabled"
        else:
            browser = "ready" if pool.ready else "down"
        engines_on = ["browser", "impersonate"] + (["flaresolverr"] if settings.flaresolverr_url else [])
        return Health(
            status="ok" if browser != "down" else "degraded",
            version=__version__,
            browser=browser,
            active=limiter.active,
            capacity=limiter.capacity,
            engines=engines_on,
        )

    @v1.post("/render", response_model=RenderResult)
    async def render(req: RenderRequest) -> RenderResult:
        return await run(req.timeout_s, lambda d: engines.render(deps, req, d))

    @v1.post("/screenshot", response_model=ScreenshotResult)
    async def screenshot(req: ScreenshotRequest) -> ScreenshotResult:
        return await run(req.timeout_s, lambda d: engines.screenshot(deps, req, d))

    @v1.post("/scrape/google_maps", response_model=GoogleMapsResult)
    async def scrape_google_maps(req: GoogleMapsRequest) -> GoogleMapsResult:
        return await run(req.timeout_s, lambda d: google_maps.scrape(deps, req, d))

    @v1.post("/scrape/justdial_page", response_model=JustDialPageResult)
    async def scrape_justdial_page(req: JustDialPageRequest) -> JustDialPageResult:
        return await run(req.timeout_s, lambda d: justdial.scrape(deps, req, d))

    def search_route(source: str, model: type) -> None:
        async def handler(req):
            return await run(req.timeout_s, lambda d: search.scrape(deps, source, req, d))

        handler.__name__ = f"scrape_{source}"
        handler.__annotations__ = {"req": model, "return": SearchResult}
        v1.add_api_route(
            f"/scrape/{source}", handler, methods=["POST"], response_model=SearchResult, name=f"scrape_{source}"
        )

    for source, model in (
        ("linkedin", LinkedInRequest),
        ("job_boards", JobBoardsRequest),
        ("crunchbase", CrunchbaseRequest),
        ("review_directories", ReviewDirectoriesRequest),
        ("web_directories", WebDirectoriesRequest),
        ("google_search", GoogleSearchRequest),
    ):
        search_route(source, model)

    app.include_router(v1)
    return app


def openapi_schema(app: FastAPI) -> dict:
    """The checked-in contract (apps/backend-go/internal/browserclient/testdata/openapi.json).
    FastAPI's 422 entries are dropped: validation errors are 400 + ErrorEnvelope."""
    schema = get_openapi(title=app.title, version=app.version, routes=app.routes)
    for path in schema.get("paths", {}).values():
        for op in path.values():
            op.get("responses", {}).pop("422", None)
    comps = schema.get("components", {}).get("schemas", {})
    comps.pop("HTTPValidationError", None)
    comps.pop("ValidationError", None)
    return json.loads(json.dumps(schema, sort_keys=True))
