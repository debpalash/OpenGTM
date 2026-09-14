"""Materialize an audience and record deterministic membership changes."""

from datetime import datetime, timezone

from sqlalchemy.orm import Session

from apps.api.core.tenancy import WorkspaceCtx
from apps.api.services.audiences.models import Audience, AudienceMember, AudienceMembershipEvent


def refresh_audience(db: Session, ctx: WorkspaceCtx, audience: Audience) -> dict:
    """Re-evaluate all matching leads and atomically apply the membership diff."""
    lead_store = ctx.lead_db()
    rows: list[dict] = []
    page = 1
    page_size = 500
    try:
        while True:
            batch, total = lead_store.query_leads_page(
                audience.filters or {}, page=page, page_size=page_size,
            )
            rows.extend(batch)
            if len(rows) >= total or not batch:
                break
            page += 1
    finally:
        lead_store.close()

    snapshots = {int(row["id"]): row for row in rows if row.get("id") is not None}
    existing = {
        member.lead_id: member for member in db.query(AudienceMember).filter(
            AudienceMember.workspace_id == ctx.workspace_id,
            AudienceMember.audience_id == audience.id,
        ).all()
    }
    entered_ids = sorted(set(snapshots) - set(existing))
    exited_ids = sorted(set(existing) - set(snapshots))
    changed_ids = sorted(
        lead_id for lead_id in set(snapshots) & set(existing)
        if (existing[lead_id].snapshot or {}) != snapshots[lead_id]
    )
    now = datetime.now(timezone.utc)
    events: list[AudienceMembershipEvent] = []

    for lead_id in entered_ids:
        snapshot = snapshots[lead_id]
        db.add(AudienceMember(
            workspace_id=ctx.workspace_id, audience_id=audience.id,
            lead_id=lead_id, snapshot=snapshot, joined_at=now, last_seen_at=now,
        ))
        event = AudienceMembershipEvent(
            workspace_id=ctx.workspace_id, audience_id=audience.id,
            lead_id=lead_id, event_type="entered", snapshot=snapshot,
        )
        db.add(event)
        events.append(event)

    for lead_id in set(snapshots) & set(existing):
        existing[lead_id].snapshot = snapshots[lead_id]
        existing[lead_id].last_seen_at = now

    for lead_id in exited_ids:
        member = existing[lead_id]
        event = AudienceMembershipEvent(
            workspace_id=ctx.workspace_id, audience_id=audience.id,
            lead_id=lead_id, event_type="exited", snapshot=member.snapshot or {},
        )
        db.add(event)
        events.append(event)
        db.delete(member)

    audience.member_count = len(snapshots)
    audience.refreshed_at = now
    db.commit()
    for event in events:
        db.refresh(event)

    if events:
        from apps.api.services.automations.events import emit_audience_membership
        emit_audience_membership(db, ctx.workspace_id, events)
    if entered_ids or exited_ids or changed_ids:
        from apps.api.services.destinations.engine import enqueue_audience_syncs
        enqueue_audience_syncs(db, ctx.workspace_id, audience.id)

    return {
        "audience": audience.to_api(),
        "entered": len(entered_ids),
        "exited": len(exited_ids),
        "unchanged": len(snapshots) - len(entered_ids) - len(changed_ids),
        "changed": len(changed_ids),
    }


def refresh_audience_observed(db: Session, ctx: WorkspaceCtx, audience: Audience) -> dict:
    """Refresh while persisting health even when source evaluation raises."""
    try:
        result = refresh_audience(db, ctx, audience)
    except Exception as exc:
        db.rollback()
        # Reload after rollback so health evidence is written independently of
        # an interrupted membership transaction.
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
