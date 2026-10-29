"""POST /v1/holehe: which sites an email is registered on (legacy
leadgen/enrichment/providers/holehe_verify.py).

Same module set and order as legacy (pkgutil walk of holehe.modules, first 50
by default) and the same concurrency (10) and per-request timeout (10 s). Every
module's traffic goes through the egress proxy. The confidence / verdict
mapping (is_real, email_verify) stays in Go; this returns raw per-site results.

Deviation (legacy bug, fixed): legacy passed a dict as holehe's ``out`` list,
so every module raised on ``out.append`` and no site was ever reported.
"""

from __future__ import annotations

import asyncio
import importlib
import logging
import pkgutil
from functools import cache
from typing import Any

from ..engines import Deadline, Deps
from ..models import HoleheRequest, HoleheResult, HoleheSite

logger = logging.getLogger("browser_service.scrapers.holehe")

CONCURRENCY = 10
CLIENT_TIMEOUT_S = 10


@cache
def _modules() -> tuple:
    import holehe.modules

    out = []
    for _imp, modname, ispkg in pkgutil.walk_packages(holehe.modules.__path__, holehe.modules.__name__ + "."):
        if ispkg:
            continue
        try:
            mod = importlib.import_module(modname)
        except Exception:  # noqa: S112 - a broken module is skipped, as in legacy
            continue
        fn = getattr(mod, modname.rsplit(".", 1)[-1], None)
        if fn is not None:
            out.append(fn)
    return tuple(out)


def load_modules() -> list:
    return list(_modules())


def make_client(proxy_url: str, timeout: float) -> Any:
    import httpx

    return httpx.AsyncClient(proxy=proxy_url, timeout=timeout, trust_env=False, follow_redirects=False)


async def check(deps: Deps, req: HoleheRequest, deadline: Deadline) -> HoleheResult:
    modules = load_modules()[: req.max_modules]
    session = deps.proxy.open_session(deps.settings.max_download_bytes, upstream=req.upstream())
    client = make_client(deps.proxy.proxy_url(session), CLIENT_TIMEOUT_S)
    sem = asyncio.Semaphore(CONCURRENCY)
    errors = 0

    async def one(fn) -> dict | None:
        nonlocal errors
        async with sem:
            out: list = []
            try:
                await fn(req.email, client, out)
            except Exception:
                errors += 1
                return None
            return out[0] if out and isinstance(out[0], dict) else None

    tasks = [asyncio.ensure_future(one(fn)) for fn in modules]
    partial = False
    try:
        done, pending = await asyncio.wait(tasks, timeout=max(0.5, deadline.left() - 1.0)) if tasks else (set(), set())
        if pending:
            partial = True
            for t in pending:
                t.cancel()
            await asyncio.gather(*pending, return_exceptions=True)
    finally:
        try:
            await client.aclose()
        finally:
            deps.proxy.close_session(session)

    sites: list[HoleheSite] = []
    for t in tasks:  # module order, as legacy gathered them
        if t.cancelled() or t.exception() is not None:
            continue
        r = t.result()
        if r is None:
            continue
        sites.append(
            HoleheSite(
                name=str(r.get("name", "unknown"))[:100],
                domain=str(r.get("domain") or "")[:253],
                method=str(r.get("method") or "")[:50],
                exists=r.get("exists") is True,
                rate_limited=bool(r.get("rateLimit")),
            )
        )
    return HoleheResult(
        email=req.email,
        sites=sites,
        exists_on=[s.name for s in sites if s.exists],
        total_checked=len(sites),
        errors=errors,
        partial=partial,
        elapsed_ms=deadline.elapsed_ms(),
    )
