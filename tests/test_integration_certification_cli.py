import hashlib
import json
import sys

from scripts import attest_integration_certification as cli


KEY = "test-only-integration-certification-key"


def _certificate(evidence_sha256: str) -> dict:
    required = {
        "authentication", "external_write", "idempotency",
        "retry_recovery", "tenant_isolation", "inbound_reconciliation",
    }
    return {
        "subject_id": "hubspot",
        "status": "supported",
        "mode": "controlled_live",
        "validated_at": "2026-09-12T00:00:00Z",
        "expires_at": "2026-12-12T00:00:00Z",
        "build_sha": "abc123",
        "validation_run_id": "hubspot-live-1",
        "evidence_url": "https://evidence.example/hubspot-live-1.json",
        "evidence_sha256": evidence_sha256,
        "external_system_id_hash": "a" * 64,
        "checks": {
            name: {"passed": True, "evidence": f"artifact.json#/{name}"}
            for name in required
        },
    }


def test_cli_hashes_evidence_before_attesting(tmp_path, monkeypatch):
    evidence = tmp_path / "evidence.json"
    evidence.write_text(json.dumps({
        name: {"request_id": f"req-{name}"}
        for name in _certificate("b" * 64)["checks"]
    }), encoding="utf-8")
    certificate = tmp_path / "certificate.json"
    certificate.write_text(json.dumps(_certificate(
        hashlib.sha256(evidence.read_bytes()).hexdigest()
    )), encoding="utf-8")
    output = tmp_path / "attested.json"
    monkeypatch.setenv("OPENGTM_INTEGRATION_CERTIFICATION_KEY", KEY)
    monkeypatch.setattr(sys, "argv", [
        "attest", "--input", str(certificate), "--evidence", str(evidence),
        "--output", str(output),
    ])

    assert cli.main() == 0
    assert json.loads(output.read_text(encoding="utf-8"))["attestation"]["algorithm"] == "hmac-sha256"


def test_cli_refuses_evidence_digest_mismatch_without_output(tmp_path, monkeypatch):
    evidence = tmp_path / "evidence.json"
    evidence.write_bytes(b'{"live":false}\n')
    certificate = tmp_path / "certificate.json"
    certificate.write_text(json.dumps(_certificate("b" * 64)), encoding="utf-8")
    output = tmp_path / "attested.json"
    monkeypatch.setenv("OPENGTM_INTEGRATION_CERTIFICATION_KEY", KEY)
    monkeypatch.setattr(sys, "argv", [
        "attest", "--input", str(certificate), "--evidence", str(evidence),
        "--output", str(output),
    ])

    assert cli.main() == 2
    assert not output.exists()


def test_cli_refuses_unresolved_check_evidence(tmp_path, monkeypatch):
    evidence = tmp_path / "evidence.json"
    evidence.write_text(json.dumps({"authentication": {"request_id": "req-1"}}), encoding="utf-8")
    certificate = tmp_path / "certificate.json"
    certificate.write_text(json.dumps(_certificate(
        hashlib.sha256(evidence.read_bytes()).hexdigest()
    )), encoding="utf-8")
    output = tmp_path / "attested.json"
    monkeypatch.setenv("OPENGTM_INTEGRATION_CERTIFICATION_KEY", KEY)
    monkeypatch.setattr(sys, "argv", [
        "attest", "--input", str(certificate), "--evidence", str(evidence),
        "--output", str(output),
    ])

    assert cli.main() == 2
    assert not output.exists()
