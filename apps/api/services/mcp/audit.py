"""Append-only MCP audit log writer.

Writes to the RLS-scoped ``mcp_audit_log`` table, so the insert MUST run inside
``workspace_scope(ctx.workspace_id)`` (the ``after_begin`` hook sets the GUC and
the WITH CHECK policy stamps the row to the caller's tenant).

Phase 1 ships READ tools only, which are not audited (to limit volume — owner
decision #3). This module is the foundation the Phase-2 write tools call to
record exactly one row per call, including denials. ``record`` is best-effort:
auditing must never crash a tool call, but a failure is logged loudly.
"""

from __future__ import annotations

import logging
from typing import Any, Dict, Optional

logger = logging.getLogger("mcp.audit")

# Argument keys whose values are secrets/PII and must never be persisted raw.
_REDACT_KEYS = {"token", "password", "secret", "api_key", "authorization", "email"}


def redact_arguments(args: Optional[Dict[str, Any]]) -> Dict[str, Any]:
    """Return a shallow PII/secret-trimmed copy of tool arguments."""
    if not args:
        return {}
    out: Dict[str, Any] = {}
    for k, v in args.items():
        if k.lower() in _REDACT_KEYS:
            out[k] = "***redacted***"
        elif isinstance(v, str) and len(v) > 500:
            out[k] = v[:500] + "…"
        else:
            out[k] = v
    return out


def record(
    ctx,
    tool_name: str,
    arguments: Optional[Dict[str, Any]] = None,
    *,
    result_status: str = "ok",
    error: Optional[str] = None,
    estimated_cost_usd: float = 0.0,
    idempotency_key: Optional[str] = None,
) -> None:
    """Best-effort: write one ``mcp_audit_log`` row scoped to ``ctx.workspace_id``."""
    from apps.api.core.tenancy import workspace_scope
    from apps.api.database import SessionLocal
    from apps.api.services.mcp.models import MCPAuditLog

    try:
        with workspace_scope(ctx.workspace_id):
            with SessionLocal() as s, s.begin():
                s.add(MCPAuditLog(
                    workspace_id=ctx.workspace_id,
                    token_id=getattr(ctx, "token_id", None),
                    user_id=getattr(ctx, "user_id", None),
                    tool_name=tool_name,
                    arguments_redacted=redact_arguments(arguments),
                    idempotency_key=idempotency_key,
                    result_status=result_status,
                    error=error,
                    estimated_cost_usd=estimated_cost_usd,
                ))
    except Exception as e:  # pragma: no cover - defensive
        logger.error("mcp audit write failed (tool=%s status=%s): %s",
                     tool_name, result_status, e)
