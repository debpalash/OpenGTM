"""Durable audience destination synchronization worker."""

import hashlib
import json
from datetime import datetime, timezone

from apps.api.database import SessionLocal


MEMBER_PAGE_SIZE = 500
AD_BATCH_SIZE = 5000


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


def _cancellation_requested(db, run) -> bool:
    db.refresh(run)
    return run.status in {"cancelling", "cancelled"}


def _delivery_counts(db, workspace_id: str, run_id: str) -> dict[str, int]:
    """Aggregate a run ledger in SQL so terminal paths stay constant-memory."""
    from sqlalchemy import case, func
    from apps.api.services.destinations.models import DestinationDelivery

    attempted, succeeded, failed, skipped = db.query(
        func.coalesce(func.sum(case((DestinationDelivery.attempts > 0, 1), else_=0)), 0),
        func.coalesce(func.sum(case((DestinationDelivery.status == "success", 1), else_=0)), 0),
        func.coalesce(func.sum(case((DestinationDelivery.status == "failed", 1), else_=0)), 0),
        func.coalesce(func.sum(case((DestinationDelivery.status == "skipped", 1), else_=0)), 0),
    ).filter(
        DestinationDelivery.workspace_id == workspace_id,
        DestinationDelivery.run_id == run_id,
    ).one()
    return {
        "attempted": int(attempted),
        "succeeded": int(succeeded),
        "failed": int(failed),
        "skipped": int(skipped),
    }


def _finish_cancelled(db, run, destination) -> None:
    """Finalize cooperative cancellation without erasing completed effects."""
    from apps.api.services.destinations.models import DestinationDelivery

    db.query(DestinationDelivery).filter(
        DestinationDelivery.workspace_id == run.workspace_id,
        DestinationDelivery.run_id == run.id,
        DestinationDelivery.status.in_(("pending", "in_flight")),
    ).update({
        DestinationDelivery.status: "cancelled",
        DestinationDelivery.error: "Run cancelled before delivery",
    }, synchronize_session=False)
    counts = _delivery_counts(db, run.workspace_id, run.id)
    run.status = "cancelled"
    run.error = "Cancellation completed"
    run.finished_at = datetime.now(timezone.utc)
    run.attempted = counts["attempted"]
    run.succeeded = counts["succeeded"]
    run.failed = counts["failed"]
    run.skipped = counts["skipped"]
    if destination is not None and run.succeeded:
        destination.last_success_at = run.finished_at
    db.commit()


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
            DestinationRun.status.in_(("pending", "running", "cancelling")),
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


def reconcile_destination_job_failure(
    job_id: int,
    payload: dict,
    error: str,
    will_retry: bool,
) -> None:
    """Mirror a killed/failed queue attempt into durable activation state.

    This runs in the parent worker, so it also executes when the destination
    subprocess is terminated before its own exception handling can finalize the
    run. Without it, a terminal queue failure leaves a ``running`` run that
    blocks all later syncs for the destination.
    """
    workspace_id = str(payload.get("workspace_id") or "").strip()
    run_id = str(payload.get("run_id") or "").strip()
    if not workspace_id or not run_id:
        raise ValueError("destination failure payload requires workspace_id and run_id")

    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.destinations.models import (
        AudienceDestination,
        DestinationDelivery,
        DestinationRun,
    )

    message = str(error or "destination sync attempt failed")[:1000]
    with workspace_scope(workspace_id):
        with SessionLocal() as db:
            run = db.query(DestinationRun).filter(
                DestinationRun.id == run_id,
                DestinationRun.workspace_id == workspace_id,
            ).first()
            if run is None or run.status in {"completed", "completed_with_errors", "cancelled"}:
                return

            if run.status == "cancelling":
                destination = db.query(AudienceDestination).filter(
                    AudienceDestination.id == run.destination_id,
                    AudienceDestination.workspace_id == workspace_id,
                ).first()
                _finish_cancelled(db, run, destination)
                return

            delivery_scope = db.query(DestinationDelivery).filter(
                DestinationDelivery.workspace_id == workspace_id,
                DestinationDelivery.run_id == run_id,
            )
            if will_retry:
                run.status = "pending"
                run.error = f"Queue retry scheduled: {message}"
                run.finished_at = None
                delivery_scope.filter(DestinationDelivery.status == "in_flight").update({
                    DestinationDelivery.status: "pending",
                    DestinationDelivery.error: run.error,
                }, synchronize_session=False)
            else:
                now = datetime.now(timezone.utc)
                run.status = "failed"
                run.error = f"Final failure: {message}"
                run.finished_at = now
                delivery_scope.filter(DestinationDelivery.status.in_(("pending", "in_flight"))).update({
                    DestinationDelivery.status: "failed",
                    DestinationDelivery.error: run.error,
                }, synchronize_session=False)
                counts = _delivery_counts(db, workspace_id, run_id)
                run.attempted = counts["attempted"]
                run.succeeded = counts["succeeded"]
                run.failed = counts["failed"]
                run.skipped = counts["skipped"]
                destination = db.query(AudienceDestination).filter(
                    AudienceDestination.id == run.destination_id,
                    AudienceDestination.workspace_id == workspace_id,
                ).first()
                if destination is not None:
                    destination.health_status = "degraded"
                    destination.last_error = run.error
            db.commit()


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

    if dtype == "airtable":
        from apps.api.services.integrations.airtable import upsert_record

        cfg = destination.config or {}
        result = await upsert_record(
            mapped,
            str(cfg.get("base_id") or ""),
            str(cfg.get("table") or ""),
            f"dest:{destination.id}:lead:{lead_id}",
            str(cfg.get("idempotency_field") or "OpenGTM ID"),
            bool(cfg.get("typecast", True)),
            destination.workspace_id,
        )
        return {
            "success": bool(result.get("success")),
            "summary": f"Airtable record {result.get('operation')}" if result.get("success") else "",
            "error": result.get("error"),
            "external_id": result.get("record_id"),
        }

    if dtype == "slack":
        from apps.api.services.automations.actions import _act_webhook
        from apps.api.services.workspace.secrets import get_secret

        cfg = destination.config or {}
        webhook_url = get_secret(
            destination.workspace_id,
            str(cfg.get("webhook_secret_ref") or ""),
            "",
        )
        if not webhook_url:
            return {"success": False, "summary": "", "error": "Slack webhook secret not found"}
        default_message = "New audience member: {company} {contact_person} {email}"
        template = str(cfg.get("message_template") or default_message)

        class _SafeFields(dict):
            def __missing__(self, key):
                return ""

        message = template.format_map(_SafeFields({
            key: "" if value is None else str(value) for key, value in mapped.items()
        })).strip()
        webhook = {
            "url": webhook_url,
            "method": "POST",
            "headers": {"Idempotency-Key": idem},
            "body": {
                "text": message[:3000],
                "metadata": {
                    "event_type": "opengtm_audience_member",
                    "event_payload": {
                        "destination_id": destination.id,
                        "lead_id": lead_id,
                    },
                },
            },
        }
        result = await _act_webhook(destination.workspace_id, webhook, snapshot, [])
        return {"success": result.status == "success", "summary": result.summary, "error": result.error}

    return {"success": False, "summary": "", "error": f"unsupported destination '{dtype}'"}


async def _sync_ad_candidates(
    db, run, destination, workspace_id: str, stats: dict,
    operation: str, candidates: list[tuple],
) -> bool:
    """Deliver one bounded paid-media batch; return false when cancelled."""
    if not candidates:
        return True
    if _cancellation_requested(db, run):
        _finish_cancelled(db, run, destination)
        return False
    from apps.api.services.destinations.ads import sync_ad_batch

    for _, delivery, _, _, _ in candidates:
        delivery.status = "in_flight"
        delivery.attempts = (delivery.attempts or 0) + 1
        stats["attempted"] += 1
    db.commit()
    try:
        snapshots = [item[2] if operation == "add" else item[4] for item in candidates]
        result = await sync_ad_batch(
            workspace_id, destination.destination_type,
            destination.config or {}, snapshots, operation=operation,
        )
    except Exception as exc:
        result = type("Result", (), {
            "success": False, "summary": "", "error": str(exc)[:500],
            "external_id": None,
        })()
    for _, delivery, _, _, _ in candidates:
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
    return True


def _keyset_rows(query, id_column, page_size: int):
    """Iterate bounded pages without holding a cursor across transaction commits."""
    last_id = None
    while True:
        page_query = query
        if last_id is not None:
            page_query = page_query.filter(id_column > last_id)
        page = page_query.order_by(id_column.asc()).limit(page_size).all()
        if not page:
            return
        for row in page:
            yield row
        last_id = getattr(page[-1], id_column.key)


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
            if run is None or run.status in {"completed", "completed_with_errors", "failed", "cancelled"}:
                return
            destination = db.query(AudienceDestination).filter(
                AudienceDestination.id == run.destination_id,
                AudienceDestination.workspace_id == workspace_id,
            ).first()
            if run.status == "cancelling":
                _finish_cancelled(db, run, destination)
                return
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

            members = _keyset_rows(db.query(AudienceMember).filter(
                AudienceMember.workspace_id == workspace_id,
                AudienceMember.audience_id == destination.audience_id,
            ), AudienceMember.id, MEMBER_PAGE_SIZE)
            stats = {"attempted": 0, "succeeded": 0, "failed": 0, "skipped": 0}
            pending = []
            ad_add_batch = []
            member_count = 0
            for member in members:
                member_count += 1
                if _cancellation_requested(db, run):
                    _finish_cancelled(db, run, destination)
                    return
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
                if destination.destination_type in {"meta_ads", "google_ads", "linkedin_ads"}:
                    from apps.api.services.destinations.ads import identifiers_supported
                if destination.destination_type in {"meta_ads", "google_ads", "linkedin_ads"} and not identifiers_supported(destination.destination_type, mapped):
                    delivery.status = "skipped"
                    delivery.summary = "No valid email or phone identifier"
                    delivery.error = None
                    delivery.payload = {}
                    stats["skipped"] += 1
                    db.commit()
                    continue
                deferred_batch = destination.destination_type in {
                    "meta_ads", "google_ads", "linkedin_ads", "warehouse_http",
                }
                delivery.status = "pending" if deferred_batch else "in_flight"
                if not deferred_batch:
                    delivery.attempts = (delivery.attempts or 0) + 1
                delivery.error = None
                delivery.payload = mapped if destination.destination_type in {"meta_ads", "google_ads", "linkedin_ads"} else {}
                db.commit()
                if not deferred_batch:
                    stats["attempted"] += 1
                if deferred_batch:
                    candidate = (member, delivery, snapshot, idem, mapped)
                    if destination.destination_type in {"meta_ads", "google_ads", "linkedin_ads"}:
                        ad_add_batch.append(candidate)
                        if len(ad_add_batch) >= AD_BATCH_SIZE:
                            if not await _sync_ad_candidates(
                                db, run, destination, workspace_id, stats,
                                "add", ad_add_batch,
                            ):
                                return
                            ad_add_batch.clear()
                    else:
                        pending.append(candidate)
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

            if ad_add_batch:
                if not await _sync_ad_candidates(
                    db, run, destination, workspace_id, stats,
                    "add", ad_add_batch,
                ):
                    return
                ad_add_batch.clear()

            if destination.destination_type in {"meta_ads", "google_ads", "linkedin_ads"}:
                # Reconcile exits from the latest successful state per lead. Ad
                # delivery payloads contain hashes only, never raw identifiers.
                from sqlalchemy import and_, func
                latest_ids = db.query(
                    DestinationDelivery.lead_id,
                    func.max(DestinationDelivery.id).label("delivery_id"),
                ).filter(
                    DestinationDelivery.workspace_id == workspace_id,
                    DestinationDelivery.destination_id == destination.id,
                    DestinationDelivery.status == "success",
                ).group_by(DestinationDelivery.lead_id).subquery()
                prior_active = _keyset_rows(db.query(DestinationDelivery).join(
                    latest_ids, DestinationDelivery.id == latest_ids.c.delivery_id,
                ).outerjoin(AudienceMember, and_(
                    AudienceMember.workspace_id == workspace_id,
                    AudienceMember.audience_id == destination.audience_id,
                    AudienceMember.lead_id == DestinationDelivery.lead_id,
                )).filter(
                    DestinationDelivery.operation == "upsert",
                    AudienceMember.id.is_(None),
                ), DestinationDelivery.id, MEMBER_PAGE_SIZE)
                removal_batch = []
                for prior in prior_active:
                    if not prior.payload:
                        continue
                    mapped = dict(prior.payload)
                    fingerprint = _fingerprint(mapped)
                    idem = f"dest:{destination.id}:lead:{prior.lead_id}:remove:{fingerprint}"
                    removal = db.query(DestinationDelivery).filter(
                        DestinationDelivery.workspace_id == workspace_id,
                        DestinationDelivery.idempotency_key == idem,
                    ).first()
                    if removal is not None and removal.status == "success":
                        stats["skipped"] += 1
                        continue
                    removal = removal or DestinationDelivery(
                        workspace_id=workspace_id, run_id=run.id,
                        destination_id=destination.id, lead_id=prior.lead_id,
                        operation="remove", idempotency_key=idem,
                        payload_fingerprint=fingerprint, payload=mapped,
                    )
                    if removal.id is None:
                        db.add(removal)
                    removal.status = "pending"
                    removal.error = None
                    removal_batch.append((None, removal, mapped, idem, mapped))
                    if len(removal_batch) >= AD_BATCH_SIZE:
                        if not await _sync_ad_candidates(
                            db, run, destination, workspace_id, stats,
                            "remove", removal_batch,
                        ):
                            return
                        removal_batch.clear()
                if removal_batch and not await _sync_ad_candidates(
                    db, run, destination, workspace_id, stats,
                    "remove", removal_batch,
                ):
                    return

            if destination.destination_type == "warehouse_http" and pending:
                if _cancellation_requested(db, run):
                    _finish_cancelled(db, run, destination)
                    return
                from apps.api.services.destinations.warehouse import sync_warehouse_batch
                for _, delivery, _, _, _ in pending:
                    delivery.status = "in_flight"
                    delivery.attempts = (delivery.attempts or 0) + 1
                    stats["attempted"] += 1
                db.commit()
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

            # A cancellation may arrive while the final external request is in
            # flight. Preserve its completed delivery result, then terminate the
            # run instead of overwriting the request with a completed status.
            if _cancellation_requested(db, run):
                _finish_cancelled(db, run, destination)
                return

            run.attempted = stats["attempted"]
            run.succeeded = stats["succeeded"]
            run.failed = stats["failed"]
            run.skipped = stats["skipped"]
            run.status = "completed" if not stats["failed"] else "completed_with_errors"
            run.finished_at = datetime.now(timezone.utc)
            destination.health_status = "healthy" if not stats["failed"] else "degraded"
            destination.last_error = None if not stats["failed"] else f"{stats['failed']} deliveries failed"
            if stats["succeeded"] or (member_count == 0 and not stats["failed"]):
                destination.last_success_at = run.finished_at
            db.commit()
