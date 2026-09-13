"""
Phase 1.5: YALC-style declarative provider manifests + capability registry.
See docs/research/clay-alternatives-ingestion-catalog.md §G.
"""
import asyncio
from pathlib import Path
import pytest

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.enrichment.declarative.template import (
    render_string, render_template, project_response, project_value,
)
from apps.api.services.leadgen.enrichment.declarative.manifest import (
    ProviderManifest, load_all_manifests, validate_manifest_directory,
)
from apps.api.services.leadgen.enrichment.declarative.signing import install_bundle, package_manifest, sign_manifest, verify_manifest
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
import base64
import json
import zipfile
from apps.api.services.leadgen.enrichment.declarative.compiler import (
    DeclarativeProvider, compile_manifest,
)


# ── template engine ──────────────────────────────────────────────

def _env(v):
    return {"HUNTER_API_KEY": "secret123"}.get(v, "")

def test_render_input_and_default():
    ctx = {"input": {"first_name": "Elon", "domain": ""}}
    assert render_string("{{input.first_name}}", ctx, _env) == "Elon"
    assert render_string("{{input.domain | default: x.com}}", ctx, _env) == "x.com"

def test_render_env():
    assert render_string("Bearer ${env:HUNTER_API_KEY}", {}, _env) == "Bearer secret123"
    assert render_string("${env:NOPE}", {}, _env) == ""

def test_render_template_nested():
    ctx = {"input": {"company": "SpaceX"}}
    body = {"q": "{{input.company}}", "n": 5, "tags": ["{{input.company}}"]}
    out = render_template(body, ctx, _env)
    assert out == {"q": "SpaceX", "n": 5, "tags": ["SpaceX"]}

def test_project_response_paths():
    data = {"data": {"work_email": "a@x.com"}, "results": [{"email": "b@x.com"}], "handle": "spacex"}
    assert project_value(data, "$.data.work_email") == "a@x.com"
    assert project_value(data, "$.results[].email") == "b@x.com"
    assert project_value(data, "$.results[0].email") == "b@x.com"
    assert project_value(data, "https://linkedin.com/company/$.handle") == "https://linkedin.com/company/spacex"
    assert project_value(data, "$.missing.key") is None

def test_project_response_mappings():
    data = {"person": {"email": "a@x.com", "li": "elonmusk"}}
    out = project_response(data, {"email": "$.person.email", "linkedin_url": "https://linkedin.com/in/$.person.li"})
    assert out == {"email": "a@x.com", "linkedin_url": "https://linkedin.com/in/elonmusk"}


# ── compiled provider ─────────────────────────────────────────────

_MANIFEST = ProviderManifest(
    name="test_email", capability="email", default_confidence=0.8,
    auth={"type": "header", "param": "X-Api-Key", "env_var": "TEST_KEY"},
    request={"method": "POST", "url": "https://api.test/find",
             "body": {"first": "{{input.first_name}}", "domain": "{{input.domain}}"}},
    response={"mappings": {"email": "$.email"}},
)

def test_provider_is_unavailable_without_key():
    p = compile_manifest(_MANIFEST, env_resolver=lambda v: "")
    assert p.name == "test_email"
    assert p.capabilities == ["email"]
    assert p.is_available() is False  # missing TEST_KEY

def test_provider_available_with_key():
    p = compile_manifest(_MANIFEST, env_resolver=lambda v: "k" if v == "TEST_KEY" else "")
    assert p.is_available() is True

def test_provider_missing_key_returns_failure_not_crash():
    p = compile_manifest(_MANIFEST, env_resolver=lambda v: "")
    res = asyncio.run(p.enrich(Lead(company="SpaceX", website="spacex.com")))
    assert res.success is False and "TEST_KEY" in (res.error or "")


# ── manifest loading + bundled manifest ───────────────────────────

def test_bundled_manifests_load():
    manifests = load_all_manifests()
    names = {m.name for m in manifests}
    assert "leadmagic_email" in names, names
    lm = next(m for m in manifests if m.name == "leadmagic_email")
    assert lm.capability == "email"
    assert lm.auth.env_var == "LEADMAGIC_API_KEY"
    # compiles + is inert (no key in test env)
    p = compile_manifest(lm)
    assert "email" in p.capabilities


def test_bundled_connector_catalog_is_compatible():
    report = validate_manifest_directory()
    assert report["ok"] is True, report["errors"]
    assert report["manifest_version"] == "1"
    assert report["count"] >= 4
    assert all(item["name"] and item["author"] for item in report["connectors"])


def test_connector_validation_fails_loudly_for_duplicates_and_http(tmp_path):
    body = """manifest_version: \"1\"\nname: duplicate_id\ncapability: email\nrequest:\n  method: POST\n  url: http://unsafe.example/find\nresponse:\n  mappings:\n    email: $.email\n"""
    (tmp_path / "one.yaml").write_text(body, encoding="utf-8")
    (tmp_path / "two.yaml").write_text(body.replace("http://", "https://"), encoding="utf-8")
    report = validate_manifest_directory(tmp_path)
    assert report["ok"] is False
    assert any("HTTPS" in item["error"] for item in report["errors"])
    assert any("duplicate provider id" in item["error"] for item in report["errors"])


def test_connector_ed25519_signature_trust_and_tamper_detection(tmp_path):
    manifest = tmp_path / "signed.yaml"
    manifest.write_text('manifest_version: "1"\nname: signed_email\ncapability: email\nrequest:\n  url: https://api.example.com/find\nresponse:\n  mappings:\n    email: $.email\n', encoding="utf-8")
    private_key = Ed25519PrivateKey.generate()
    private_path = tmp_path / "publisher.pem"
    private_path.write_bytes(private_key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
    public_bytes = private_key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    trust_store = tmp_path / "trusted.json"
    trust_store.write_text(json.dumps({"version": 1, "keys": [{"key_id": "publisher-1", "publisher": "Test Publisher", "public_key": base64.b64encode(public_bytes).decode()}]}), encoding="utf-8")

    sign_manifest(manifest, private_path, "publisher-1")
    verified = verify_manifest(manifest, trust_store)
    assert verified == {"status": "trusted", "key_id": "publisher-1", "publisher": "Test Publisher"}
    report = validate_manifest_directory(tmp_path, signature_policy="required", trust_store=trust_store)
    assert report["ok"] and report["connectors"][0]["signature"]["status"] == "trusted"

    first_bundle = package_manifest(manifest, tmp_path / "first.ogc")
    second_bundle = package_manifest(manifest, tmp_path / "second.ogc")
    assert first_bundle.read_bytes() == second_bundle.read_bytes()
    installed = install_bundle(first_bundle, tmp_path / "installed", trust_store)
    assert installed["id"] == "signed_email" and installed["signature"]["status"] == "trusted"
    assert Path(installed["manifest"]).exists()
    with pytest.raises(FileExistsError):
        install_bundle(first_bundle, tmp_path / "installed", trust_store)
    replaced = install_bundle(first_bundle, tmp_path / "installed", trust_store, replace=True)
    assert replaced["signature"]["status"] == "trusted"

    malformed = tmp_path / "malformed.ogc"
    with zipfile.ZipFile(malformed, "w") as archive:
        archive.writestr("../connector.yaml", manifest.read_bytes())
        archive.writestr("connector.yaml.sig", Path(f"{manifest}.sig").read_bytes())
    untouched = tmp_path / "untouched"
    with pytest.raises(ValueError, match="must contain only"):
        install_bundle(malformed, untouched, trust_store)
    assert not untouched.exists()

    manifest.write_text(manifest.read_text(encoding="utf-8").replace("$.email", "$.work_email"), encoding="utf-8")
    assert verify_manifest(manifest, trust_store)["status"] == "invalid"
    tampered_dir = tmp_path / "tampered"; tampered_dir.mkdir()
    tampered_manifest = tampered_dir / "signed.yaml"
    tampered_manifest.write_bytes(manifest.read_bytes())
    Path(f"{tampered_manifest}.sig").write_bytes(Path(f"{manifest}.sig").read_bytes())
    report = validate_manifest_directory(tampered_dir, signature_policy="optional", trust_store=trust_store)
    assert not report["ok"] and any("signature is invalid" in item["error"] for item in report["errors"])


def test_required_signature_policy_rejects_unsigned_manifest(tmp_path):
    (tmp_path / "unsigned.yaml").write_text('manifest_version: "1"\nname: unsigned_email\ncapability: email\nrequest:\n  url: https://api.example.com/find\nresponse:\n  mappings:\n    email: $.email\n', encoding="utf-8")
    report = validate_manifest_directory(tmp_path, signature_policy="required", trust_store=tmp_path / "missing.json")
    assert not report["ok"] and "signature is unsigned" in report["errors"][0]["error"]
