"""Fail-closed maturity registry for external activation integrations."""

from __future__ import annotations

import hashlib
import hmac
import json
import os
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


def _subject_id(certificate: Mapping[str, Any]) -> str:
    return str(certificate.get("subject_id") or certificate.get("integration_id") or "")


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
        _subject_id(certificate) in {*INTEGRATIONS, *SIGNAL_SOURCES}
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
