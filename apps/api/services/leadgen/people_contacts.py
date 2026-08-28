"""Exact-person work-email discovery with an auditable verification contract.

This workflow starts from people already selected by stable ``person_id``. It
does not search for replacement people and it never treats an email-finder
claim as deliverability evidence. Discovery and verification are separate
stages, and every attempted provider is returned with a normalized outcome.
"""

from __future__ import annotations

import asyncio
from datetime import datetime, timezone
import hashlib
import re
import time
from typing import Any, Iterable

from apps.api.services.leadgen.dedup import normalize_domain
from apps.api.services.leadgen.enrichment.email_deliverability import (
    DeliverabilityResult,
    verify_deliverability,
)
from apps.api.services.leadgen.enrichment.licenses import resolve_license
from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.targeted_people import (
    people_result_set_id,
    person_entity_id,
)
from apps.api.services.workbook.provider_runner import run_provider


EXACT_EMAIL_PROVIDERS = ("prospeo", "hunter_io")
_EXACT_METHODS = {
    "prospeo": frozenset({"linkedin", "exact_name"}),
    "hunter_io": frozenset({"exact_name"}),
}
_EMAIL_RE = re.compile(r"^[^\s@]+@[^\s@]+\.[^\s@]+$")


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def _identity_text(value: Any) -> str:
    return re.sub(r"[^a-z0-9]+", " ", str(value or "").lower()).strip()


def _email_matches_company(email: str, canonical_domain: str) -> bool:
    if not _EMAIL_RE.fullmatch(email):
        return False
    email_domain = normalize_domain(email.rsplit("@", 1)[-1])
    target = normalize_domain(canonical_domain)
    return bool(
        email_domain
        and target
        and (email_domain == target or email_domain.endswith(f".{target}"))
    )


def _safe_discovery_failure(error: str) -> tuple[str, str]:
    normalized = str(error or "").lower()
    if "api key" in normalized or "configured" in normalized:
        return "unavailable", "provider_not_configured"
    if "rate" in normalized or "429" in normalized:
        return "error", "rate_limited"
    return "no_data", "no_exact_email"


async def _run_discovery_provider(
    provider_name: str,
    lead: Lead,
    *,
    timeout: float,
) -> dict | None:
    return await run_provider(provider_name, lead, timeout)


async def _verify_discovered_email(
    email: str,
    *,
    workspace_id: str,
) -> DeliverabilityResult:
    return await verify_deliverability(email, workspace_id=workspace_id or None)


def _verification_attempts(verdict: DeliverabilityResult) -> list[dict[str, Any]]:
    raw_attempts = getattr(verdict, "verification_attempts", None) or []
    if raw_attempts:
        return [
            {
                "stage": "verification",
                "provider": str(attempt.get("provider") or "verification_cascade"),
                "status": str(attempt.get("status") or "unknown"),
                "detail": str(attempt.get("detail") or "")[:80],
                "source_license": resolve_license(str(attempt.get("provider") or "")),
            }
            for attempt in raw_attempts
            if isinstance(attempt, dict)
        ]
    return [{
        "stage": "verification",
        "provider": verdict.source or "verification_cascade",
        "status": verdict.status or "unknown",
        "detail": "",
        "source_license": resolve_license(verdict.source),
    }]


def _public_contact_status(verdict: DeliverabilityResult) -> str:
    if verdict.status == "invalid" or verdict.confidence == "":
        return "invalid"
    if verdict.status == "catch_all":
        return "catch_all"
    if verdict.status == "valid" and verdict.confidence == "verified":
        return "verified"
    return "risky"


async def _enrich_one_person(
    person: dict[str, Any],
    *,
    company: str,
    canonical_domain: str,
    workspace_id: str,
    provider_timeout: float,
) -> dict[str, Any]:
    observed_at = _now_iso()
    attempts: list[dict[str, Any]] = []
    base = dict(person)
    base["person_id"] = person_entity_id(company, base)
    name = str(base.get("name") or base.get("full_name") or "").strip()

    if not canonical_domain:
        return {
            **base,
            "email": None,
            "contactability": {
                "email": None,
                "status": "unavailable",
                "finder_provider": "",
                "verifier_provider": "",
                "discovery_confidence": 0.0,
                "verification_status": "not_run",
                "observed_at": observed_at,
                "attempts": [{
                    "stage": "precondition",
                    "provider": "company_identity",
                    "status": "blocked",
                    "detail": "canonical_company_unresolved",
                    "source_license": "unknown",
                }],
                "exhausted": True,
            },
        }

    lead = Lead(
        company=company,
        website=f"https://{canonical_domain}",
        contact_person=name,
        contact_title=str(base.get("title") or ""),
        linkedin_url=str(base.get("linkedin_url") or ""),
        workspace_id=workspace_id,
    )
    winner: dict[str, Any] | None = None
    winner_email = ""
    winner_provider = ""

    for provider_name in EXACT_EMAIL_PROVIDERS:
        started = time.monotonic()
        try:
            result = await _run_discovery_provider(
                provider_name,
                lead,
                timeout=provider_timeout,
            )
        except asyncio.TimeoutError:
            attempts.append({
                "stage": "discovery",
                "provider": provider_name,
                "status": "timeout",
                "detail": "provider_timeout",
                "duration_ms": round((time.monotonic() - started) * 1000),
                "source_license": resolve_license(provider_name),
            })
            continue
        except Exception as exc:
            attempts.append({
                "stage": "discovery",
                "provider": provider_name,
                "status": "error",
                "detail": type(exc).__name__,
                "duration_ms": round((time.monotonic() - started) * 1000),
                "source_license": resolve_license(provider_name),
            })
            continue

        duration_ms = round((time.monotonic() - started) * 1000)
        if not result:
            attempts.append({
                "stage": "discovery",
                "provider": provider_name,
                "status": "unavailable",
                "detail": "provider_unavailable",
                "duration_ms": duration_ms,
                "source_license": resolve_license(provider_name),
            })
            continue
        fields = result.get("fields") if isinstance(result.get("fields"), dict) else {}
        email = str(fields.get("email") or "").strip().lower()
        if not result.get("success") or not email:
            status, detail = _safe_discovery_failure(str(result.get("error") or ""))
            attempts.append({
                "stage": "discovery",
                "provider": provider_name,
                "status": status,
                "detail": detail,
                "duration_ms": round(float(result.get("duration_ms") or duration_ms)),
                "source_license": resolve_license(provider_name, result.get("license")),
            })
            continue

        method = str(fields.get("email_match_method") or "").strip().lower()
        provider_person = str(fields.get("contact_person") or "").strip()
        rejection = ""
        if method not in _EXACT_METHODS[provider_name]:
            rejection = "non_exact_match_method"
        elif provider_person and _identity_text(provider_person) != _identity_text(name):
            rejection = "person_identity_mismatch"
        elif not _email_matches_company(email, canonical_domain):
            rejection = "company_domain_mismatch"
        if rejection:
            attempts.append({
                "stage": "discovery",
                "provider": provider_name,
                "status": "rejected",
                "detail": rejection,
                "duration_ms": round(float(result.get("duration_ms") or duration_ms)),
                "source_license": resolve_license(provider_name, result.get("license")),
            })
            continue

        attempts.append({
            "stage": "discovery",
            "provider": provider_name,
            "status": "found",
            "detail": method,
            "duration_ms": round(float(result.get("duration_ms") or duration_ms)),
            "confidence": round(float(result.get("confidence") or 0.0), 4),
            "source_license": resolve_license(provider_name, result.get("license")),
        })
        winner = result
        winner_email = email
        winner_provider = provider_name
        break

    if not winner:
        return {
            **base,
            "email": None,
            "contactability": {
                "email": None,
                "status": "unavailable",
                "finder_provider": "",
                "verifier_provider": "",
                "discovery_confidence": 0.0,
                "verification_status": "not_run",
                "observed_at": observed_at,
                "attempts": attempts,
                "exhausted": True,
            },
        }

    try:
        verdict = await _verify_discovered_email(
            winner_email,
            workspace_id=workspace_id,
        )
    except Exception as exc:
        verdict = DeliverabilityResult(
            email=winner_email,
            confidence="unknown",
            status="unknown",
            source="verification_cascade",
        )
        attempts.append({
            "stage": "verification",
            "provider": "verification_cascade",
            "status": "error",
            "detail": type(exc).__name__,
            "source_license": "unknown",
        })
    else:
        attempts.extend(_verification_attempts(verdict))

    status = _public_contact_status(verdict)
    actionable_email = None if status == "invalid" else winner_email
    return {
        **base,
        "email": actionable_email,
        "contactability": {
            "email": actionable_email,
            "status": status,
            "finder_provider": winner_provider,
            "verifier_provider": verdict.source or "verification_cascade",
            "discovery_confidence": round(float(winner.get("confidence") or 0.0), 4),
            "verification_status": verdict.status or "unknown",
            "verification_confidence": round(
                float(verdict.verification_confidence or 0.0), 4
            ),
            "observed_at": observed_at,
            "attempts": attempts,
            "exhausted": False,
            "is_role": bool(verdict.is_role),
            "is_free": bool(verdict.is_free),
            "is_disposable": bool(verdict.is_disposable),
        },
    }


async def enrich_people_contacts(
    company: str,
    function: str,
    people: Iterable[dict[str, Any]],
    *,
    company_resolution: dict[str, Any] | None = None,
    person_ids: Iterable[str] = (),
    workspace_id: str = "",
    action_id: str = "",
    provider_timeout: float = 20.0,
) -> dict[str, Any]:
    """Discover and verify work emails for an exact saved people selection."""
    company = re.sub(r"\s+", " ", str(company or "").strip())[:120]
    function = re.sub(r"\s+", " ", str(function or "").strip())[:100]
    resolution = dict(company_resolution or {})
    canonical_domain = normalize_domain(
        str(resolution.get("canonical_domain") or "").strip()
    )
    candidates: list[dict[str, Any]] = []
    by_id: dict[str, dict[str, Any]] = {}
    for raw in people:
        if not isinstance(raw, dict):
            continue
        person = dict(raw)
        person_id = person_entity_id(company, person)
        person["person_id"] = person_id
        if not canonical_domain:
            canonical_domain = normalize_domain(
                str(person.get("canonical_company_domain") or "").strip()
            )
        if person_id not in by_id:
            candidates.append(person)
            by_id[person_id] = person

    requested_ids = list(dict.fromkeys(
        str(person_id).strip()
        for person_id in person_ids
        if str(person_id).strip()
    ))
    unknown_ids = [person_id for person_id in requested_ids if person_id not in by_id]
    if unknown_ids:
        return {
            "ok": False,
            "error": "Unknown people selection",
            "unknown_person_ids": unknown_ids,
        }
    selected = [by_id[person_id] for person_id in requested_ids] if requested_ids else candidates
    selected_ids = [person["person_id"] for person in selected]
    if not selected:
        return {"ok": False, "error": "No people selected", "people": [], "count": 0}

    semaphore = asyncio.Semaphore(3)

    async def _bounded(person: dict[str, Any]) -> dict[str, Any]:
        async with semaphore:
            return await _enrich_one_person(
                person,
                company=company,
                canonical_domain=canonical_domain,
                workspace_id=workspace_id,
                provider_timeout=max(1.0, min(float(provider_timeout), 45.0)),
            )

    enriched = await asyncio.gather(*[_bounded(person) for person in selected])
    statuses = ("verified", "risky", "catch_all", "invalid", "unavailable")
    summary = {
        status: sum(
            1 for person in enriched
            if (person.get("contactability") or {}).get("status") == status
        )
        for status in statuses
    }
    result_set_id = people_result_set_id(company, function, candidates)
    if not action_id:
        digest = hashlib.sha256("|".join(sorted(selected_ids)).encode()).hexdigest()[:16]
        action_id = f"people-contacts:{result_set_id}:{digest}"
    return {
        "ok": True,
        "action_id": action_id,
        "reused": False,
        "workspace_id": workspace_id,
        "company": company,
        "function": function,
        "company_resolution": resolution,
        "canonical_company_domain": canonical_domain,
        "result_set_id": result_set_id,
        "selected_person_ids": selected_ids,
        "provider_order": list(EXACT_EMAIL_PROVIDERS),
        "people": enriched,
        "count": len(enriched),
        "summary": summary,
        "observed_at": _now_iso(),
        "methodology": (
            "Exact-person email discovery followed by an independent deliverability check. "
            "Finder claims alone never receive verified status."
        ),
    }
