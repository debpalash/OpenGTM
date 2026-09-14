import json
from datetime import datetime, timezone

import pytest

from apps.api.services.integrations.certification import (
    AGENT_CAPABILITIES,
    AUDIENCE_CAPABILITIES,
    GOVERNANCE_CAPABILITIES,
    INTEGRATIONS,
    SIGNAL_SOURCES,
    attest_certificate,
    agent_capability_catalog,
    audience_capability_catalog,
    certification_statuses,
    governance_capability_catalog,
    integration_catalog,
    required_checks_for_subject,
    signal_source_catalog,
)


NOW = datetime(2026, 9, 13, tzinfo=timezone.utc)
KEY = "test-only-integration-certification-key"


def _certificate(integration_id="hubspot"):
    checks = {
        name: {"passed": True, "evidence": f"artifact.json#/{name}"}
        for name in {
            "authentication", "external_write", "idempotency", "retry_recovery",
            "tenant_isolation", "inbound_reconciliation", "external_read",
            "provenance", "deduplication", "failure_recovery", "external_execution",
            "grounding", "budget_enforcement", "normalization",
            "external_authentication", "identity_binding", "jit_provisioning",
            "access_enforcement", "external_provisioning", "user_lifecycle",
            "group_sync", "token_revocation", "paged_directory",
            "consent_enforcement", "identifier_hashing", "add_reconciliation",
            "remove_reconciliation", "partial_failure_accounting",
            "streaming_upload", "manifest_checksum", "bounded_memory",
            "conflict_policy", "campaign_enrollment", "idempotent_upsert",
            "atomic_upsert", "notification_delivery",
            "employment_signal_normalization", "cik_resolution",
            "filing_cursor", "content_change_detection",
            "technology_fingerprint", "source_attribution",
            "citation_validation", "prior_step_context", "prompt_versioning",
            "bounded_traversal", "durable_results", "cooperative_cancellation",
            "in_place_retry", "completed_work_preservation", "single_flight",
            "restart_recovery",
            "quality_sample",
            "live_materialization", "observability", "serialized_refresh",
            "exact_membership_diff", "stable_traversal", "due_only_bootstrap",
            "entry_exit", "refresh_correlation", "automation_delivery",
            "bounded_delivery", "durable_ledger", "idempotent_retry",
        }
    }
    return {
        "integration_id": integration_id,
        "status": "supported",
        "validated_at": "2026-09-12T00:00:00Z",
        "expires_at": "2026-12-12T00:00:00Z",
        "build_sha": "abc123",
        "validation_run_id": "live-001",
        "evidence_url": "https://evidence.example/runs/live-001",
        "mode": "controlled_live",
        "external_system_id_hash": "a" * 64,
        "evidence_sha256": "b" * 64,
        "checks": checks,
    }


def _write(tmp_path, certificates):
    path = tmp_path / "certifications.json"
    path.write_text(json.dumps(certificates), encoding="utf-8")
    return path


def test_catalog_fails_closed_without_certifications():
    catalog = integration_catalog(path="missing.json", key=KEY, now=NOW)
    assert len(catalog) == len(INTEGRATIONS)
    assert all(item["maturity"] == "beta" for item in catalog)
    assert "inbound_reconciliation" in {
        item["id"]: item["required_checks"] for item in catalog
    }["hubspot"]


def test_required_checks_are_public_stable_and_subject_specific():
    assert required_checks_for_subject("meta_ads") == sorted(
        required_checks_for_subject("meta_ads")
    )
    assert "consent_enforcement" in required_checks_for_subject("meta_ads")
    assert "exact_membership_diff" in required_checks_for_subject(
        "dynamic_materialization"
    )
    assert "quality_sample" in required_checks_for_subject("provider:hunter_io")
    with pytest.raises(ValueError, match="unknown"):
        required_checks_for_subject("not-a-real-subject")


def test_valid_attestation_graduates_only_its_integration(tmp_path):
    certificate = attest_certificate(_certificate(), KEY)
    catalog = integration_catalog(path=_write(tmp_path, [certificate]), key=KEY, now=NOW, build_sha="abc123")
    states = {item["id"]: item for item in catalog}
    assert states["hubspot"]["maturity"] == "supported"
    assert states["hubspot"]["certification"]["validation_run_id"] == "live-001"
    assert states["hubspot"]["certification"]["mode"] == "controlled_live"
    assert states["hubspot"]["certification"]["evidence_sha256"] == "b" * 64
    assert states["salesforce"]["maturity"] == "beta"


def test_tampered_expired_and_wrong_key_certificates_fail_closed(tmp_path):
    signed = attest_certificate(_certificate(), KEY)
    tampered = {**signed, "build_sha": "changed"}
    assert integration_catalog(path=_write(tmp_path, [tampered]), key=KEY, now=NOW, build_sha="abc123")[1]["maturity"] == "beta"
    expired = attest_certificate({
        **_certificate(),
        "validated_at": "2026-08-01T00:00:00Z",
        "expires_at": "2026-09-01T00:00:00Z",
    }, KEY)
    assert integration_catalog(path=_write(tmp_path, [expired]), key=KEY, now=NOW, build_sha="abc123")[1]["maturity"] == "beta"
    assert integration_catalog(path=_write(tmp_path, [signed]), key="wrong", now=NOW, build_sha="abc123")[1]["maturity"] == "beta"


def test_certificate_requires_exact_running_build_identity(tmp_path):
    signed = attest_certificate(_certificate(), KEY)
    path = _write(tmp_path, [signed])
    assert integration_catalog(path=path, key=KEY, now=NOW)[1]["maturity"] == "beta"
    assert integration_catalog(path=path, key=KEY, now=NOW, build_sha="new-build")[1]["maturity"] == "beta"
    assert integration_catalog(path=path, key=KEY, now=NOW, build_sha="abc123")[1]["maturity"] == "supported"


def test_certificate_rejects_metadata_only_or_incomplete_live_claims(tmp_path):
    metadata_only = {
        key: value for key, value in _certificate().items()
        if key not in {"mode", "external_system_id_hash", "evidence_sha256", "checks"}
    }
    try:
        attest_certificate(metadata_only, KEY)
        assert False, "metadata-only certificate must not be signed"
    except ValueError as exc:
        assert "controlled-live evidence contract" in str(exc)

    incomplete = _certificate()
    incomplete["checks"] = {**incomplete["checks"]}
    incomplete["checks"].pop("idempotency")
    try:
        attest_certificate(incomplete, KEY)
        assert False, "missing required check must not be signed"
    except ValueError:
        pass

    signed = attest_certificate(_certificate(), KEY)
    signed["checks"]["external_write"]["passed"] = False
    catalog = integration_catalog(path=_write(tmp_path, [signed]), key=KEY, now=NOW, build_sha="abc123")
    assert {item["id"]: item["maturity"] for item in catalog}["hubspot"] == "beta"


@pytest.mark.parametrize("changes", [
    {"validation_run_id": "   "},
    {"build_sha": " "},
    {"validated_at": "not-a-time"},
    {"expires_at": "2026-09-11T00:00:00Z"},
    {"expires_at": "2027-09-12T00:00:00Z"},
    {"evidence_url": "http://evidence.example/run"},
    {"evidence_url": "https://user:secret@evidence.example/run"},
    {"status": "beta"},
])
def test_attestation_refuses_invalid_or_staleable_claims(changes):
    with pytest.raises(ValueError, match="controlled-live evidence contract"):
        attest_certificate({**_certificate(), **changes}, KEY)


def test_conflicting_aliases_and_duplicate_subjects_fail_closed(tmp_path):
    conflicting = {
        **_certificate("salesforce"),
        "subject_id": "hubspot",
    }
    with pytest.raises(ValueError, match="controlled-live evidence contract"):
        attest_certificate(conflicting, KEY)

    first = attest_certificate(_certificate("hubspot"), KEY)
    second = attest_certificate({
        **_certificate("hubspot"),
        "validation_run_id": "live-002",
        "external_system_id_hash": "c" * 64,
    }, KEY)
    for certificates in ([first, second], [second, first]):
        catalog = integration_catalog(
            path=_write(tmp_path, certificates), key=KEY, now=NOW,
            build_sha="abc123",
        )
        assert {item["id"]: item["maturity"] for item in catalog}["hubspot"] == "beta"


def test_signal_source_requires_its_own_attested_live_evidence(tmp_path):
    certificate = attest_certificate({
        **_certificate(),
        "subject_id": "jobspy",
        "integration_id": None,
        "validation_run_id": "signal-live-1",
    }, KEY)
    sources = signal_source_catalog(
        path=_write(tmp_path, [certificate]), key=KEY, now=NOW,
        build_sha="abc123",
    )
    states = {item["id"]: item for item in sources}
    assert len(states) == len(SIGNAL_SOURCES)
    assert states["jobspy"]["maturity"] == "supported"
    assert states["jobspy"]["certification"]["validation_run_id"] == "signal-live-1"
    assert states["sec_edgar"]["maturity"] == "beta"


@pytest.mark.parametrize("subject_id,required_check", [
    ("jobspy", "employment_signal_normalization"),
    ("sec_edgar", "filing_cursor"),
    ("website_monitor", "content_change_detection"),
    ("tech_stack", "technology_fingerprint"),
    ("news_search", "source_attribution"),
])
def test_signal_sources_require_source_specific_live_evidence(
    subject_id, required_check,
):
    incomplete = {
        **_certificate(),
        "subject_id": subject_id,
        "integration_id": None,
    }
    incomplete["checks"] = {**incomplete["checks"]}
    incomplete["checks"].pop(required_check)
    with pytest.raises(ValueError, match="controlled-live evidence contract"):
        attest_certificate(incomplete, KEY)

    complete = {**_certificate(), "subject_id": subject_id, "integration_id": None}
    assert attest_certificate(complete, KEY)["subject_id"] == subject_id


def test_agent_capability_certification_is_independent(tmp_path):
    certificate = attest_certificate({
        **_certificate(),
        "subject_id": "grounded_research",
        "integration_id": None,
        "validation_run_id": "agent-live-1",
    }, KEY)
    capabilities = agent_capability_catalog(
        path=_write(tmp_path, [certificate]), key=KEY, now=NOW,
        build_sha="abc123",
    )
    states = {item["id"]: item for item in capabilities}
    assert len(states) == len(AGENT_CAPABILITIES)
    assert states["grounded_research"]["maturity"] == "supported"
    assert states["chained_playbooks"]["maturity"] == "beta"


@pytest.mark.parametrize("subject_id,required_check", [
    ("grounded_research", "citation_validation"),
    ("chained_playbooks", "prior_step_context"),
    ("audience_runs", "bounded_traversal"),
    ("run_recovery", "completed_work_preservation"),
    ("recurring_schedules", "single_flight"),
])
def test_agent_subjects_require_capability_specific_live_evidence(
    subject_id, required_check,
):
    incomplete = {
        **_certificate(),
        "subject_id": subject_id,
        "integration_id": None,
    }
    incomplete["checks"] = {**incomplete["checks"]}
    incomplete["checks"].pop(required_check)
    with pytest.raises(ValueError, match="controlled-live evidence contract"):
        attest_certificate(incomplete, KEY)

    complete = {**_certificate(), "subject_id": subject_id, "integration_id": None}
    assert attest_certificate(complete, KEY)["subject_id"] == subject_id


def test_audience_capability_certification_is_independent(tmp_path):
    certificate = attest_certificate({
        **_certificate(),
        "subject_id": "dynamic_materialization",
        "integration_id": None,
        "validation_run_id": "audience-live-1",
    }, KEY)
    capabilities = audience_capability_catalog(
        path=_write(tmp_path, [certificate]), key=KEY, now=NOW,
        build_sha="abc123",
    )
    states = {item["id"]: item for item in capabilities}
    assert len(states) == len(AUDIENCE_CAPABILITIES)
    assert states["dynamic_materialization"]["maturity"] == "supported"
    assert states["scheduled_refresh"]["maturity"] == "beta"


@pytest.mark.parametrize("subject_id,required_check", [
    ("dynamic_materialization", "exact_membership_diff"),
    ("scheduled_refresh", "due_only_bootstrap"),
    ("membership_events", "refresh_correlation"),
    ("destination_runs", "durable_ledger"),
])
def test_audience_subjects_require_capability_specific_live_evidence(
    subject_id, required_check,
):
    incomplete = {
        **_certificate(),
        "subject_id": subject_id,
        "integration_id": None,
    }
    incomplete["checks"] = {**incomplete["checks"]}
    incomplete["checks"].pop(required_check)
    with pytest.raises(ValueError, match="controlled-live evidence contract"):
        attest_certificate(incomplete, KEY)

    assert attest_certificate({
        **_certificate(), "subject_id": subject_id, "integration_id": None,
    }, KEY)["subject_id"] == subject_id


def test_governance_capabilities_require_operation_specific_live_evidence(tmp_path):
    oidc = attest_certificate({
        **_certificate(),
        "subject_id": "oidc_sso",
        "integration_id": None,
        "validation_run_id": "oidc-live-1",
    }, KEY)
    capabilities = governance_capability_catalog(
        path=_write(tmp_path, [oidc]), key=KEY, now=NOW,
        build_sha="abc123",
    )
    states = {item["id"]: item for item in capabilities}
    assert len(states) == len(GOVERNANCE_CAPABILITIES)
    assert states["oidc_sso"]["maturity"] == "supported"
    assert states["scim_directory"]["maturity"] == "beta"

    incomplete_scim = _certificate()
    incomplete_scim.update(subject_id="scim_directory", integration_id=None)
    incomplete_scim["checks"] = {**incomplete_scim["checks"]}
    incomplete_scim["checks"].pop("group_sync")
    with pytest.raises(ValueError, match="controlled-live evidence contract"):
        attest_certificate(incomplete_scim, KEY)


@pytest.mark.parametrize("subject_id,required_check", [
    ("meta_ads", "consent_enforcement"),
    ("google_ads", "partial_failure_accounting"),
    ("linkedin_ads", "remove_reconciliation"),
    ("warehouse_http", "manifest_checksum"),
])
def test_ads_and_streaming_warehouse_require_specialized_evidence(
    subject_id, required_check,
):
    incomplete = _certificate(subject_id)
    incomplete["checks"] = {**incomplete["checks"]}
    incomplete["checks"].pop(required_check)
    with pytest.raises(ValueError, match="controlled-live evidence contract"):
        attest_certificate(incomplete, KEY)

    signed = attest_certificate(_certificate(subject_id), KEY)
    assert signed["integration_id"] == subject_id


@pytest.mark.parametrize("subject_id,required_check", [
    ("hubspot", "conflict_policy"),
    ("salesforce", "conflict_policy"),
    ("instantly", "campaign_enrollment"),
    ("smartlead", "campaign_enrollment"),
    ("google_sheets", "idempotent_upsert"),
    ("airtable", "atomic_upsert"),
    ("slack", "notification_delivery"),
])
def test_activation_subjects_require_promised_operation_evidence(
    subject_id, required_check,
):
    incomplete = _certificate(subject_id)
    incomplete["checks"] = {**incomplete["checks"]}
    incomplete["checks"].pop(required_check)
    with pytest.raises(ValueError, match="controlled-live evidence contract"):
        attest_certificate(incomplete, KEY)

    assert attest_certificate(_certificate(subject_id), KEY)["integration_id"] == subject_id


def test_installed_connector_maturity_requires_matching_subject(tmp_path):
    certificate = attest_certificate({
        **_certificate(),
        "subject_id": "connector:leadmagic_email",
        "integration_id": None,
        "validation_run_id": "connector-live-1",
        "subject_build_sha256": "d" * 64,
    }, KEY)
    statuses = certification_statuses(
        ["connector:leadmagic_email", "connector:prospeo_mobile"],
        path=_write(tmp_path, [certificate]), key=KEY, now=NOW,
        build_sha="abc123",
        subject_builds={
            "connector:leadmagic_email": "d" * 64,
            "connector:prospeo_mobile": "e" * 64,
        },
    )
    assert statuses["connector:leadmagic_email"]["maturity"] == "supported"
    assert statuses["connector:leadmagic_email"]["certification"]["subject_build_sha256"] == "d" * 64
    assert statuses["connector:prospeo_mobile"]["maturity"] == "beta"

    replaced = certification_statuses(
        ["connector:leadmagic_email"],
        path=_write(tmp_path, [certificate]), key=KEY, now=NOW,
        build_sha="abc123",
        subject_builds={"connector:leadmagic_email": "e" * 64},
    )
    assert replaced["connector:leadmagic_email"]["maturity"] == "beta"

    unbound = certification_statuses(
        ["connector:leadmagic_email"],
        path=_write(tmp_path, [certificate]), key=KEY, now=NOW,
        build_sha="abc123",
    )
    assert unbound["connector:leadmagic_email"]["maturity"] == "beta"


def test_enrichment_provider_maturity_is_independent_and_build_bound(tmp_path):
    certificate = attest_certificate({
        **_certificate(),
        "subject_id": "provider:hunter_io",
        "integration_id": None,
        "validation_run_id": "hunter-live-1",
    }, KEY)
    statuses = certification_statuses(
        ["provider:hunter_io", "provider:apollo_io"],
        path=_write(tmp_path, [certificate]), key=KEY, now=NOW,
        build_sha="abc123",
    )

    assert statuses["provider:hunter_io"]["maturity"] == "supported"
    assert "quality_sample" in statuses["provider:hunter_io"]["required_checks"]
    assert statuses["provider:hunter_io"]["certification"]["validation_run_id"] == "hunter-live-1"
    assert statuses["provider:apollo_io"]["maturity"] == "beta"
