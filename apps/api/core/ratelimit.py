"""
Application rate limiter (slowapi).

A single shared ``Limiter`` so the app, the exception handler, and every router
all reference the same instance. Expensive routes decorate their handlers with
``@limiter.limit(...)`` (the handler must take a ``request: Request`` arg — that
is how slowapi finds the limiter on ``app.state`` and runs the key function).

Keying: most expensive endpoints are workspace-scoped, so we key by workspace
identity when we can derive it (the ``X-Workspace-Id`` header the frontend sends,
matching ``core.tenancy.current_workspace``), with the authenticated user as a
secondary signal and the client IP as the final fallback. This means one tenant
hammering the API can't exhaust another tenant's budget, and an unauthenticated
caller is still bounded by IP.
"""

from fastapi import Request
from slowapi import Limiter
from slowapi.util import get_remote_address


def workspace_key(request: Request) -> str:
    """Rate-limit key: prefer workspace, then user, then client IP.

    Reads the ``X-Workspace-Id`` header (same source as the tenancy dependency).
    Falls back to a bearer-token fingerprint, then the remote address, so the
    key is always populated even for unauthenticated/headerless requests.
    """
    ws = request.headers.get("x-workspace-id")
    if ws:
        return f"ws:{ws}"

    auth = request.headers.get("authorization", "")
    if auth.lower().startswith("bearer "):
        token = auth[7:].strip()
        if token:
            # The raw token is the per-user secret; bucket by it so a single
            # user without a workspace header is still isolated. Truncated to
            # keep keys bounded; collisions only loosen limits, never tighten.
            return f"tok:{token[:32]}"

    return f"ip:{get_remote_address(request)}"


# Shared limiter. Default key is workspace-aware; per-route decorators may
# override the key_func if needed.
limiter = Limiter(key_func=workspace_key)
