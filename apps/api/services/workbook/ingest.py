"""Inbound rows ("webhook source") — per-workbook ingest tokens + idempotency.

Lets external systems (and the future Chrome extension) push rows INTO a
workbook via ``POST /api/v2/workbooks/{id}/rows/ingest`` (see routers/ingest.py).
This module owns the two tables and the token mint/resolve helpers:

  * ``workbook_ingest_tokens`` — AUTH-PLANE (NOT RLS), like ``mcp_tokens``:
    looked up by sha256 ``token_hash`` BEFORE any workspace GUC is bound, so it
    must not carry an RLS policy. Every row is still bound to exactly one
    (workspace_id, workbook_id) — there is no cross-workbook token. Only the
    hash is stored; the plaintext (``wbi_…``) is shown once at mint time.
  * ``workbook_ingest_idempotency`` — RLS-scoped ledger of recent
    ``Idempotency-Key`` results per workbook (fail-closed policy on
    ``current_setting('app.workspace_id', true)``, mirroring the workbooks RLS
    posture from migration e5f6a7b8c9d0). A replayed key returns the stored
    response instead of inserting rows twice.

Token verification is fail-closed and constant-time: the presented plaintext is
hashed, the row is fetched by that hash, and the stored hash is re-compared via
``hmac.compare_digest`` (mirrors services/mcp/auth.py).
"""

from __future__ import annotations

import hashlib
import hmac
import secrets
import uuid
from typing import Tuple

from sqlalchemy import (
    Column, DateTime, Index, Integer, JSON, String, UniqueConstraint,
)
from sqlalchemy.sql import func

from apps.api.database import Base

# Plaintext token prefix so tokens are recognisable in logs/configs
# (mirrors the "ycp_" MCP PAT prefix). wbi = workbook ingest.
TOKEN_PREFIX = "wbi_"


def _uuid() -> str:
    return str(uuid.uuid4())


class IngestAuthError(Exception):
    """Raised when an ingest token is missing, invalid, revoked, or mis-scoped."""


# ── ORM models ───────────────────────────────────────────────────────────────

class WorkbookIngestToken(Base):
    """A hashed, single-workbook-bound ingest token. Auth-plane (NOT RLS)."""

    __tablename__ = "workbook_ingest_tokens"
    __table_args__ = (
        # Uniqueness enforced by the unique index (not a column constraint) so
        # the ORM metadata matches the migration's CREATE UNIQUE INDEX exactly.
        Index("ix_workbook_ingest_tokens_token_hash", "token_hash", unique=True),
        Index("ix_workbook_ingest_tokens_workspace_id", "workspace_id"),
        Index("ix_workbook_ingest_tokens_workbook_id", "workbook_id"),
    )

    id = Column(String(36), primary_key=True, default=_uuid)
    # Denormalized tenant — resolved from the token at auth time (the caller has
    # no session), then used to enter workspace_scope for the RLS'd row writes.
    workspace_id = Column(String(64), nullable=False)
    # The single workbook this token can ever ingest into.
    workbook_id = Column(String, nullable=False)
    # sha256 hex of the plaintext token. Never store the plaintext.
    token_hash = Column(String(64), nullable=False)
    # First chars of the plaintext (e.g. "wbi_ab12cd34") for display only.
    prefix = Column(String(20), nullable=False)
    created_at = Column(DateTime(timezone=True), nullable=False, server_default=func.now())
    revoked_at = Column(DateTime(timezone=True), nullable=True)
    created_by = Column(Integer, nullable=True)


class WorkbookIngestIdempotency(Base):
    """Recent Idempotency-Key → stored response, per workbook. RLS-scoped."""

    __tablename__ = "workbook_ingest_idempotency"
    __table_args__ = (
        # One logical ingest per (workbook, key) — the replay/race guard.
        UniqueConstraint("workbook_id", "idempotency_key", name="uq_wb_ingest_idem"),
        Index("ix_workbook_ingest_idem_workspace_id", "workspace_id"),
        Index("ix_workbook_ingest_idem_workbook_id", "workbook_id"),
    )

    id = Column(String(36), primary_key=True, default=_uuid)
    # Denormalized tenant (same fail-closed RLS policy as the workbook tables).
    workspace_id = Column(String(64), nullable=False)
    workbook_id = Column(String, nullable=False)
    idempotency_key = Column(String(128), nullable=False)
    # The exact JSON body returned to the first caller; replays return it as-is.
    response = Column(JSON, nullable=False, default=dict)
    created_at = Column(DateTime(timezone=True), nullable=False, server_default=func.now())


# ── Token hashing / minting / resolution ─────────────────────────────────────

def hash_token(raw: str) -> str:
    """sha256 hex of the plaintext token (what we persist + look up by)."""
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()


def generate_token() -> Tuple[str, str, str]:
    """Mint a fresh token. Returns ``(plaintext, token_hash, prefix)``.

    Plaintext is shown to the caller exactly once and never stored.
    """
    raw = TOKEN_PREFIX + secrets.token_urlsafe(32)
    return raw, hash_token(raw), raw[:12]


def resolve_ingest_token(raw: str, workbook_id: str) -> str:
    """Authenticate an ingest token for ``workbook_id`` → its ``workspace_id``.

    Fail-closed: any failure (unknown, revoked, bound to a different workbook)
    raises :class:`IngestAuthError`. The stored hash is compared constant-time
    via ``hmac.compare_digest`` on top of the indexed hash lookup. Uses its own
    short session (auth-plane table; no workspace GUC is bound yet).
    """
    from apps.api.database import SessionLocal

    presented = hash_token(raw or "")
    with SessionLocal() as s:
        tok = (
            s.query(WorkbookIngestToken)
            .filter(WorkbookIngestToken.token_hash == presented)
            .first()
        )
        if tok is None or not hmac.compare_digest(tok.token_hash, presented):
            raise IngestAuthError("invalid ingest token")
        if tok.revoked_at is not None:
            raise IngestAuthError("ingest token revoked")
        # A token is bound to exactly one workbook — never accept it elsewhere.
        if tok.workbook_id != workbook_id:
            raise IngestAuthError("ingest token not valid for this workbook")
        return tok.workspace_id
