"""Fail-closed maturity registry for external activation integrations."""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import re
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Mapping
from urllib.parse import urlsplit

CERTIFICATION_PATH_ENV = "OPENGTM_INTEGRATION_CERTIFICATIONS"
CERTIFICATION_KEY_ENV = "OPENGTM_INTEGRATION_CERTIFICATION_KEY"

INTEGRATIONS: dict[str, dict[str, Any]] = {
    "webhook": {"category": "activation", "capabilities": ["outbound"]},
    "hubspot": {"category": "crm", "capabilities": ["outbound", "inbound"]},
    "salesforce": {"category": "crm", "capabilities": ["outbound", "inbound"]},
    "warehouse_http": {"category": "warehouse", "capabilities": ["outbound"]},
    "meta_ads": {"category": "ads", "capabilities": ["audience_sync"]},
    "google_ads": {"category": "ads", "capabilities": ["audience_sync"]},
    "linkedin_ads": {"category": "ads", "capabilities": ["audience_sync"]},
    "instantly": {"category": "sequencer", "capabilities": ["campaign_enroll"]},
    "smartlead": {"category": "sequencer", "capabilities": ["campaign_enroll"]},
    "google_sheets": {"category": "warehouse", "capabilities": ["outbound"]},
    "airtable": {"category": "warehouse", "capabilities": ["outbound"]},
}

SIGNAL_SOURCES: dict[str, dict[str, Any]] = {
    "jobspy": {"signal_types": ["hiring", "partnership_hiring"]},
    "sec_edgar": {"signal_types": ["funding", "leadership_change"]},
    "website_monitor": {"signal_types": ["website_change", "pricing_page_change"]},
    "tech_stack": {"signal_types": ["tech_change", "new_tech_adopted"]},
    "news_search": {"signal_types": ["news", "funding"]},
}

AGENT_CAPABILITIES: dict[str, dict[str, Any]] = {
    "grounded_research": {"features": ["citations", "budget_caps", "provenance"]},
    "chained_playbooks": {"features": ["prior_step_context", "versioned_prompts"]},
    "audience_runs": {"features": ["bounded_profiles", "durable_results"]},
    "recurring_schedules": {"features": ["single_flight", "restart_safe"]},
}


def _subject_id(certificate: Mapping[str, Any]) -> str:
    return str(certificate.get("subject_id") or certificate.get("integration_id") or "")


def _known_subject(subject_id: str) -> bool:
    return subject_id in {*INTEGRATIONS, *SIGNAL_SOURCES, *AGENT_CAPABILITIES} or bool(
        re.fullmatch(r"connector:[a-z][a-z0-9_-]{0,79}", subject_id)
    )


def _payload(certificate: Mapping[str, Any]) -> bytes:
    unsigned = {key: value for key, value in certificate.items() if key != "attestation"}
    return json.dumps(
        unsigned, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")


def attest_certificate(certificate: Mapping[str, Any], key: str) -> dict[str, Any]:
    if not key:
        raise ValueError("certification key must not be empty")
    result = dict(certificate)
    key_bytes = key.encode("utf-8")
    result["attestation"] = {
        "algorithm": "hmac-sha256",
        "key_id": hashlib.sha256(key_bytes).hexdigest()[:16],
        "signature": hmac.new(key_bytes, _payload(result), hashlib.sha256).hexdigest(),
    }
    return result


def _parse_time(value: Any) -> datetime | None:
    try:
        parsed = datetime.fromisoformat(str(value).replace("Z", "+00:00"))
        return parsed.astimezone(timezone.utc)
    except (TypeError, ValueError):
        return None


def _valid(certificate: Mapping[str, Any], key: str, now: datetime) -> bool:
    attestation = certificate.get("attestation")
    if not key or not isinstance(attestation, dict):
        return False
    key_bytes = key.encode("utf-8")
    expected_id = hashlib.sha256(key_bytes).hexdigest()[:16]
    expected = hmac.new(key_bytes, _payload(certificate), hashlib.sha256).hexdigest()
    validated_at = _parse_time(certificate.get("validated_at"))
    expires_at = _parse_time(certificate.get("expires_at"))
    evidence = urlsplit(str(certificate.get("evidence_url") or ""))
    return bool(
        _known_subject(_subject_id(certificate))
        and certificate.get("status") == "supported"
        and certificate.get("build_sha")
        and certificate.get("validation_run_id")
        and validated_at
        and expires_at
        and validated_at <= now < expires_at
        and evidence.scheme == "https"
        and evidence.netloc
        and attestation.get("algorithm") == "hmac-sha256"
        and hmac.compare_digest(str(attestation.get("key_id") or ""), expected_id)
        and hmac.compare_digest(str(attestation.get("signature") or ""), expected)
    )


def integration_catalog(
    *,
    path: str | Path | None = None,
    key: str | None = None,
    now: datetime | None = None,
) -> list[dict[str, Any]]:
    """Return maturity metadata; missing, invalid, or expired evidence stays beta."""
    path = path or os.getenv(CERTIFICATION_PATH_ENV, "")
    key = key if key is not None else os.getenv(CERTIFICATION_KEY_ENV, "")
    now = now or datetime.now(timezone.utc)
    certificates: list[Any] = []
    if path:
        try:
            loaded = json.loads(Path(path).read_text(encoding="utf-8"))
            certificates = loaded if isinstance(loaded, list) else []
        except (OSError, json.JSONDecodeError):
            certificates = []
    valid = {
        _subject_id(item): item
        for item in certificates
        if isinstance(item, dict) and _valid(item, key or "", now)
    }
    return [
        {
            "id": integration_id,
            **definition,
            "maturity": "supported" if integration_id in valid else "beta",
            "certification": {
                field: valid[integration_id][field]
                for field in (
                    "validated_at", "expires_at", "build_sha",
                    "validation_run_id", "evidence_url",
                )
            }
            if integration_id in valid
            else None,
        }
        for integration_id, definition in INTEGRATIONS.items()
    ]


def signal_source_catalog(
    *,
    path: str | Path | None = None,
    key: str | None = None,
    now: datetime | None = None,
) -> list[dict[str, Any]]:
    """Return fail-closed maturity metadata for first-party signal sources."""
    path = path or os.getenv(CERTIFICATION_PATH_ENV, "")
    key = key if key is not None else os.getenv(CERTIFICATION_KEY_ENV, "")
    now = now or datetime.now(timezone.utc)
    certificates: list[Any] = []
    if path:
        try:
            loaded = json.loads(Path(path).read_text(encoding="utf-8"))
            certificates = loaded if isinstance(loaded, list) else []
        except (OSError, json.JSONDecodeError):
            certificates = []
    valid = {
        _subject_id(item): item
        for item in certificates
        if isinstance(item, dict) and _valid(item, key or "", now)
    }
    return [
        {
            "id": source_id,
            **definition,
            "maturity": "supported" if source_id in valid else "beta",
            "certification": {
                field: valid[source_id][field]
                for field in (
                    "validated_at", "expires_at", "build_sha",
                    "validation_run_id", "evidence_url",
                )
            }
            if source_id in valid
            else None,
        }
        for source_id, definition in SIGNAL_SOURCES.items()
    ]


def agent_capability_catalog(
    *,
    path: str | Path | None = None,
    key: str | None = None,
    now: datetime | None = None,
) -> list[dict[str, Any]]:
    """Return signed controlled-live maturity for agent workflow capabilities."""
    path = path or os.getenv(CERTIFICATION_PATH_ENV, "")
    key = key if key is not None else os.getenv(CERTIFICATION_KEY_ENV, "")
    now = now or datetime.now(timezone.utc)
    certificates: list[Any] = []
    if path:
        try:
            loaded = json.loads(Path(path).read_text(encoding="utf-8"))
            certificates = loaded if isinstance(loaded, list) else []
        except (OSError, json.JSONDecodeError):
            certificates = []
    valid = {
        _subject_id(item): item
        for item in certificates
        if isinstance(item, dict) and _valid(item, key or "", now)
    }
    return [
        {
            "id": capability_id,
            **definition,
            "maturity": "supported" if capability_id in valid else "beta",
            "certification": {
                field: valid[capability_id][field]
                for field in (
                    "validated_at", "expires_at", "build_sha",
                    "validation_run_id", "evidence_url",
                )
            }
            if capability_id in valid
            else None,
        }
        for capability_id, definition in AGENT_CAPABILITIES.items()
    ]


def certification_statuses(
    subject_ids: list[str],
    *,
    path: str | Path | None = None,
    key: str | None = None,
    now: datetime | None = None,
) -> dict[str, dict[str, Any]]:
    """Resolve maturity for a bounded runtime catalog such as installed connectors."""
    path = path or os.getenv(CERTIFICATION_PATH_ENV, "")
    key = key if key is not None else os.getenv(CERTIFICATION_KEY_ENV, "")
    now = now or datetime.now(timezone.utc)
    requested = set(subject_ids)
    certificates: list[Any] = []
    if path:
        try:
            loaded = json.loads(Path(path).read_text(encoding="utf-8"))
            certificates = loaded if isinstance(loaded, list) else []
        except (OSError, json.JSONDecodeError):
            certificates = []
    valid = {
        _subject_id(item): item
        for item in certificates
        if isinstance(item, dict)
        and _subject_id(item) in requested
        and _valid(item, key or "", now)
    }
    return {
        subject_id: {
            "maturity": "supported" if subject_id in valid else "beta",
            "certification": {
                field: valid[subject_id][field]
                for field in (
                    "validated_at", "expires_at", "build_sha",
                    "validation_run_id", "evidence_url",
                )
            }
            if subject_id in valid
            else None,
        }
        for subject_id in subject_ids
    }
