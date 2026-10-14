"""POST /api/leads/domain-intel analyzes a clean, lowercase hostname."""

from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from apps.api.routers import leads
from apps.api.services.leadgen.enrichment import domain_intel
from apps.api.services.leadgen.enrichment.domain_intel import clean_domain


@pytest.fixture
def analyzed(monkeypatch):
    seen = []

    async def fake_analyze(domain):
        seen.append(domain)
        return {"domain": domain}

    monkeypatch.setattr(domain_intel, "analyze_domain", fake_analyze)
    return seen


@pytest.fixture
def client():
    ctx = SimpleNamespace(workspace_id="ws-fixture")
    app = FastAPI()
    app.include_router(leads.router)
    app.dependency_overrides[leads.require_editor] = lambda: ctx
    with TestClient(app) as http:
        yield http


@pytest.mark.parametrize("raw,expected", [
    ("example.com", "example.com"),
    ("Example.COM", "example.com"),
    ("  www.Example.com. ", "example.com"),
    ("https://WWW.Example.com:8443/pricing?x=1#top", "example.com"),
    ("example.com/careers", "example.com"),
    ("user:pw@example.com", "example.com"),
    # Only a leading www. goes; a www. inside the name is part of it.
    ("awww.example.com", "awww.example.com"),
    ("blog.www.example.com", "blog.www.example.com"),
    ("wwwexample.com", "wwwexample.com"),
    ("bücher.de", "xn--bcher-kva.de"),
])
def test_clean_domain_keeps_only_the_hostname(raw, expected):
    assert clean_domain(raw) == expected


@pytest.mark.parametrize("raw", [
    "", "   ", "localhost", "exa mple.com", "example.com&exclude=none",
    "example..com", "-example.com", "https://", 123, None, ["example.com"],
])
def test_clean_domain_rejects_what_is_not_a_hostname(raw):
    assert clean_domain(raw) == ""


def test_route_analyzes_the_cleaned_domain(client, analyzed):
    response = client.post("/api/leads/domain-intel", json={"domain": "https://WWW.Example.com/about"})

    assert response.status_code == 200, response.text
    assert analyzed == ["example.com"]


def test_route_keeps_a_www_inside_the_name(client, analyzed):
    assert client.post("/api/leads/domain-intel", json={"domain": "awww.example.com"}).status_code == 200
    assert analyzed == ["awww.example.com"]


@pytest.mark.parametrize("body", [
    {}, {"domain": ""}, {"domain": 42}, {"domain": ["a.com"]},
    {"domain": "example.com&exclude=none"}, {"domain": "not a domain"},
])
def test_route_rejects_input_that_is_not_a_domain(client, analyzed, body):
    response = client.post("/api/leads/domain-intel", json=body)

    assert response.status_code == 400, response.text
    assert analyzed == []
