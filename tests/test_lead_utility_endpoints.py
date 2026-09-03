"""REST utilities that back the n8n node: verify-email, score, tech-stack.

They reuse the same services the MCP tools call, so these tests only check the
HTTP contract (auth-scoped workspace, validation, error mapping).
"""
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient

from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.main import app


@pytest.fixture
def client():
    ctx = WorkspaceCtx(user=SimpleNamespace(id=1, username="t"), workspace_id="ws-test", slug="main")  # type: ignore[arg-type]
    app.dependency_overrides[current_workspace] = lambda: ctx
    try:
        yield TestClient(app)
    finally:
        app.dependency_overrides.pop(current_workspace, None)


def test_verify_email_uses_shared_cascade_with_authenticated_workspace(client, monkeypatch):
    from apps.api.services.leadgen.enrichment import email_verify_cascade as cascade

    seen = {}

    async def fake_verify(email, workspace_id=None):
        seen["email"], seen["workspace_id"] = email, workspace_id
        return SimpleNamespace(status=cascade.VALID, deliverable=True, confidence=0.95, source="smtp", detail="ok")

    monkeypatch.setattr(cascade, "verify_email", fake_verify)
    r = client.post("/api/leads/verify-email", json={"email": " jane@acme.com "})
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["valid"] is True and body["status"] == "valid"
    assert seen == {"email": "jane@acme.com", "workspace_id": "ws-test"}


def test_verify_email_rejects_garbage(client):
    assert client.post("/api/leads/verify-email", json={"email": "not an email"}).status_code == 400


def test_score_inline_lead_returns_score_and_tier(client):
    r = client.post("/api/leads/score", json={"lead": {"company": "Acme", "website": "https://acme.com", "email": "jane@acme.com"}})
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["company"] == "Acme"
    assert 0 <= body["score"] <= 100
    assert isinstance(body["tier"], str) and body["tier"]


def test_score_requires_lead_or_lead_id(client):
    assert client.post("/api/leads/score", json={}).status_code == 400


def test_tech_stack_is_409_until_fetches_are_enabled(client, monkeypatch):
    from apps.api.services.leadgen.enrichment.providers import tech_stack_provider as tsp

    monkeypatch.setattr(tsp.settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", False, raising=False)
    r = client.post("/api/leads/tech-stack", json={"domain": "acme.com"})
    assert r.status_code == 409, r.text
    assert client.post("/api/leads/tech-stack", json={"domain": "   "}).status_code == 400
