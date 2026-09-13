"""Durable audience destination synchronization worker."""

import hashlib
import json
from datetime import datetime, timezone

from apps.api.database import SessionLocal


def _fingerprint(payload: dict) -> str:
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"), default=str).encode()
    return hashlib.sha256(encoded).hexdigest()


def _map_payload(snapshot: dict, field_map: dict) -> dict:
    if not field_map:
        return dict(snapshot)
    return {
        target: snapshot.get(source)
        for source, target in field_map.items()
        if snapshot.get(source) not in (None, "")
    }


def enqueue_audience_syncs(db, workspace_id: str, audience_id: str) -> int:
    """Enqueue one durable sync for every enabled destination without an active run."""
    from apps.api.services.destinations.models import AudienceDestination, DestinationRun
    from apps.api.services.queue_service import queue_service

    destinations = db.query(AudienceDestination).filter(
        AudienceDestination.workspace_id == workspace_id,
        AudienceDestination.audience_id == audience_id,
        AudienceDestination.enabled.is_(True),
    ).all()
    enqueued = 0
    for destination in destinations:
        active = db.query(DestinationRun).filter(
            DestinationRun.workspace_id == workspace_id,
            DestinationRun.destination_id == destination.id,
            DestinationRun.status.in_(("pending", "running")),
        ).first()
        if active:
            continue
        run = DestinationRun(
            workspace_id=workspace_id, destination_id=destination.id,
            requested_by="audience_refresh",
        )
        db.add(run)
        db.commit()
        db.refresh(run)
        queue_service.add_job(
            db, "audience_destination_sync",
            {"workspace_id": workspace_id, "run_id": run.id},
            fire_key=f"destination_sync:{run.id}",
        )
        enqueued += 1
    return enqueued


async def _deliver(destination, lead_id: int, snapshot: dict, idem: str) -> dict:
    dtype = destination.destination_type
    mapped = _map_payload(snapshot, destination.field_map or {})
    if dtype == "webhook":
        from apps.api.services.automations.actions import _act_webhook

        cfg = dict(destination.config or {})
        headers = dict(cfg.get("headers") or {})
        headers["Idempotency-Key"] = idem
        cfg["headers"] = headers
        cfg["body"] = {
            "event": "audience.member.upsert",
            "audience_id": destination.audience_id,
            "destination_id": destination.id,
            "lead_id": lead_id,
            "data": mapped,
        }
        result = await _act_webhook(destination.workspace_id, cfg, snapshot, [])
        return {"success": result.status == "success", "summary": result.summary, "error": result.error}

    if dtype in {"hubspot", "salesforce"}:
        from apps.api.services.automations.actions import _act_push_crm

        cfg = {**(destination.config or {}), "type": dtype, "field_map": destination.field_map or {}}
        result = await _act_push_crm(destination.workspace_id, cfg, snapshot, [], lead_id)
        return {"success": result.status == "success", "summary": result.summary, "error": result.error}

    if dtype == "instantly":
        from apps.api.services.integrations.instantly import add_lead_to_campaign

        cfg = destination.config or {}
        result = await add_lead_to_campaign(
            str(cfg.get("campaign_id") or ""),
            mapped,
            bool(cfg.get("skip_if_in_campaign", True)),
            destination.workspace_id,
        )
        return {
            "success": bool(result.get("success")),
            "summary": (
                "Lead already in campaign"
                if result.get("duplicate")
                else "Lead added to campaign"
                if result.get("success")
                else ""
            ),
            "error": result.get("error"),
            "external_id": result.get("lead_id"),
        }

    if dtype == "smartlead":
        from apps.api.services.integrations.smartlead import add_lead_to_campaign

        cfg = destination.config or {}
        result = await add_lead_to_campaign(
            str(cfg.get("campaign_id") or ""),
            mapped,
            cfg.get("settings"),
            destination.workspace_id,
        )
        return {
            "success": bool(result.get("success")),
            "summary": (
                "Lead already in campaign"
                if result.get("duplicate")
                else "Lead added to campaign"
                if result.get("success")
                else ""
            ),
            "error": result.get("error"),
        }

    if dtype == "google_sheets":
        from apps.api.services.integrations.sheets import upsert_row

        cfg = destination.config or {}
        columns = cfg.get("columns") or []
        result = await upsert_row(
            str(cfg.get("spreadsheet_id") or ""),
            [mapped.get(column, "") for column in columns],
            f"dest:{destination.id}:lead:{lead_id}",
            str(cfg.get("range") or "Sheet1!A:ZZ"),
            destination.workspace_id,
        )
        return {
            "success": bool(result.get("success")),
            "summary": f"Sheet row {result.get('operation')}" if result.get("success") else "",
            "error": result.get("error"),
            "external_id": result.get("range"),
        }

    return {"success": False, "summary": "", "error": f"unsupported destination '{dtype}'"}


async def handle_destination_sync(job_id: int, payload: dict) -> None:
    workspace_id = payload.get("workspace_id")
    run_id = payload.get("run_id")
    if not workspace_id or not run_id:
        raise ValueError("destination sync requires workspace_id and run_id")

    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.audiences.models import AudienceMember
    from apps.api.services.destinations.models import AudienceDestination, DestinationDelivery, DestinationRun

    with workspace_scope(workspace_id):
        with SessionLocal() as db:
            run = db.query(DestinationRun).filter(
                DestinationRun.id == run_id, DestinationRun.workspace_id == workspace_id,
            ).first()
            if run is None or run.status == "completed":
                return
            destination = db.query(AudienceDestination).filter(
                AudienceDestination.id == run.destination_id,
                AudienceDestination.workspace_id == workspace_id,
            ).first()
            if destination is None or not destination.enabled:
                run.status = "cancelled"
                run.error = "destination missing or disabled"
                run.finished_at = datetime.now(timezone.utc)
                db.commit()
                return
            run.status = "running"
            run.started_at = run.started_at or datetime.now(timezone.utc)
            run.error = None
            db.commit()

            members = db.query(AudienceMember).filter(
                AudienceMember.workspace_id == workspace_id,
                AudienceMember.audience_id == destination.audience_id,
            ).order_by(AudienceMember.lead_id.asc()).all()
            stats = {"attempted": 0, "succeeded": 0, "failed": 0, "skipped": 0}
            pending = []
            for member in members:
                snapshot = dict(member.snapshot or {})
                mapped = _map_payload(snapshot, destination.field_map or {})
                if destination.destination_type in {"meta_ads", "google_ads", "linkedin_ads"}:
                    from apps.api.services.destinations.ads import hashed_identifiers
                    mapped = hashed_identifiers(snapshot)
                fingerprint = _fingerprint(mapped)
                idem = f"dest:{destination.id}:lead:{member.lead_id}:{fingerprint}"
                prior = db.query(DestinationDelivery).filter(
                    DestinationDelivery.workspace_id == workspace_id,
                    DestinationDelivery.idempotency_key == idem,
                ).first()
                if prior is not None and prior.status == "success":
                    stats["skipped"] += 1
                    continue
                delivery = prior or DestinationDelivery(
                    workspace_id=workspace_id, run_id=run.id, destination_id=destination.id,
                    lead_id=member.lead_id, operation="upsert", idempotency_key=idem,
                    payload_fingerprint=fingerprint,
                )
                if prior is None:
                    db.add(delivery)
                delivery.status = "in_flight"
                delivery.attempts = (delivery.attempts or 0) + 1
                delivery.error = None
                db.commit()
                stats["attempted"] += 1
                pending.append((member, delivery, snapshot, idem, mapped))
                if destination.destination_type in {"meta_ads", "google_ads", "linkedin_ads", "warehouse_http"}:
                    continue
                try:
                    result = await _deliver(destination, member.lead_id, snapshot, idem)
                except Exception as exc:
                    result = {"success": False, "summary": "", "error": str(exc)[:500]}
                delivery.status = "success" if result.get("success") else "failed"
                delivery.summary = result.get("summary") or ""
                delivery.error = (result.get("error") or "")[:1000] or None
                delivery.external_id = result.get("external_id")
                if result.get("success"):
                    delivery.delivered_at = datetime.now(timezone.utc)
                    stats["succeeded"] += 1
                else:
                    stats["failed"] += 1
                db.commit()

            if destination.destination_type in {"meta_ads", "google_ads", "linkedin_ads"} and pending:
                from apps.api.services.destinations.ads import sync_ad_batch
                for offset in range(0, len(pending), 5000):
                    batch = pending[offset:offset + 5000]
                    try:
                        result = await sync_ad_batch(workspace_id, destination.destination_type, destination.config or {}, [x[2] for x in batch])
                    except Exception as exc:
                        result = type("Result", (), {"success": False, "summary": "", "error": str(exc)[:500], "external_id": None})()
                    for _, delivery, _, _, _ in batch:
                        delivery.status = "success" if result.success else "failed"
                        delivery.summary = result.summary
                        delivery.error = (result.error or "")[:1000] or None
                        delivery.external_id = result.external_id
                        if result.success:
                            delivery.delivered_at = datetime.now(timezone.utc)
                            stats["succeeded"] += 1
                        else:
                            stats["failed"] += 1
                    db.commit()

            if destination.destination_type == "warehouse_http" and pending:
                from apps.api.services.destinations.warehouse import sync_warehouse_batch
                try:
                    result = await sync_warehouse_batch(workspace_id, destination, run.id, [(item[0].lead_id, item[4]) for item in pending])
                except Exception as exc:
                    result = type("Result", (), {"success": False, "summary": "", "error": str(exc)[:500], "external_id": None})()
                for _, delivery, _, _, _ in pending:
                    delivery.status = "success" if result.success else "failed"
                    delivery.summary, delivery.error, delivery.external_id = result.summary, (result.error or "")[:1000] or None, result.external_id
                    if result.success:
                        delivery.delivered_at = datetime.now(timezone.utc); stats["succeeded"] += 1
                    else:
                        stats["failed"] += 1
                db.commit()

            run.attempted = stats["attempted"]
            run.succeeded = stats["succeeded"]
            run.failed = stats["failed"]
            run.skipped = stats["skipped"]
            run.status = "completed" if not stats["failed"] else "completed_with_errors"
            run.finished_at = datetime.now(timezone.utc)
            destination.health_status = "healthy" if not stats["failed"] else "degraded"
            destination.last_error = None if not stats["failed"] else f"{stats['failed']} deliveries failed"
            if stats["succeeded"] or (not members and not stats["failed"]):
                destination.last_success_at = run.finished_at
            db.commit()
