"""MCP token auth + per-call workspace resolution.

This is the single trust boundary for the MCP bridge. It mirrors how the REST
``current_workspace`` dependency and the chat ``_resolve_chat_workspace`` fix
(#90) resolve and authorize a tenant, but for a non-FastAPI caller:

  * Cloud / multi-tenant (``settings.MCP_REQUIRE_AUTH`` true): a presented
    plaintext MCP token is hashed, looked up in ``mcp_tokens``, checked for
    revocation/expiry, then the user's CURRENT workspace membership is
    re-verified live (``ws_manager.is_member``) — capability grant alone is
    never sufficient. Missing/invalid/expired/revoked/non-member → raise.
  * Self-host (SQLite / ``MCP_REQUIRE_AUTH`` false): keyless. Binds to the
    ``main`` workspace with the read capabilities granted, ``user_id`` None.

Capabilities are explicit, additive, default-empty. Phase 1 grants only the
read capability; the write capabilities are defined here so the token/grant
plumbing is complete, but no write tool is wired until Phase 2.
"""

from __future__ import annotations

import hashlib
import secrets
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import FrozenSet, List, Optional, Tuple

from apps.api.core.config import settings
from apps.api.services.workspace import manager as ws_manager


# ── Capability vocabulary ────────────────────────────────────────────────────
CAP_LEADS_READ = "leads:read"          # Phase 1
CAP_LEADS_WRITE = "leads:write"        # Phase 2
CAP_SEQUENCES_ENROLL = "sequences:enroll"  # Phase 2
CAP_WORKBOOKS_WRITE = "workbooks:write"    # Phase 2
CAP_AUTOMATIONS_WRITE = "automations:write"  # v2 — create_automation
CAP_OUTREACH_SEND = "outreach:send"          # v2 — send_email / sequencer send (HIGHEST RISK)
CAP_LEADS_DELETE = "leads:delete"            # v2 — delete_lead (DESTRUCTIVE, admin-only)
CAP_WORKBOOKS_DELETE = "workbooks:delete"    # v2 — delete_workbook (DESTRUCTIVE, admin-only)

READ_CAPABILITIES: FrozenSet[str] = frozenset({CAP_LEADS_READ})
WRITE_CAPABILITIES: FrozenSet[str] = frozenset({
    CAP_LEADS_WRITE, CAP_SEQUENCES_ENROLL, CAP_WORKBOOKS_WRITE,
    # v2 ops (each its own capability; all write-gated by MCP_WRITE_ENABLED):
    CAP_AUTOMATIONS_WRITE, CAP_OUTREACH_SEND, CAP_LEADS_DELETE, CAP_WORKBOOKS_DELETE,
})
ALL_CAPABILITIES: FrozenSet[str] = READ_CAPABILITIES | WRITE_CAPABILITIES

# v2 ops that are DESTRUCTIVE or HIGHEST-RISK demand the workspace ADMIN role
# (owner implicitly passes), not merely a write-capable member. The tool→roles
# map lives in ``tools.py`` so every tool's role floor is explicit.
ADMIN_ONLY_CAPABILITIES: FrozenSet[str] = frozenset({
    CAP_AUTOMATIONS_WRITE, CAP_OUTREACH_SEND, CAP_LEADS_DELETE, CAP_WORKBOOKS_DELETE,
})

# Workspace roles allowed to perform a standard MCP *write* (owner always passes
# via require_role). A user downgraded to "viewer" after the token was minted is
# denied at call time — the live role re-check that defeats the privilege-freeze
# confused-deputy variant (a capability grant is necessary but not sufficient).
WRITE_ROLES: Tuple[str, ...] = ("admin", "member", "editor")
# Stricter floor for destructive / send tools — admin (or owner) only.
ADMIN_ROLES: Tuple[str, ...] = ("admin",)

# Plaintext token prefix so tokens are recognisable in logs/configs.
TOKEN_PREFIX = "ycp_"  # Legacy-compatible OpenGTM capability token prefix.


class MCPAuthError(Exception):
    """Raised when an MCP credential is missing, invalid, or no longer authorized."""


@dataclass(frozen=True)
class MCPCtx:
    """Resolved, authorized tenant context for one MCP session/call.

    The MCP analog of ``WorkspaceCtx``: every tool runs scoped to this and
    nothing else. ``capabilities`` is the effective grant set for the session.
    """

    workspace_id: str
    slug: str
    capabilities: FrozenSet[str]
    user_id: Optional[int] = None
    token_id: Optional[str] = None

    def has(self, capability: str) -> bool:
        return capability in self.capabilities


# ── Token hashing / minting ──────────────────────────────────────────────────

def hash_token(raw: str) -> str:
    """sha256 hex of the plaintext token (what we persist + look up by)."""
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()


def generate_token() -> Tuple[str, str, str]:
    """Mint a fresh token. Returns ``(plaintext, token_hash, prefix)``.

    Plaintext is shown to the caller exactly once and never stored.
    """
    raw = TOKEN_PREFIX + secrets.token_urlsafe(32)
    return raw, hash_token(raw), raw[:12]


def _utcnow() -> datetime:
    return datetime.now(timezone.utc)


def create_token(
    *,
    user_id: int,
    workspace_id: str,
    capabilities: List[str],
    name: str = "",
    created_by: Optional[int] = None,
    ttl_days: Optional[int] = None,
) -> Tuple[str, "object"]:
    """Persist a new MCP token row and return ``(plaintext, MCPToken)``.

    Validates the requested capabilities against the known vocabulary and binds
    the token to exactly one workspace. The caller (REST router) is responsible
    for authorizing that the actor may mint a token for ``workspace_id``.
    """
    from apps.api.database import SessionLocal
    from apps.api.services.mcp.models import MCPToken

    bad = set(capabilities) - ALL_CAPABILITIES
    if bad:
        raise MCPAuthError(f"unknown capabilities: {sorted(bad)}")

    raw, token_hash, prefix = generate_token()
    if ttl_days is None:
        ttl_days = settings.MCP_TOKEN_TTL_DAYS
    expires_at = _utcnow() + timedelta(days=ttl_days) if ttl_days and ttl_days > 0 else None

    tok = MCPToken(
        token_hash=token_hash,
        prefix=prefix,
        name=name or "",
        user_id=user_id,
        workspace_id=workspace_id,
        capabilities=list(dict.fromkeys(capabilities)),  # dedupe, keep order
        expires_at=expires_at,
        created_by=created_by if created_by is not None else user_id,
    )
    with SessionLocal() as s, s.begin():
        s.add(tok)
        s.flush()
        s.refresh(tok)
        s.expunge(tok)
    return raw, tok


# ── Resolution ───────────────────────────────────────────────────────────────

def _self_host_ctx() -> MCPCtx:
    """Keyless self-host context bound to the ``main`` workspace (read-only)."""
    ws_id = ws_manager._get_active_workspace_id()
    slug = ws_manager.workspace_slug(ws_id) or "main"
    return MCPCtx(
        workspace_id=ws_id,
        slug=slug,
        capabilities=READ_CAPABILITIES,
        user_id=None,
        token_id=None,
    )


def resolve_mcp_token(raw: Optional[str]) -> MCPCtx:
    """Authenticate + authorize an MCP credential into an :class:`MCPCtx`.

    Fail-closed in cloud: any failure raises :class:`MCPAuthError`. Self-host
    (``MCP_REQUIRE_AUTH`` false) ignores the token and returns the keyless
    ``main`` context.
    """
    if not settings.MCP_REQUIRE_AUTH:
        return _self_host_ctx()

    if not raw:
        raise MCPAuthError("missing MCP token")

    from apps.api.database import SessionLocal
    from apps.api.services.mcp.models import MCPToken

    token_hash = hash_token(raw)
    now = _utcnow()

    with SessionLocal() as s, s.begin():
        tok = (
            s.query(MCPToken)
            .filter(MCPToken.token_hash == token_hash)
            .first()
        )
        if tok is None:
            raise MCPAuthError("invalid MCP token")
        if tok.revoked_at is not None:
            raise MCPAuthError("MCP token revoked")
        if tok.expires_at is not None and _aware(tok.expires_at) <= now:
            raise MCPAuthError("MCP token expired")

        user_id = tok.user_id
        workspace_id = tok.workspace_id
        token_id = tok.id
        granted = frozenset(tok.capabilities or [])

        # Touch last_used_at inside the same txn (cheap; useful for forensics).
        tok.last_used_at = now

    # Live membership re-check: a token grant is necessary, not sufficient. If
    # the user was removed from the workspace after the token was minted, deny.
    if not ws_manager.is_member(workspace_id, user_id):
        raise MCPAuthError("MCP token user is no longer a workspace member")

    slug = ws_manager.workspace_slug(workspace_id)
    if not slug:
        raise MCPAuthError("MCP token workspace not found")

    return MCPCtx(
        workspace_id=workspace_id,
        slug=slug,
        capabilities=granted,
        user_id=user_id,
        token_id=token_id,
    )


def _aware(dt: datetime) -> datetime:
    """Treat naive timestamps (SQLite) as UTC so comparisons never crash."""
    if dt.tzinfo is None:
        return dt.replace(tzinfo=timezone.utc)
    return dt


def require_role(ctx: MCPCtx, *roles: str) -> None:
    """Live workspace-role re-check (Phase-2 write tools call this).

    Mirrors ``tenancy.require_workspace_role``: owner implicitly satisfies any
    role. Self-host (no user) is treated as full owner. Raises on failure.
    """
    if ctx.user_id is None:
        return  # self-host single workspace
    role = ws_manager.member_role(ctx.workspace_id, ctx.user_id)
    if role == "owner" or role in roles:
        return
    raise MCPAuthError("insufficient workspace role")
