"""Array fan-out honors the full response path before choosing a value."""
import asyncio
import socket

import httpx
import pytest

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.enrichment.declarative.compiler import compile_manifest
from apps.api.services.leadgen.enrichment.declarative.manifest import ProviderManifest
from apps.api.services.leadgen.enrichment.declarative.template import project_value
from apps.api.services.workbook.http_column import execute_http_column


@pytest.fixture
def response_transport(monkeypatch):
    real_init = httpx.AsyncClient.__init__
    seen = []

    def install(payload):
        def handler(request):
            seen.append(request)
            return httpx.Response(200, json=payload)

        def init(self, *args, **kwargs):
            kwargs["transport"] = httpx.MockTransport(handler)
            real_init(self, *args, **kwargs)

        monkeypatch.setattr(httpx.AsyncClient, "__init__", init)
        monkeypatch.setattr(socket, "getaddrinfo", lambda *args, **kwargs: [
            (socket.AF_INET, None, None, "", ("93.184.216.34", 443)),
        ])
        return seen
    return install


@pytest.mark.parametrize("payload,path", [
    ({"results": [{}, {"email": "person@example.test"}]}, "$.results[].email"),
    ({"results": [{"email": None}, {"email": "person@example.test"}]}, "$.results[].email"),
    ({"results": [{"email": ""}, {"email": "person@example.test"}]}, "$.results[].email"),
    ({"groups": [{"results": []}, {"results": [{}, {"email": "person@example.test"}]}]},
     "$.groups[].results[].email"),
])
def test_compiled_provider_keeps_later_projected_result(response_transport, payload, path):
    requests = response_transport(payload)
    manifest = ProviderManifest(
        name="array_email", capability="email", auth={"type": "none"},
        request={"method": "GET", "url": "https://api.example.test/find"},
        response={"mappings": {"email": path}},
    )
    result = asyncio.run(compile_manifest(manifest).enrich(Lead(company="Example")))
    assert result.success, result.error
    assert result.fields == {"email": "person@example.test"}
    assert len(requests) == 1


def test_http_action_uses_later_projected_result(response_transport):
    requests = response_transport({"results": [{}, {"email": "person@example.test"}]})
    result = asyncio.run(execute_http_column({
        "type": "http", "http_url": "https://api.example.test/find",
        "http_extract": "$.results[].email",
    }, {}, []))
    assert result == {"success": True, "value": "person@example.test", "error": None}
    assert len(requests) == 1


@pytest.mark.parametrize("data,path,expected", [
    ({"results": [{"email": "first"}, {"email": "second"}]}, "$.results[].email", "first"),
    ({"results": [{}, {"email": "second"}]}, "$.results[0].email", None),
    ({"results": [{}, {"email": "second"}]}, "$.results[1].email", "second"),
    ({"results": [{"value": False}, {"value": True}]}, "$.results[].value", False),
    ({"results": [{"value": 0}, {"value": 1}]}, "$.results[].value", 0),
    ({"results": []}, "$.results[].email", None),
    ({"results": "invalid"}, "$.results[].email", None),
])
def test_array_projection_controls(data, path, expected):
    actual = project_value(data, path)
    assert actual == expected and type(actual) is type(expected)
