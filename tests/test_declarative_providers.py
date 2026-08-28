"""
Phase 1.5: YALC-style declarative provider manifests + capability registry.
See docs/research/clay-alternatives-ingestion-catalog.md §G.
"""
import asyncio

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.enrichment.declarative.template import (
    render_string, render_template, project_response, project_value,
)
from apps.api.services.leadgen.enrichment.declarative.manifest import (
    ProviderManifest, load_all_manifests,
)
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
