import json
import sys
from datetime import datetime, timezone

import pytest

from apps.api.services.scale_certification import attest_scale_report, scale_report_valid
from scripts import attest_scale_report as cli


KEY = "scale-test-key"
NOW = datetime(2026, 9, 13, tzinfo=timezone.utc)


def _workbook():
    return {
        "gate": "workbook_scale", "ok": True, "dialect": "postgresql",
        "run_id": "wb-1", "build_sha": "abc123",
        "finished_at": "2026-09-12T00:00:00Z", "rows": 1_000_000,
        "page_size": 100, "selection_exact": True, "search_matches": 1,
        "selected_positions": [0, 499_999, 999_999],
        "latency_ms": {"first_page": 10, "last_page": 20, "custom_sort_page": 30, "exact_selection": 5, "needle_search": 40},
        "threshold_ms": {"page": 2000, "selection": 2000, "search": 5000},
    }


def _queue():
    return {
        "gate": "queue_scale", "ok": True, "dialect": "postgresql",
        "run_tag": "queue-load:1", "build_sha": "abc123",
        "finished_at": "2026-09-12T00:00:00Z", "jobs": 10_000,
        "tenants": 100, "claimers": 32, "tenant_cap": 2, "completed": 10_000,
        "pending": 0, "duplicate_claims": 0, "cap_violations": {},
        "claimer_failures": [], "alive_claimers": 0,
        "duration_seconds": 10, "throughput_jobs_per_second": 1000,
        "claim_latency_ms": {"p50": 1, "p95": 3, "p99": 5},
        "max_active_by_tenant": {f"ws-{index}": 2 for index in range(100)},
    }


@pytest.mark.parametrize("report", [_workbook(), _queue()])
def test_scale_report_is_signed_current_and_build_bound(report):
    signed = attest_scale_report(report, KEY)
    assert scale_report_valid(signed, KEY, "abc123", now=NOW)
    assert not scale_report_valid(signed, KEY, "other", now=NOW)
    signed["ok"] = False
    assert not scale_report_valid(signed, KEY, "abc123", now=NOW)


@pytest.mark.parametrize("changes", [
    {"rows": 999_999}, {"dialect": "sqlite"}, {"selection_exact": False},
    {"search_matches": 0}, {"build_sha": ""}, {"page_size": 50},
    {"selected_positions": [1, 999_999]},
])
def test_workbook_scale_contract_fails_closed(changes):
    with pytest.raises(ValueError, match="controlled scale evidence contract"):
        attest_scale_report({**_workbook(), **changes}, KEY)


@pytest.mark.parametrize("changes", [
    {"jobs": 9_999}, {"tenants": 99}, {"claimers": 31},
    {"duplicate_claims": 1}, {"cap_violations": {"ws": 3}},
    {"tenant_cap": 3}, {"throughput_jobs_per_second": 0},
])
def test_queue_scale_contract_fails_closed(changes):
    with pytest.raises(ValueError, match="controlled scale evidence contract"):
        attest_scale_report({**_queue(), **changes}, KEY)


def test_scale_attestation_cli_writes_verified_report(tmp_path, monkeypatch):
    source = tmp_path / "scale.json"
    source.write_text(json.dumps(_workbook()), encoding="utf-8")
    output = tmp_path / "scale.attested.json"
    monkeypatch.setenv("OPENGTM_SCALE_ATTESTATION_KEY", KEY)
    monkeypatch.setattr(sys, "argv", [
        "attest-scale", "--input", str(source), "--output", str(output),
    ])

    assert cli.main() == 0
    signed = json.loads(output.read_text(encoding="utf-8"))
    assert scale_report_valid(signed, KEY, "abc123", now=NOW)
