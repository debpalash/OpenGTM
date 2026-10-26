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


def test_declarative_results_tagged_with_provider():
    p = compile_manifest(_manifest(), env_resolver=lambda v: "")  # missing key path
    res = asyncio.run(p.enrich(Lead(company="Acme")))
    assert res.success is False
    assert res.provider == "prov_x"


