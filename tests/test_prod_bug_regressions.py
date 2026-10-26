"""Regression tests: declarative env injection, provider tag, trust_env."""
import asyncio
import re
from pathlib import Path

from apps.api.services.leadgen.enrichment.declarative.compiler import compile_manifest
from apps.api.services.leadgen.enrichment.declarative.manifest import ProviderManifest
from apps.api.services.leadgen.models import Lead


def _manifest(**over):
    base = dict(
        name="prov_x", capability="email",
        auth={"type": "header", "param": "X-Api-Key", "env_var": "PROV_KEY"},
        request={"method": "GET", "url": "https://api.test/find",
                 "headers": {"X-Leak": "${env:DATABASE_URL}"}},
        response={"mappings": {"email": "$.email"}},
    )
    base.update(over)
    return ProviderManifest(**base)


def test_template_cannot_read_undeclared_env(monkeypatch):
    monkeypatch.setenv("DATABASE_URL", "postgres://secret")
    seen = {}

    def resolver(var):
        seen[var] = True
        return {"PROV_KEY": "k", "DATABASE_URL": "postgres://secret"}.get(var, "")

    p = compile_manifest(_manifest(), env_resolver=resolver)
    assert p._env("DATABASE_URL") == ""
    assert p._env("PROV_KEY") == "k"
    assert "DATABASE_URL" not in seen


def test_declarative_results_tagged_with_provider():
    p = compile_manifest(_manifest(), env_resolver=lambda v: "")  # missing key path
    res = asyncio.run(p.enrich(Lead(company="Acme")))
    assert res.success is False
    assert res.provider == "prov_x"


def test_declarative_client_ignores_proxy_env(monkeypatch):
    import httpx
    seen = {}
    real = httpx.AsyncClient

    def spy(*a, **kw):
        seen.update(kw)
        return real(*a, **kw)

    monkeypatch.setattr(httpx, "AsyncClient", spy)
    monkeypatch.setattr(
        "apps.api.core.url_guard.check_url", lambda *a, **k: None, raising=False
    )
    p = compile_manifest(_manifest(request={"method": "GET", "url": "https://api.test/f"}),
                         env_resolver=lambda v: "k")
    asyncio.run(p.enrich(Lead(company="Acme")))
    assert seen.get("trust_env") is False


def test_provider_httpx_clients_disable_trust_env():
    root = Path(__file__).resolve().parents[1] / "apps/api/services/leadgen/enrichment"
    bad = []
    for f in root.rglob("*.py"):
        src = f.read_text()
        for m in re.finditer(r"httpx\.(?:Async)?Client\(", src):
            depth, i = 1, m.end()
            while depth and i < len(src):
                depth += {"(": 1, ")": -1}.get(src[i], 0)
                i += 1
            if "trust_env=False" not in src[m.end():i]:
                bad.append(f"{f.name}:{src[:m.start()].count(chr(10)) + 1}")
    assert not bad, bad
