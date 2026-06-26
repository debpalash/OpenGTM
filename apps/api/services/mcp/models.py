"""MCP ORM models — scoped tokens (auth plane) + append-only audit log (RLS).

Two tables on the single Base from apps.api.database:

  * ``mcp_tokens``    — PAT-style credential for the MCP bridge. This is an
    AUTH-PLANE table (like ``users``): **NOT RLS-scoped**, looked up by token
    hash with no workspace GUC bound. Every row is nonetheless bound to exactly
    one ``workspace_id`` and one ``user_id`` — there is no cross-workspace token.
    Only the sha256 hash is stored; the plaintext is shown once at mint time.
  * ``mcp_audit_log`` — append-only forensic trail. **RLS-scoped by
    ``workspace_id``** (fail-closed policy created in the migration) so audit
    reads only ever see the caller's tenant. One row per write tool call
    (incl. denials) once write tools land in Phase 2; in Phase 1 it is the
    foundation table (read tools are not audited to limit volume).

The ``UniqueConstraint(workspace_id, idempotency_key)`` idiom mirrors
``automations/models.py`` so a retried (Phase-2) write is a no-op.
"""

import uuid

from sqlalchemy import (
    Column, String, Integer, Text, DateTime, JSON, Float,
    UniqueConstraint, Index,
)
from sqlalchemy.sql import func

from apps.api.database import Base


def _uuid() -> str:
    return str(uuid.uuid4())


class MCPToken(Base):
    """A hashed, single-workspace-bound, capability-scoped MCP token.

    Auth-plane (NOT RLS). Looked up by ``token_hash``; ``prefix`` is the only
    plaintext fragment retained, purely for human-readable listing/revocation.
    """

    __tablename__ = "mcp_tokens"
    __table_args__ = (
        Index("ix_mcp_tokens_token_hash", "token_hash", unique=True),
        Index("ix_mcp_tokens_workspace_id", "workspace_id"),
        Index("ix_mcp_tokens_user_id", "user_id"),
    )

    id = Column(String(36), primary_key=True, default=_uuid)
    # sha256 hex of the plaintext token. Never store the plaintext. Uniqueness is
    # enforced by the unique index in __table_args__ (not a column constraint, so
    # the ORM metadata matches the migration's CREATE UNIQUE INDEX exactly).
    token_hash = Column(String(64), nullable=False)
    # First chars of the plaintext (e.g. "ycp_ab12cd34") for display only.
    prefix = Column(String(20), nullable=False)
    name = Column(String(200), nullable=False, default="")
    user_id = Column(Integer, nullable=False)
    # The single workspace this token can ever act in. No cross-workspace token.
    workspace_id = Column(String(64), nullable=False)
    # JSON array of grant strings, e.g. ["leads:read"]. Empty = no access.
    capabilities = Column(JSON, nullable=False, default=list)
    expires_at = Column(DateTime(timezone=True), nullable=True)
    revoked_at = Column(DateTime(timezone=True), nullable=True)
    last_used_at = Column(DateTime(timezone=True), nullable=True)
    created_at = Column(DateTime(timezone=True), nullable=False, server_default=func.now())
    created_by = Column(Integer, nullable=True)


class MCPAuditLog(Base):
    """Append-only audit row for an MCP tool call. RLS-scoped by workspace_id."""

    __tablename__ = "mcp_audit_log"
    __table_args__ = (
        # Idempotency ledger idiom (Phase-2 writes): one logical write per key.
        UniqueConstraint(
            "workspace_id", "idempotency_key",
            name="uq_mcp_audit_ws_idem",
        ),
        Index("ix_mcp_audit_log_workspace_id", "workspace_id"),
        Index("ix_mcp_audit_log_ws_created", "workspace_id", "created_at"),
    )

    id = Column(String(36), primary_key=True, default=_uuid)
    workspace_id = Column(String(64), nullable=False)
    token_id = Column(String(36), nullable=True)
    user_id = Column(Integer, nullable=True)
    tool_name = Column(String(100), nullable=False)
    # PII/secret-trimmed copy of the call arguments.
    arguments_redacted = Column(JSON, nullable=False, default=dict)
    idempotency_key = Column(String(128), nullable=True)
    # ok | error | denied | capped
    result_status = Column(String(20), nullable=False, default="ok")
    error = Column(Text, nullable=True)
    estimated_cost_usd = Column(Float, nullable=False, default=0.0)
    created_at = Column(DateTime(timezone=True), nullable=False, server_default=func.now())
