"""MCP token management API — mint / list / revoke scoped MCP tokens.

prefix ``/api/mcp/tokens``. Every endpoint is gated by
``require_workspace_role("admin")`` so only a workspace admin/owner can mint a
token that acts in that workspace. The plaintext token is returned EXACTLY ONCE
at creation; only its sha256 hash is persisted.

Phase 1 only the read capability (``leads:read``) is meaningfully usable; write
capabilities can be granted but the matching tools stay hidden/inert until
``MCP_WRITE_ENABLED`` is on (Phase 2).
"""

import logging
from typing import List, Optional

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field

from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role
from apps.api.database import get_db
from apps.api.services.mcp import auth as mcp_auth
from apps.api.services.mcp.models import MCPToken

logger = logging.getLogger("mcp.tokens.api")
router = APIRouter(prefix="/api/mcp/tokens", tags=["mcp"])

require_admin = require_workspace_role("admin")


class TokenCreate(BaseModel):
    name: str = Field("", max_length=200)
    # Default to read-only — the only Phase-1 usable capability.
    capabilities: List[str] = Field(default_factory=lambda: [mcp_auth.CAP_LEADS_READ])
    ttl_days: Optional[int] = None


class TokenOut(BaseModel):
    id: str
    prefix: str
    name: str
    capabilities: List[str]
    workspace_id: str
    created_at: Optional[str] = None
    expires_at: Optional[str] = None
    last_used_at: Optional[str] = None
    revoked_at: Optional[str] = None


class TokenCreated(TokenOut):
    token: str  # plaintext, shown ONCE


def _to_out(t: MCPToken) -> dict:
    return {
        "id": t.id,
        "prefix": t.prefix,
        "name": t.name or "",
        "capabilities": list(t.capabilities or []),
        "workspace_id": t.workspace_id,
        "created_at": t.created_at.isoformat() if t.created_at else None,
        "expires_at": t.expires_at.isoformat() if t.expires_at else None,
        "last_used_at": t.last_used_at.isoformat() if t.last_used_at else None,
        "revoked_at": t.revoked_at.isoformat() if t.revoked_at else None,
    }


@router.post("", response_model=TokenCreated)
def create_mcp_token(body: TokenCreate, ctx: WorkspaceCtx = Depends(require_admin)):
    """Mint a token bound to the caller's current workspace. Plaintext shown once."""
    caps = list(dict.fromkeys(body.capabilities or []))
    bad = set(caps) - set(mcp_auth.ALL_CAPABILITIES)
    if bad:
        raise HTTPException(status_code=400, detail=f"unknown capabilities: {sorted(bad)}")
    try:
        raw, tok = mcp_auth.create_token(
            user_id=ctx.user.id,
            workspace_id=ctx.workspace_id,
            capabilities=caps,
            name=body.name,
            created_by=ctx.user.id,
            ttl_days=body.ttl_days,
        )
    except mcp_auth.MCPAuthError as e:
        raise HTTPException(status_code=400, detail=str(e))
    out = _to_out(tok)
    out["token"] = raw
    return out


@router.get("", response_model=List[TokenOut])
def list_mcp_tokens(ctx: WorkspaceCtx = Depends(require_admin), db=Depends(get_db)):
    """List this workspace's tokens (hashes only; never the plaintext)."""
    rows = (
        db.query(MCPToken)
        .filter(MCPToken.workspace_id == ctx.workspace_id)
        .order_by(MCPToken.created_at.desc())
        .all()
    )
    return [_to_out(t) for t in rows]


@router.delete("/{token_id}")
def revoke_mcp_token(token_id: str, ctx: WorkspaceCtx = Depends(require_admin), db=Depends(get_db)):
    """Revoke a token. Scoped to the caller's workspace (cross-tenant id → 404)."""
    from datetime import datetime, timezone

    tok = (
        db.query(MCPToken)
        .filter(MCPToken.id == token_id, MCPToken.workspace_id == ctx.workspace_id)
        .first()
    )
    if tok is None:
        raise HTTPException(status_code=404, detail="token not found")
    if tok.revoked_at is None:
        tok.revoked_at = datetime.now(timezone.utc)
        db.commit()
    return {"ok": True, "id": token_id}
