"""Inbound rows API ("webhook source") — push rows INTO a workbook.

prefix ``/api/v2/workbooks``. Flag-gated by ``INGEST_API_ENABLED`` (every path
404s when off, mirroring how ``INTENT_POLLER_ENABLED`` gates routers/watches.py).

Two auth paths for the ingest endpoint:
  * session/JWT — the normal ``current_workspace`` resolution (workspace taken
    from ``X-Workspace-Id`` or the user's active workspace, membership enforced);
  * per-workbook ingest token — machine callers present ``Authorization:
    Bearer wbi_…`` (or ``X-Ingest-Token: wbi_…``); the token resolves the
    (workspace, workbook) pair itself (constant-time hash compare, fail-closed)
    and the handler then enters ``workspace_scope`` so the RLS'd row writes are
    tenant-correct.

Rows are materialized the same way the manual add-rows endpoint and the source
engine do (WorkbookRow with denormalized ``workspace_id``, position-appended,
``total_rows`` refreshed, ``on_row_added`` emitted for automations). Incoming
keys are mapped case-insensitively to the workbook's ``lead_field`` column
names; unknown keys are KEPT verbatim as extra fields in row data (consistent
with POST /rows, which stores arbitrary dicts that ``to_api_row`` flattens) and
reported back in ``unmapped_keys``.

Dedup: ingest reuses the add-rows identity behavior (normalized website domain,
else normalized company name) against rows already in the workbook AND within
the batch — it does NOT run the source engine's canonical-entity resolution, so
rows land with ``canonical_entity_id = None`` (appended snapshots, like manual
adds). Send ``"dedupe": false`` to append blindly.

Idempotency: an optional ``Idempotency-Key`` header makes the ingest replay-safe
— the first result is stored per (workbook, key) and returned verbatim on
replay (marked with an ``Idempotent-Replay: true`` response header).
"""

from __future__ import annotations

import logging
from datetime import datetime, timedelta, timezone
from typing import Optional

from fastapi import APIRouter, Depends, HTTPException, Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field
from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import Session
from sqlalchemy import func as sa_func

from apps.api.core.config import settings
from apps.api.core.tenancy import (
    WorkspaceCtx, current_workspace, require_workspace_role, workspace_scope,
)
from apps.api.database import get_db
from apps.api.services.workbook.models import LEAD_FIELD_MAP, Workbook, WorkbookRow
from apps.api.services.workbook import ingest as ingest_svc
from apps.api.services.workbook.ingest import (
    IngestAuthError, WorkbookIngestIdempotency, WorkbookIngestToken,
)

logger = logging.getLogger("workbook.ingest.api")
router = APIRouter(prefix="/api/v2/workbooks", tags=["ingest"])

require_editor = require_workspace_role("editor", "admin")

# Hard cap per request — beyond this the request is rejected with 413.
MAX_INGEST_ROWS = 500
# Idempotency keys older than this are pruned opportunistically on write.
IDEMPOTENCY_RETENTION_DAYS = 7


def _require_enabled():
    if not getattr(settings, "INGEST_API_ENABLED", False):
        raise HTTPException(status_code=404, detail="ingest api disabled")


# ── request models ────────────────────────────────────────────────────────────

class IngestRowsRequest(BaseModel):
    rows: list[dict] = Field(
        ..., min_length=1,
        description="Row dicts keyed by workbook column name or lead field",
    )
    dedupe: bool = Field(
        True,
        description="Skip rows whose identity (domain/company) already exists or repeats",
    )


# ── auth helpers ──────────────────────────────────────────────────────────────

async def optional_session_workspace(
    request: Request, db: Session = Depends(get_db)
) -> Optional[WorkspaceCtx]:
    """Resolve the caller's session/JWT workspace, or None for machine callers.

    Returns None when the request carries no bearer credential or the bearer is
    an ingest token (``wbi_…`` — handled by the token path). A present-but-bad
    JWT still raises 401, exactly like the normal dependency chain would.
    """
    auth = request.headers.get("authorization") or ""
    if not auth.lower().startswith("bearer "):
        return None
    raw = auth.split(" ", 1)[1].strip()
    if not raw or raw.startswith(ingest_svc.TOKEN_PREFIX):
        return None
    from apps.api.core.security import get_current_user

    user = await get_current_user(token=raw, db=db)  # 401 on invalid JWT
    if not getattr(user, "is_active", True):
        raise HTTPException(status_code=400, detail="Inactive user")
    return current_workspace(
        user=user, x_workspace_id=request.headers.get("x-workspace-id")
    )


def _extract_ingest_token(request: Request) -> Optional[str]:
    """Pull an ingest token from X-Ingest-Token or a wbi_-prefixed bearer."""
    raw = (request.headers.get("x-ingest-token") or "").strip()
    if raw:
        return raw
    auth = request.headers.get("authorization") or ""
    if auth.lower().startswith("bearer "):
        candidate = auth.split(" ", 1)[1].strip()
        if candidate.startswith(ingest_svc.TOKEN_PREFIX):
            return candidate
    return None


# ── column mapping / dedup helpers ────────────────────────────────────────────

def _key_map(wb: Workbook) -> dict:
    """lowercase incoming key → canonical lead_field name.

    Every known lead field maps to itself; each of the workbook's lead_field
    columns additionally maps its display name (e.g. "Company") to its field.
    """
    m = {f.lower(): f for f in LEAD_FIELD_MAP}
    for c in (wb.columns_config or []):
        if c.get("type") == "lead_field" and c.get("lead_field"):
            lf = str(c["lead_field"])
            m.setdefault(lf.lower(), lf)
            name = str(c.get("name") or "").strip()
            if name:
                m[name.lower()] = lf
    return m


def _map_row(raw: dict, key_map: dict, unmapped: set) -> dict:
    """Map one incoming row's keys; unknown keys are kept verbatim."""
    out = {}
    for k, v in raw.items():
        canon = key_map.get(str(k).strip().lower())
        if canon:
            out[canon] = v
        else:
            out[str(k)] = v
            unmapped.add(str(k))
    return out


def _identity(d: dict) -> str:
    """Same row identity the manual add-rows endpoint dedupes on."""
    from apps.api.services.leadgen.dedup import normalize_company, normalize_domain

    dom = normalize_domain(d.get("website") or d.get("domain") or "")
    if dom:
        return f"d:{dom}"
    comp = normalize_company(d.get("company") or "")
    return f"n:{comp}" if comp else ""


# ── endpoints ────────────────────────────────────────────────────────────────

@router.post("/{workbook_id}/ingest-token")
def create_ingest_token(
    workbook_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Create/rotate the workbook's ingest token. Plaintext returned ONCE.

    Rotation semantics: any previously active token for this workbook is
    revoked in the same transaction, so exactly one token is live at a time.
    """
    _require_enabled()
    wb = (
        db.query(Workbook)
        .filter(Workbook.id == workbook_id, Workbook.workspace_id == ctx.workspace_id)
        .first()
    )
    if wb is None:
        # 404 (not 403) so cross-tenant ids don't leak existence.
        raise HTTPException(status_code=404, detail="Workbook not found")

    now = datetime.now(timezone.utc)
    rotated = (
        db.query(WorkbookIngestToken)
        .filter(
            WorkbookIngestToken.workbook_id == workbook_id,
            WorkbookIngestToken.workspace_id == ctx.workspace_id,
            WorkbookIngestToken.revoked_at.is_(None),
        )
        .update({"revoked_at": now}, synchronize_session=False)
    )

    raw, token_hash, prefix = ingest_svc.generate_token()
    user_id = getattr(ctx.user, "id", None)
    tok = WorkbookIngestToken(
        workspace_id=ctx.workspace_id,
        workbook_id=workbook_id,
        token_hash=token_hash,
        prefix=prefix,
        created_by=user_id if isinstance(user_id, int) else None,
    )
    db.add(tok)
    db.commit()
    db.refresh(tok)
    return {
        "token": raw,  # plaintext — shown exactly once, only the hash is stored
        "id": tok.id,
        "prefix": prefix,
        "workbook_id": workbook_id,
        "workspace_id": ctx.workspace_id,
        "created_at": tok.created_at.isoformat() if tok.created_at else None,
        "rotated": rotated,
    }


@router.post("/{workbook_id}/rows/ingest")
async def ingest_rows(
    workbook_id: str,
    body: IngestRowsRequest,
    request: Request,
    db: Session = Depends(get_db),
    session_ctx: Optional[WorkspaceCtx] = Depends(optional_session_workspace),
):
    """Push rows into a workbook (session/JWT or per-workbook ingest token)."""
    _require_enabled()

    # ── auth: session ctx, else ingest token (fail-closed 401) ──
    if session_ctx is not None:
        ws_id = session_ctx.workspace_id
    else:
        raw = _extract_ingest_token(request)
        if not raw:
            raise HTTPException(status_code=401, detail="Missing credentials")
        try:
            ws_id = ingest_svc.resolve_ingest_token(raw, workbook_id)
        except IngestAuthError as e:
            raise HTTPException(status_code=401, detail=str(e))

    if len(body.rows) > MAX_INGEST_ROWS:
        raise HTTPException(
            status_code=413,
            detail=f"too many rows: {len(body.rows)} > {MAX_INGEST_ROWS} per request",
        )

    # Bind the tenant for the whole unit of work (RLS GUC on Postgres; safe to
    # nest when the session path already published it via current_workspace).
    with workspace_scope(ws_id):
        wb = (
            db.query(Workbook)
            .filter(Workbook.id == workbook_id, Workbook.workspace_id == ws_id)
            .first()
        )
        if wb is None:
            raise HTTPException(status_code=404, detail="Workbook not found")

        # ── idempotency replay: return the stored result verbatim ──
        idem_key = (request.headers.get("idempotency-key") or "").strip()[:128]
        if idem_key:
            prior = (
                db.query(WorkbookIngestIdempotency)
                .filter(
                    WorkbookIngestIdempotency.workbook_id == workbook_id,
                    WorkbookIngestIdempotency.idempotency_key == idem_key,
                )
                .first()
            )
            if prior is not None:
                return JSONResponse(
                    content=prior.response, headers={"Idempotent-Replay": "true"}
                )

        # ── map keys → lead fields; drop rows that are empty after mapping ──
        key_map = _key_map(wb)
        unmapped: set = set()
        mapped: list[dict] = []
        skipped_empty = 0
        for raw_row in body.rows:
            row = _map_row(raw_row or {}, key_map, unmapped)
            if not any(v not in (None, "") for v in row.values()):
                skipped_empty += 1
                continue
            mapped.append(row)

        # ── dedup (same identity behavior as the manual add-rows endpoint) ──
        skipped_dupes = 0
        if body.dedupe and mapped:
            seen = set()
            for (data,) in db.query(WorkbookRow.data).filter(
                WorkbookRow.workbook_id == workbook_id
            ):
                ident = _identity(data or {})
                if ident:
                    seen.add(ident)
            deduped = []
            for row in mapped:
                ident = _identity(row)
                if ident and ident in seen:
                    skipped_dupes += 1
                    continue
                if ident:
                    seen.add(ident)
                deduped.append(row)
            mapped = deduped

        # ── materialize rows (same shape as add_rows / source_engine) ──
        max_pos = (
            db.query(sa_func.max(WorkbookRow.position))
            .filter(WorkbookRow.workbook_id == workbook_id)
            .scalar()
        ) or 0
        new_rows = []
        for i, row_data in enumerate(mapped):
            r = WorkbookRow(
                workbook_id=workbook_id,
                workspace_id=ws_id,  # denormalized tenant — NOT NULL, RLS binds on it
                position=max_pos + i + 1,
                data=row_data,
                lead_id=None,  # external rows carry no leads-DB identity
                enrichments={},
            )
            db.add(r)
            new_rows.append(r)
        db.flush()

        total = (
            db.query(sa_func.count(WorkbookRow.id))
            .filter(WorkbookRow.workbook_id == workbook_id)
            .scalar()
        ) or 0
        wb.total_rows = total

        result = {
            "workbook_id": workbook_id,
            "added": len(new_rows),
            "skipped_duplicates": skipped_dupes,
            "skipped_empty": skipped_empty,
            "total_rows": total,
            "row_ids": [r.id for r in new_rows],
            "unmapped_keys": sorted(unmapped),
        }

        # ── idempotency record (unique (workbook, key) wins races) ──
        if idem_key:
            db.add(WorkbookIngestIdempotency(
                workspace_id=ws_id,
                workbook_id=workbook_id,
                idempotency_key=idem_key,
                response=result,
            ))
            # Opportunistic prune of stale keys for this workbook.
            cutoff = datetime.now(timezone.utc) - timedelta(days=IDEMPOTENCY_RETENTION_DAYS)
            db.query(WorkbookIngestIdempotency).filter(
                WorkbookIngestIdempotency.workbook_id == workbook_id,
                WorkbookIngestIdempotency.created_at < cutoff,
            ).delete(synchronize_session=False)

        try:
            db.commit()
        except IntegrityError:
            # Concurrent request already recorded this Idempotency-Key — treat
            # ours as the replay and return the winner's stored response.
            db.rollback()
            prior = (
                db.query(WorkbookIngestIdempotency)
                .filter(
                    WorkbookIngestIdempotency.workbook_id == workbook_id,
                    WorkbookIngestIdempotency.idempotency_key == idem_key,
                )
                .first()
            )
            if prior is not None:
                return JSONResponse(
                    content=prior.response, headers={"Idempotent-Replay": "true"}
                )
            raise

        # Automations: on_row_added event (same semantics as add_rows /
        # source-engine materialization; best-effort after commit).
        if new_rows:
            try:
                from apps.api.services.automations import events as _auto_events
                _auto_events.emit_row_added(ws_id, workbook_id, [r.id for r in new_rows])
            except Exception as _e:
                logger.warning("on_row_added emit (ingest) failed: %s", _e)

        return result
