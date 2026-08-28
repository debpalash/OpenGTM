"""Collectors for an exact group of persisted workbook accounts."""

from __future__ import annotations

import asyncio
import hashlib
import json
import re
from types import SimpleNamespace
from typing import Any, Optional

from apps.api.services.poller.sources import DetectedEvent


_PARTNERSHIP_TERMS = (
    "partnership",
    "partnerships",
    "alliances",
    "partner development",
    "partner ecosystem",
    "channel partner",
    "business development",
)


def _health_key(account_id: str, signal_type: str) -> str:
    return f"{account_id}:{signal_type}"


def _fingerprint_html(text: str) -> str:
    # Strip volatile markup and collapse whitespace before hashing. The hash is
    # persisted, not the customer's page body.
    visible = re.sub(r"<script\b[^>]*>.*?</script>", " ", text, flags=re.I | re.S)
    visible = re.sub(r"<style\b[^>]*>.*?</style>", " ", visible, flags=re.I | re.S)
    visible = re.sub(r"<[^>]+>", " ", visible)
    visible = re.sub(r"\s+", " ", visible).strip().lower()
    return hashlib.sha256(visible[:500_000].encode()).hexdigest()


async def _fetch_pricing(url: str) -> dict[str, Any]:
    import httpx

    from apps.api.core.url_guard import BlockedUrlError, guarded_get

    try:
        async with httpx.AsyncClient(
            timeout=httpx.Timeout(12.0, connect=5.0),
            follow_redirects=False,
            headers={"User-Agent": "OpenGTM Signal Monitor/1.0"},
        ) as client:
            response = await guarded_get(client, url)
        if response.status_code == 404:
            return {"ok": True, "present": False, "url": url, "fingerprint": "missing"}
        response.raise_for_status()
        content_type = response.headers.get("content-type", "")
        if "html" not in content_type.lower():
            return {"ok": False, "error_class": "pricing_not_html"}
        return {
            "ok": True,
            "present": True,
            "url": str(response.url),
            "fingerprint": _fingerprint_html(response.text),
        }
    except BlockedUrlError:
        return {"ok": False, "error_class": "pricing_url_blocked"}
    except httpx.TimeoutException:
        return {"ok": False, "error_class": "pricing_timeout"}
    except httpx.HTTPStatusError as exc:
        return {"ok": False, "error_class": f"pricing_http_{exc.response.status_code}"}
    except httpx.HTTPError:
        return {"ok": False, "error_class": "pricing_transport_error"}


def _partnership_jobs(account: dict[str, Any]) -> dict[str, Any]:
    from apps.api.services.leadgen.enrichment.providers.jobspy_signals import (
        JobSpySignalProvider,
    )
    from apps.api.services.leadgen.models import Lead

    provider = JobSpySignalProvider()
    try:
        result = asyncio.run(provider.enrich(Lead(company=account["company"])))
    except Exception:
        return {"ok": False, "error_class": "hiring_provider_error"}
    if result is not None and result.error == "no_jobs_found":
        return {"ok": True, "items": []}
    if result is None or not result.success:
        return {"ok": False, "error_class": "hiring_provider_unavailable"}
    try:
        payload = json.loads((result.fields or {}).get("hiring_signals") or "{}")
    except (TypeError, ValueError):
        return {"ok": False, "error_class": "hiring_payload_invalid"}
    items = []
    for title in payload.get("roles") or []:
        normalized = str(title).strip()
        if normalized and any(term in normalized.lower() for term in _PARTNERSHIP_TERMS):
            items.append({
                "id": hashlib.sha256(normalized.lower().encode()).hexdigest()[:20],
                "title": normalized,
                "url": "",
            })
    return {"ok": True, "items": items}


def _sec_events(
    watch: Any,
    account: dict[str, Any],
    account_cursor: dict[str, Any],
    *,
    backfill: bool,
) -> dict[str, Any]:
    from apps.api.services.poller import sources

    virtual = SimpleNamespace(
        id=f"{watch.id}:{account['account_id']}",
        target=account["company"],
        resolved_cik=account_cursor.get("resolved_cik"),
        cursor={
            **dict(account_cursor.get("sec") or {}),
            "bootstrapped": bool(account_cursor.get("bootstrapped")),
        },
    )
    events, patch = sources.fetch_funding_and_exec(
        virtual,
        want_funding=True,
        want_exec=True,
        backfill=backfill,
    )
    if events is None and patch is None:
        return {"ok": False, "error_class": "sec_provider_unavailable"}
    patch = dict(patch or {})
    resolved_cik = patch.pop("_resolved_cik", None) or virtual.resolved_cik
    return {
        "ok": True,
        "events": list(events or []),
        "cursor": patch,
        "resolved_cik": resolved_cik,
    }


def _default_observations(
    watch: Any,
    account: dict[str, Any],
    account_cursor: dict[str, Any],
    requested: set[str],
    *,
    backfill: bool,
) -> dict[str, dict[str, Any]]:
    observed: dict[str, dict[str, Any]] = {}
    if "partnership_hiring" in requested:
        observed["partnership_hiring"] = _partnership_jobs(account)
    if requested.intersection({"funding", "leadership_change"}):
        sec = _sec_events(watch, account, account_cursor, backfill=backfill)
        if not sec.get("ok"):
            for signal in requested.intersection({"funding", "leadership_change"}):
                observed[signal] = dict(sec)
        else:
            mapping = {
                "funding": "company_funded",
                "leadership_change": "executive_hired",
            }
            for signal, source_type in mapping.items():
                if signal in requested:
                    observed[signal] = {
                        "ok": True,
                        "events": [event for event in sec["events"] if event.signal_type == source_type],
                        "cursor": sec["cursor"],
                        "resolved_cik": sec.get("resolved_cik"),
                    }
    if "pricing_page_change" in requested:
        observed["pricing_page_change"] = asyncio.run(
            _fetch_pricing(f"{account['website'].rstrip('/')}/pricing")
        )
    return observed


def fetch_account_group(
    watch: Any,
    *,
    backfill: bool,
    observations_override: Optional[dict[str, dict[str, dict[str, Any]]]] = None,
) -> tuple[list[DetectedEvent], dict[str, Any]]:
    """Diff all requested collectors for every exact account.

    ``observations_override`` is the recorded-provider seam. Its shape is
    ``{account_id: {signal_type: observation}}`` and exercises the same cursor,
    failure, and event logic as live collectors.
    """
    config = watch.config if isinstance(watch.config, dict) else {}
    accounts = config.get("accounts") if isinstance(config.get("accounts"), list) else []
    requested = set(watch.signal_types or [])
    cursor = watch.cursor if isinstance(watch.cursor, dict) else {}
    group_cursor = dict(cursor.get("account_group") or {})
    health = dict(cursor.get("collector_health") or {})
    failures: list[str] = []
    emitted: list[DetectedEvent] = []

    for account in accounts:
        if not isinstance(account, dict) or not account.get("account_id"):
            continue
        account_id = str(account["account_id"])
        account_cursor = dict(group_cursor.get(account_id) or {})
        bootstrapped = bool(account_cursor.get("bootstrapped"))
        suppress = not bootstrapped and not backfill
        observed = (
            dict((observations_override or {}).get(account_id) or {})
            if observations_override is not None
            else _default_observations(
                watch, account, account_cursor, requested, backfill=backfill
            )
        )

        known_jobs = list(account_cursor.get("partnership_job_ids") or [])
        known_job_set = set(known_jobs)
        for signal_type in sorted(requested):
            key = _health_key(account_id, signal_type)
            prior_health = dict(health.get(key) or {})
            attempt_count = int(prior_health.get("attempt_count") or 0) + 1
            observation = dict(observed.get(signal_type) or {})
            if observation.get("ok") is not True:
                error_class = str(observation.get("error_class") or "collector_unavailable")
                failures.append(error_class)
                health[key] = {
                    "state": "failed",
                    "attempt_count": attempt_count,
                    "last_error_class": error_class,
                }
                continue

            health[key] = {
                "state": "healthy",
                "attempt_count": attempt_count,
                "last_error_class": None,
            }
            if signal_type == "partnership_hiring":
                for item in observation.get("items") or []:
                    item_id = str(item.get("id") or "").strip()
                    if not item_id or item_id in known_job_set:
                        continue
                    known_job_set.add(item_id)
                    known_jobs.append(item_id)
                    if not suppress:
                        emitted.append(DetectedEvent(
                            natural_event_id=f"{account_id}:partnership_job:{item_id}",
                            signal_type="partnership_hiring",
                            title=f"Partnership role opened: {item.get('title') or 'Untitled role'}",
                            description=f"New partnership hiring evidence for {account['company']}",
                            source="jobspy",
                            source_url=str(item.get("url") or ""),
                            weight=8,
                            lead_id=account.get("lead_id"),
                            company=account["company"],
                        ))
            elif signal_type in {"funding", "leadership_change"}:
                source_signal = "company_funded" if signal_type == "funding" else "executive_hired"
                for event in observation.get("events") or []:
                    if getattr(event, "signal_type", "") != source_signal:
                        continue
                    if suppress:
                        continue
                    emitted.append(DetectedEvent(
                        natural_event_id=f"{account_id}:{event.natural_event_id}",
                        signal_type=signal_type,
                        title=event.title,
                        description=event.description,
                        source=event.source,
                        source_url=event.source_url,
                        weight=event.weight,
                        occurred_at=event.occurred_at,
                        lead_id=account.get("lead_id"),
                        company=account["company"],
                    ))
                if observation.get("cursor"):
                    account_cursor["sec"] = dict(observation["cursor"])
                if observation.get("resolved_cik"):
                    account_cursor["resolved_cik"] = observation["resolved_cik"]
            elif signal_type == "pricing_page_change":
                fingerprint = str(observation.get("fingerprint") or "")
                previous = str(account_cursor.get("pricing_fingerprint") or "")
                if fingerprint:
                    account_cursor["pricing_fingerprint"] = fingerprint
                    account_cursor["pricing_url"] = str(observation.get("url") or "")
                if previous and fingerprint and previous != fingerprint and not suppress:
                    emitted.append(DetectedEvent(
                        natural_event_id=f"{account_id}:pricing:{fingerprint[:20]}",
                        signal_type="pricing_page_change",
                        title="Pricing page changed",
                        description=f"The saved pricing-page fingerprint changed for {account['company']}",
                        source="company_website",
                        source_url=str(observation.get("url") or account["website"]),
                        weight=8,
                        lead_id=account.get("lead_id"),
                        company=account["company"],
                    ))

        account_cursor["partnership_job_ids"] = known_jobs[-500:]
        account_cursor["bootstrapped"] = True
        group_cursor[account_id] = account_cursor

    return emitted, {
        "account_group": group_cursor,
        "collector_health": health,
        "_collector_failures": failures,
    }
