"""Tamper-evident, build-bound evidence contracts for controlled scale gates."""

from __future__ import annotations

import hashlib
import hmac
import json
from datetime import datetime, timezone
from typing import Any, Mapping


SCALE_ATTESTATION_KEY_ENV = "OPENGTM_SCALE_ATTESTATION_KEY"
MAX_SCALE_EVIDENCE_AGE_DAYS = 92


def _payload(report: Mapping[str, Any]) -> bytes:
    unsigned = {key: value for key, value in report.items() if key != "attestation"}
    return json.dumps(
        unsigned, sort_keys=True, separators=(",", ":"), ensure_ascii=False,
    ).encode("utf-8")


def _time(value: object) -> datetime | None:
    try:
        return datetime.fromisoformat(str(value).replace("Z", "+00:00")).astimezone(timezone.utc)
    except (TypeError, ValueError):
        return None


def _workbook_contract(report: Mapping[str, Any]) -> bool:
    latency = report.get("latency_ms")
    threshold = report.get("threshold_ms")
    positions = report.get("selected_positions")
    return bool(
        report.get("gate") == "workbook_scale"
        and report.get("ok") is True
        and report.get("dialect") == "postgresql"
        and isinstance(report.get("rows"), int)
        and report["rows"] >= 1_000_000
        and report.get("page_size") == 100
        and report.get("selection_exact") is True
        and isinstance(positions, list)
        and len(positions) >= 2
        and positions[0] == 0
        and positions[-1] == report["rows"] - 1
        and report.get("search_matches") == 1
        and isinstance(latency, dict)
        and isinstance(threshold, dict)
        and all(isinstance(latency.get(name), (int, float)) for name in (
            "first_page", "last_page", "custom_sort_page",
            "exact_selection", "needle_search",
        ))
        and all(isinstance(threshold.get(name), (int, float)) for name in (
            "page", "selection", "search",
        ))
        and max(latency[name] for name in (
            "first_page", "last_page", "custom_sort_page",
        )) <= threshold.get("page", -1)
        and latency["exact_selection"] <= threshold.get("selection", -1)
        and latency["needle_search"] <= threshold.get("search", -1)
    )


def _queue_contract(report: Mapping[str, Any]) -> bool:
    maximum = report.get("max_active_by_tenant")
    claim_latency = report.get("claim_latency_ms")
    return bool(
        report.get("gate") == "queue_scale"
        and report.get("ok") is True
        and report.get("dialect") == "postgresql"
        and isinstance(report.get("jobs"), int) and report["jobs"] >= 10_000
        and isinstance(report.get("tenants"), int) and report["tenants"] >= 100
        and isinstance(report.get("claimers"), int) and report["claimers"] >= 32
        and report.get("tenant_cap") == 2
        and report.get("completed") == report.get("jobs")
        and report.get("pending") == 0
        and report.get("duplicate_claims") == 0
        and report.get("cap_violations") == {}
        and report.get("claimer_failures") == []
        and report.get("alive_claimers") == 0
        and isinstance(maximum, dict)
        and len(maximum) == report["tenants"]
        and all(isinstance(value, int) and 0 < value <= 2 for value in maximum.values())
        and isinstance(report.get("duration_seconds"), (int, float))
        and report["duration_seconds"] > 0
        and isinstance(report.get("throughput_jobs_per_second"), (int, float))
        and report["throughput_jobs_per_second"] > 0
        and isinstance(claim_latency, dict)
        and all(isinstance(claim_latency.get(name), (int, float)) and claim_latency[name] >= 0
                for name in ("p50", "p95", "p99"))
    )


def _contract_valid(report: Mapping[str, Any]) -> bool:
    return bool(
        str(report.get("build_sha") or "").strip()
        and not any(character.isspace() for character in str(report.get("build_sha")))
        and _time(report.get("finished_at")) is not None
        and bool(str(report.get("run_id") or report.get("run_tag") or "").strip())
        and (_workbook_contract(report) or _queue_contract(report))
    )


def attest_scale_report(report: Mapping[str, Any], key: str) -> dict[str, Any]:
    if not key:
        raise ValueError("scale attestation key must not be empty")
    if not _contract_valid(report):
        raise ValueError("report does not satisfy a controlled scale evidence contract")
    result = dict(report)
    key_bytes = key.encode("utf-8")
    result["attestation"] = {
        "algorithm": "hmac-sha256",
        "key_id": hashlib.sha256(key_bytes).hexdigest()[:16],
        "signature": hmac.new(key_bytes, _payload(result), hashlib.sha256).hexdigest(),
    }
    return result


def scale_report_valid(
    report: Mapping[str, Any], key: str, build_sha: str,
    *, now: datetime | None = None,
) -> bool:
    now = now or datetime.now(timezone.utc)
    finished_at = _time(report.get("finished_at"))
    attestation = report.get("attestation")
    if not isinstance(attestation, dict) or not key or finished_at is None:
        return False
    key_bytes = key.encode("utf-8")
    expected = hmac.new(key_bytes, _payload(report), hashlib.sha256).hexdigest()
    return bool(
        _contract_valid(report)
        and build_sha
        and hmac.compare_digest(str(report.get("build_sha") or ""), build_sha)
        and finished_at <= now
        and (now - finished_at).total_seconds() <= MAX_SCALE_EVIDENCE_AGE_DAYS * 86400
        and attestation.get("algorithm") == "hmac-sha256"
        and hmac.compare_digest(
            str(attestation.get("key_id") or ""),
            hashlib.sha256(key_bytes).hexdigest()[:16],
        )
        and hmac.compare_digest(str(attestation.get("signature") or ""), expected)
    )
