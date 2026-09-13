import json
from datetime import datetime, timezone

from apps.api.services.integrations.certification import (
    AGENT_CAPABILITIES,
    INTEGRATIONS,
    SIGNAL_SOURCES,
    attest_certificate,
    agent_capability_catalog,
    certification_statuses,
    integration_catalog,
    signal_source_catalog,
)


NOW = datetime(2026, 9, 13, tzinfo=timezone.utc)
KEY = "test-only-integration-certification-key"


def _certificate(integration_id="hubspot"):
    return {
        "integration_id": integration_id,
        "status": "supported",
        "validated_at": "2026-09-12T00:00:00Z",
        "expires_at": "2026-12-12T00:00:00Z",
        "build_sha": "abc123",
        "validation_run_id": "live-001",
        "evidence_url": "https://evidence.example/runs/live-001",
    }


def _write(tmp_path, certificates):
    path = tmp_path / "certifications.json"
    path.write_text(json.dumps(certificates), encoding="utf-8")
    return path


def test_catalog_fails_closed_without_certifications():
    catalog = integration_catalog(path="missing.json", key=KEY, now=NOW)
    assert len(catalog) == len(INTEGRATIONS)
    assert all(item["maturity"] == "beta" for item in catalog)


def test_valid_attestation_graduates_only_its_integration(tmp_path):
    certificate = attest_certificate(_certificate(), KEY)
    catalog = integration_catalog(path=_write(tmp_path, [certificate]), key=KEY, now=NOW)
    states = {item["id"]: item for item in catalog}
    assert states["hubspot"]["maturity"] == "supported"
    assert states["hubspot"]["certification"]["validation_run_id"] == "live-001"
    assert states["salesforce"]["maturity"] == "beta"


def test_tampered_expired_and_wrong_key_certificates_fail_closed(tmp_path):
    signed = attest_certificate(_certificate(), KEY)
    tampered = {**signed, "build_sha": "changed"}
    assert integration_catalog(path=_write(tmp_path, [tampered]), key=KEY, now=NOW)[1]["maturity"] == "beta"
    expired = attest_certificate({**_certificate(), "expires_at": "2026-09-01T00:00:00Z"}, KEY)
    assert integration_catalog(path=_write(tmp_path, [expired]), key=KEY, now=NOW)[1]["maturity"] == "beta"
    assert integration_catalog(path=_write(tmp_path, [signed]), key="wrong", now=NOW)[1]["maturity"] == "beta"


def test_signal_source_requires_its_own_attested_live_evidence(tmp_path):
    certificate = attest_certificate({
        **_certificate(),
        "subject_id": "jobspy",
        "integration_id": None,
        "validation_run_id": "signal-live-1",
    }, KEY)
    sources = signal_source_catalog(
        path=_write(tmp_path, [certificate]), key=KEY, now=NOW,
    )
    states = {item["id"]: item for item in sources}
    assert len(states) == len(SIGNAL_SOURCES)
    assert states["jobspy"]["maturity"] == "supported"
    assert states["jobspy"]["certification"]["validation_run_id"] == "signal-live-1"
    assert states["sec_edgar"]["maturity"] == "beta"


def test_agent_capability_certification_is_independent(tmp_path):
    certificate = attest_certificate({
        **_certificate(),
        "subject_id": "grounded_research",
        "integration_id": None,
        "validation_run_id": "agent-live-1",
    }, KEY)
    capabilities = agent_capability_catalog(
        path=_write(tmp_path, [certificate]), key=KEY, now=NOW,
    )
    states = {item["id"]: item for item in capabilities}
    assert len(states) == len(AGENT_CAPABILITIES)
    assert states["grounded_research"]["maturity"] == "supported"
    assert states["chained_playbooks"]["maturity"] == "beta"


def test_installed_connector_maturity_requires_matching_subject(tmp_path):
    certificate = attest_certificate({
        **_certificate(),
        "subject_id": "connector:leadmagic_email",
        "integration_id": None,
        "validation_run_id": "connector-live-1",
    }, KEY)
    statuses = certification_statuses(
        ["connector:leadmagic_email", "connector:prospeo_mobile"],
        path=_write(tmp_path, [certificate]), key=KEY, now=NOW,
    )
    assert statuses["connector:leadmagic_email"]["maturity"] == "supported"
    assert statuses["connector:prospeo_mobile"]["maturity"] == "beta"
