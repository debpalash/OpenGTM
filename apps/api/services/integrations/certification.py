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
BUILD_SHA_ENV = "OPENGTM_BUILD_SHA"
MAX_CERTIFICATION_LIFETIME_DAYS = 92

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
    "slack": {"category": "activation", "capabilities": ["notification"]},
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
    "run_recovery": {"features": ["cooperative_cancellation", "in_place_retry", "completed_work_preservation"]},
    "recurring_schedules": {"features": ["single_flight", "restart_safe"]},
}

GOVERNANCE_CAPABILITIES: dict[str, dict[str, Any]] = {
    "oidc_sso": {
        "features": ["issuer_validation", "identity_binding", "jit_provisioning", "access_enforcement"],
    },
    "scim_directory": {
        "features": ["user_lifecycle", "group_sync", "token_revocation", "paged_directory"],
    },
}

INTEGRATION_CHECKS = {"authentication", "external_write", "idempotency", "retry_recovery", "tenant_isolation"}
ADS_CHECKS = {
    "consent_enforcement", "identifier_hashing", "add_reconciliation",
    "remove_reconciliation", "partial_failure_accounting",
}
WAREHOUSE_STREAM_CHECKS = {
    "streaming_upload", "manifest_checksum", "bounded_memory",
}
SIGNAL_CHECKS = {"external_read", "provenance", "deduplication", "failure_recovery", "tenant_isolation"}
AGENT_CHECKS = {"external_execution", "grounding", "budget_enforcement", "failure_recovery", "tenant_isolation"}
CONNECTOR_CHECKS = {"authentication", "external_read", "normalization", "failure_recovery", "tenant_isolation"}
GOVERNANCE_CHECKS: dict[str, set[str]] = {
    "oidc_sso": {
        "external_authentication", "identity_binding", "jit_provisioning",
        "access_enforcement", "failure_recovery", "tenant_isolation",
    },
    "scim_directory": {
        "external_provisioning", "user_lifecycle", "group_sync",
        "token_revocation", "paged_directory", "tenant_isolation",
    },
}


def _subject_id(certificate: Mapping[str, Any]) -> str:
    return str(certificate.get("subject_id") or certificate.get("integration_id") or "")


def _known_subject(subject_id: str) -> bool:
    return subject_id in {
        *INTEGRATIONS, *SIGNAL_SOURCES, *AGENT_CAPABILITIES,
        *GOVERNANCE_CAPABILITIES,
    } or bool(
        re.fullmatch(r"connector:[a-z][a-z0-9_-]{0,79}", subject_id)
    )


def _payload(certificate: Mapping[str, Any]) -> bytes:
    unsigned = {key: value for key, value in certificate.items() if key != "attestation"}
    return json.dumps(
        unsigned, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")


def _required_checks(subject_id: str) -> set[str]:
    if subject_id in INTEGRATIONS:
        required = set(INTEGRATION_CHECKS)
        if "inbound" in INTEGRATIONS[subject_id].get("capabilities", []):
            required.add("inbound_reconciliation")
        if INTEGRATIONS[subject_id].get("category") == "ads":
            required.update(ADS_CHECKS)
        if subject_id == "warehouse_http":
            required.update(WAREHOUSE_STREAM_CHECKS)
        return required
    if subject_id in SIGNAL_SOURCES:
        return set(SIGNAL_CHECKS)
    if subject_id in AGENT_CAPABILITIES:
        return set(AGENT_CHECKS)
    if subject_id in GOVERNANCE_CAPABILITIES:
        return set(GOVERNANCE_CHECKS[subject_id])
    if subject_id.startswith("connector:"):
        return set(CONNECTOR_CHECKS)
    return set()


def _evidence_contract_valid(certificate: Mapping[str, Any]) -> bool:
    subject_id = _subject_id(certificate)
    legacy_id = str(certificate.get("integration_id") or "")
    explicit_id = str(certificate.get("subject_id") or "")
    if legacy_id and explicit_id and legacy_id != explicit_id:
        return False
    validation_run_id = str(certificate.get("validation_run_id") or "")
    build_sha = str(certificate.get("build_sha") or "")
    validated_at = _parse_time(certificate.get("validated_at"))
    expires_at = _parse_time(certificate.get("expires_at"))
    evidence = urlsplit(str(certificate.get("evidence_url") or ""))
    if (
        certificate.get("status") != "supported"
        or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}", validation_run_id)
        or not build_sha.strip()
        or any(character.isspace() for character in build_sha)
        or validated_at is None
        or expires_at is None
        or not validated_at < expires_at
        or (expires_at - validated_at).total_seconds()
        > MAX_CERTIFICATION_LIFETIME_DAYS * 86400
        or evidence.scheme != "https"
        or not evidence.netloc
        or evidence.username is not None
        or evidence.password is not None
    ):
        return False
    if subject_id.startswith("connector:") and not re.fullmatch(
        r"[0-9a-f]{64}", str(certificate.get("subject_build_sha256") or "")
    ):
        return False
    checks = certificate.get("checks")
    required = _required_checks(subject_id)
    if certificate.get("mode") != "controlled_live" or not required or not isinstance(checks, dict):
        return False
    if not re.fullmatch(r"[0-9a-f]{64}", str(certificate.get("external_system_id_hash") or "")):
        return False
    if not re.fullmatch(r"[0-9a-f]{64}", str(certificate.get("evidence_sha256") or "")):
        return False
    for name in required:
        check = checks.get(name)
        if not isinstance(check, dict) or check.get("passed") is not True:
            return False
        if not str(check.get("evidence") or "").strip():
            return False
    return True


def _valid_by_subject(
    certificates: list[Any], key: str, now: datetime, build_sha: str,
    *, requested: set[str] | None = None,
    subject_builds: Mapping[str, str] | None = None,
) -> dict[str, Mapping[str, Any]]:
    """Resolve only unambiguous valid certificates, independent of file order."""
    valid: dict[str, Mapping[str, Any]] = {}
    ambiguous: set[str] = set()
    for item in certificates:
        if not isinstance(item, dict):
            continue
        subject_id = _subject_id(item)
        if requested is not None and subject_id not in requested:
            continue
        if not _valid(item, key, now, build_sha):
            continue
        if subject_id.startswith("connector:"):
            expected_build = (subject_builds or {}).get(subject_id, "")
            if not expected_build or not hmac.compare_digest(
                str(item.get("subject_build_sha256") or ""), expected_build,
            ):
                continue
        if subject_id in valid or subject_id in ambiguous:
            valid.pop(subject_id, None)
            ambiguous.add(subject_id)
            continue
        valid[subject_id] = item
    return valid


def attest_certificate(certificate: Mapping[str, Any], key: str) -> dict[str, Any]:
    if not key:
        raise ValueError("certification key must not be empty")
    if not _known_subject(_subject_id(certificate)):
        raise ValueError("certificate subject is unknown")
    if not _evidence_contract_valid(certificate):
        raise ValueError("certificate does not satisfy the controlled-live evidence contract")
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


def _valid(certificate: Mapping[str, Any], key: str, now: datetime, build_sha: str) -> bool:
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
        and _evidence_contract_valid(certificate)
        and certificate.get("status") == "supported"
        and build_sha
        and hmac.compare_digest(str(certificate.get("build_sha") or ""), build_sha)
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
    build_sha: str | None = None,
) -> list[dict[str, Any]]:
    """Return maturity metadata; missing, invalid, or expired evidence stays beta."""
    path = path or os.getenv(CERTIFICATION_PATH_ENV, "")
    key = key if key is not None else os.getenv(CERTIFICATION_KEY_ENV, "")
    now = now or datetime.now(timezone.utc)
    build_sha = build_sha if build_sha is not None else os.getenv(BUILD_SHA_ENV, "")
    certificates: list[Any] = []
    if path:
        try:
            loaded = json.loads(Path(path).read_text(encoding="utf-8"))
            certificates = loaded if isinstance(loaded, list) else []
        except (OSError, json.JSONDecodeError):
            certificates = []
    valid = _valid_by_subject(certificates, key or "", now, build_sha)
    return [
        {
            "id": integration_id,
            **definition,
            "maturity": "supported" if integration_id in valid else "beta",
            "certification": {
                field: valid[integration_id][field]
                for field in (
                    "validated_at", "expires_at", "build_sha",
                    "validation_run_id", "evidence_url", "evidence_sha256", "mode",
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
    build_sha: str | None = None,
) -> list[dict[str, Any]]:
    """Return fail-closed maturity metadata for first-party signal sources."""
    path = path or os.getenv(CERTIFICATION_PATH_ENV, "")
    key = key if key is not None else os.getenv(CERTIFICATION_KEY_ENV, "")
    now = now or datetime.now(timezone.utc)
    build_sha = build_sha if build_sha is not None else os.getenv(BUILD_SHA_ENV, "")
    certificates: list[Any] = []
    if path:
        try:
            loaded = json.loads(Path(path).read_text(encoding="utf-8"))
            certificates = loaded if isinstance(loaded, list) else []
        except (OSError, json.JSONDecodeError):
            certificates = []
    valid = _valid_by_subject(certificates, key or "", now, build_sha)
    return [
        {
            "id": source_id,
            **definition,
            "maturity": "supported" if source_id in valid else "beta",
            "certification": {
                field: valid[source_id][field]
                for field in (
                    "validated_at", "expires_at", "build_sha",
                    "validation_run_id", "evidence_url", "evidence_sha256", "mode",
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
    build_sha: str | None = None,
) -> list[dict[str, Any]]:
    """Return signed controlled-live maturity for agent workflow capabilities."""
    path = path or os.getenv(CERTIFICATION_PATH_ENV, "")
    key = key if key is not None else os.getenv(CERTIFICATION_KEY_ENV, "")
    now = now or datetime.now(timezone.utc)
    build_sha = build_sha if build_sha is not None else os.getenv(BUILD_SHA_ENV, "")
    certificates: list[Any] = []
    if path:
        try:
            loaded = json.loads(Path(path).read_text(encoding="utf-8"))
            certificates = loaded if isinstance(loaded, list) else []
        except (OSError, json.JSONDecodeError):
            certificates = []
    valid = _valid_by_subject(certificates, key or "", now, build_sha)
    return [
        {
            "id": capability_id,
            **definition,
            "maturity": "supported" if capability_id in valid else "beta",
            "certification": {
                field: valid[capability_id][field]
                for field in (
                    "validated_at", "expires_at", "build_sha",
                    "validation_run_id", "evidence_url", "evidence_sha256", "mode",
                )
            }
            if capability_id in valid
            else None,
        }
        for capability_id, definition in AGENT_CAPABILITIES.items()
    ]


def governance_capability_catalog(
    *,
    path: str | Path | None = None,
    key: str | None = None,
    now: datetime | None = None,
    build_sha: str | None = None,
) -> list[dict[str, Any]]:
    """Return signed controlled-live maturity for enterprise identity capabilities."""
    path = path or os.getenv(CERTIFICATION_PATH_ENV, "")
    key = key if key is not None else os.getenv(CERTIFICATION_KEY_ENV, "")
    now = now or datetime.now(timezone.utc)
    build_sha = build_sha if build_sha is not None else os.getenv(BUILD_SHA_ENV, "")
    certificates: list[Any] = []
    if path:
        try:
            loaded = json.loads(Path(path).read_text(encoding="utf-8"))
            certificates = loaded if isinstance(loaded, list) else []
        except (OSError, json.JSONDecodeError):
            certificates = []
    valid = _valid_by_subject(certificates, key or "", now, build_sha)
    return [
        {
            "id": capability_id,
            **definition,
            "maturity": "supported" if capability_id in valid else "beta",
            "certification": {
                field: valid[capability_id][field]
                for field in (
                    "validated_at", "expires_at", "build_sha",
                    "validation_run_id", "evidence_url", "evidence_sha256", "mode",
                )
            }
            if capability_id in valid
            else None,
        }
        for capability_id, definition in GOVERNANCE_CAPABILITIES.items()
    ]


def certification_statuses(
    subject_ids: list[str],
    *,
    path: str | Path | None = None,
    key: str | None = None,
    now: datetime | None = None,
    build_sha: str | None = None,
    subject_builds: Mapping[str, str] | None = None,
) -> dict[str, dict[str, Any]]:
    """Resolve maturity for a bounded runtime catalog such as installed connectors."""
    path = path or os.getenv(CERTIFICATION_PATH_ENV, "")
    key = key if key is not None else os.getenv(CERTIFICATION_KEY_ENV, "")
    now = now or datetime.now(timezone.utc)
    build_sha = build_sha if build_sha is not None else os.getenv(BUILD_SHA_ENV, "")
    requested = set(subject_ids)
    certificates: list[Any] = []
    if path:
        try:
            loaded = json.loads(Path(path).read_text(encoding="utf-8"))
            certificates = loaded if isinstance(loaded, list) else []
        except (OSError, json.JSONDecodeError):
            certificates = []
    valid = _valid_by_subject(
        certificates, key or "", now, build_sha, requested=requested,
        subject_builds=subject_builds,
    )
    return {
        subject_id: {
            "maturity": "supported" if subject_id in valid else "beta",
            "certification": {
                field: valid[subject_id][field]
                for field in (
                    "validated_at", "expires_at", "build_sha",
                    "validation_run_id", "evidence_url", "evidence_sha256", "mode",
                    "subject_build_sha256",
                )
            }
            if subject_id in valid
            else None,
        }
        for subject_id in subject_ids
    }
