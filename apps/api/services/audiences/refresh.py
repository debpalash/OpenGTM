"""Page-bounded audience materialization with durable membership events."""

import uuid
from datetime import datetime, timezone

from sqlalchemy import or_
from sqlalchemy.orm import Session

from apps.api.core.tenancy import WorkspaceCtx
from apps.api.services.audiences.models import Audience, AudienceMember, AudienceMembershipEvent


AUDIENCE_REFRESH_PAGE_SIZE = 500


def _emit_refresh_events(
    db: Session, workspace_id: str, audience_id: str, refresh_id: str,
) -> None:
    """Emit one refresh's events in commit-safe, stable keyset pages."""
    from apps.api.services.automations.events import emit_audience_membership

    last_id = None
    while True:
        query = db.query(AudienceMembershipEvent).filter(
            AudienceMembershipEvent.workspace_id == workspace_id,
            AudienceMembershipEvent.audience_id == audience_id,
            AudienceMembershipEvent.refresh_id == refresh_id,
        )
        if last_id is not None:
            query = query.filter(AudienceMembershipEvent.id > last_id)
        events = query.order_by(AudienceMembershipEvent.id.asc()).limit(
            AUDIENCE_REFRESH_PAGE_SIZE,
        ).all()
        if not events:
            return
        last_id = events[-1].id
        emit_audience_membership(db, workspace_id, events)


def refresh_audience(db: Session, ctx: WorkspaceCtx, audience: Audience) -> dict:
    """Re-evaluate matching leads and atomically apply a page-bounded diff."""
    # Serialize scheduled and manual refreshes for this audience. PostgreSQL
    # holds the row lock through the atomic diff; SQLite serializes writers.
    audience = db.query(Audience).filter(
        Audience.id == audience.id,
        Audience.workspace_id == ctx.workspace_id,
    ).with_for_update().one()
    lead_store = ctx.lead_db()
    refresh_id = str(uuid.uuid4())
    now = datetime.now(timezone.utc)
    entered = changed = matched = 0
    page = 1
    try:
        while True:
            batch, total = lead_store.query_leads_page(
                audience.filters or {}, page=page,
                page_size=AUDIENCE_REFRESH_PAGE_SIZE,
            )
            snapshots = {
                int(row["id"]): row for row in batch if row.get("id") is not None
            }
            existing = {
                member.lead_id: member
                for member in db.query(AudienceMember).filter(
                    AudienceMember.workspace_id == ctx.workspace_id,
                    AudienceMember.audience_id == audience.id,
                    AudienceMember.lead_id.in_(snapshots),
                ).all()
            } if snapshots else {}
            for lead_id, snapshot in snapshots.items():
                member = existing.get(lead_id)
                if member is None:
                    db.add(AudienceMember(
                        workspace_id=ctx.workspace_id,
                        audience_id=audience.id,
                        lead_id=lead_id,
                        snapshot=snapshot,
                        refresh_token=refresh_id,
                        joined_at=now,
                        last_seen_at=now,
                    ))
                    db.add(AudienceMembershipEvent(
                        workspace_id=ctx.workspace_id,
                        audience_id=audience.id,
                        lead_id=lead_id,
                        event_type="entered",
                        snapshot=snapshot,
                        refresh_id=refresh_id,
                    ))
                    entered += 1
                else:
                    if (member.snapshot or {}) != snapshot:
                        changed += 1
                    member.snapshot = snapshot
                    member.refresh_token = refresh_id
                    member.last_seen_at = now
            matched += len(snapshots)
            db.flush()
            if matched >= total or not batch:
                break
            page += 1
    finally:
        lead_store.close()

    exited = 0
    last_member_id = None
    while True:
        query = db.query(AudienceMember).filter(
            AudienceMember.workspace_id == ctx.workspace_id,
            AudienceMember.audience_id == audience.id,
            or_(
                AudienceMember.refresh_token.is_(None),
                AudienceMember.refresh_token != refresh_id,
            ),
        )
        if last_member_id is not None:
            query = query.filter(AudienceMember.id > last_member_id)
        members = query.order_by(AudienceMember.id.asc()).limit(
            AUDIENCE_REFRESH_PAGE_SIZE,
        ).all()
        if not members:
            break
        last_member_id = members[-1].id
        for member in members:
            db.add(AudienceMembershipEvent(
                workspace_id=ctx.workspace_id,
                audience_id=audience.id,
                lead_id=member.lead_id,
                event_type="exited",
                snapshot=member.snapshot or {},
                refresh_id=refresh_id,
            ))
            db.delete(member)
            exited += 1
        db.flush()

    audience.member_count = matched
    audience.refreshed_at = now
    db.commit()

    if entered or exited:
        _emit_refresh_events(db, ctx.workspace_id, audience.id, refresh_id)
    if entered or exited or changed:
        from apps.api.services.destinations.engine import enqueue_audience_syncs
        enqueue_audience_syncs(db, ctx.workspace_id, audience.id)

    return {
        "audience": audience.to_api(),
        "entered": entered,
        "exited": exited,
        "unchanged": matched - entered - changed,
        "changed": changed,
    }


def refresh_audience_observed(db: Session, ctx: WorkspaceCtx, audience: Audience) -> dict:
    """Refresh while persisting health even when source evaluation raises."""
    try:
        result = refresh_audience(db, ctx, audience)
    except Exception as exc:
        db.rollback()
        audience = db.query(Audience).filter(
            Audience.id == audience.id,
            Audience.workspace_id == ctx.workspace_id,
        ).one()
        audience.refresh_health = "degraded"
        audience.last_refresh_error = f"{type(exc).__name__}: {exc}"[:1000]
        audience.consecutive_refresh_failures = (audience.consecutive_refresh_failures or 0) + 1
        db.commit()
        raise
    audience.refresh_health = "healthy"
    audience.last_refresh_error = None
    audience.consecutive_refresh_failures = 0
    db.commit()
    result["audience"] = audience.to_api()
    return result
