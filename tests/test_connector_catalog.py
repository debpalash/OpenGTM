import json
from pathlib import Path
from types import SimpleNamespace

from apps.api.routers import connectors


def test_catalog_reports_workspace_readiness_without_secret_metadata(monkeypatch):
    seen = []
    def fake_secret(workspace_id, key):
        seen.append((workspace_id, key))
        return "configured-value" if key == "LEADMAGIC_API_KEY" else ""
    monkeypatch.setattr(connectors, "get_secret", fake_secret)
    result = connectors.connector_catalog(SimpleNamespace(workspace_id="ws-one"))
    assert result["total"] >= 4
    leadmagic = next(item for item in result["connectors"] if item["id"] == "leadmagic_email")
    prospeo = next(item for item in result["connectors"] if item["id"] == "prospeo_mobile")
    assert leadmagic["configured"] is True and prospeo["configured"] is False
    assert result["signature_policy"] == "optional"
    assert leadmagic["signature"]["status"] == "unsigned"
    assert len(leadmagic["package_sha256"]) == 64
    assert leadmagic["maturity"] == "beta" and leadmagic["certification"] is None
    assert "credential_key" not in str(result)
    assert all(workspace_id == "ws-one" for workspace_id, _ in seen)


def test_published_connector_schema_is_valid_json():
    path = Path("docs/connectors/connector-manifest.schema.json")
    schema = json.loads(path.read_text(encoding="utf-8"))
    assert schema["properties"]["manifest_version"]["const"] == "1"
    assert "request" in schema["required"]
