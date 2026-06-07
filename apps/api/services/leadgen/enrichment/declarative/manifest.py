"""
Provider manifest model + loader.

A manifest declares a provider's capability, auth, HTTP request, and how to map
the response onto Yupcha's flat enrichment fields — no Python needed. Manifests
live in `manifests/<capability>/<provider>.yaml`.
"""

import os
from pathlib import Path
from typing import Any, Dict, List, Optional

import yaml
from pydantic import BaseModel, Field

MANIFESTS_DIR = Path(__file__).parent / "manifests"


class AuthSpec(BaseModel):
    # type: header | bearer | query | none
    type: str = "none"
    # name of the header/query param (e.g. "X-Api-Key", "api_key")
    param: Optional[str] = None
    # value template, typically "${env:SOME_KEY}" (bearer prepends "Bearer ")
    value: Optional[str] = None
    # the env/settings var the key comes from (for availability checks + UI)
    env_var: Optional[str] = None


class RequestSpec(BaseModel):
    method: str = "POST"
    url: str                                   # may contain {{input.x}} / ${env:X}
    headers: Dict[str, Any] = Field(default_factory=dict)
    query: Dict[str, Any] = Field(default_factory=dict)
    body_template: Optional[Any] = None        # dict/list rendered with input ctx
    timeout: float = 20.0


class ResponseSpec(BaseModel):
    # provider returned an error envelope if this path is truthy (JSONPath-lite, no `$.`)
    error_path: Optional[str] = None
    error_message_path: Optional[str] = None
    # output_field -> JSONPath-lite expr
    mappings: Dict[str, str] = Field(default_factory=dict)


class ProviderManifest(BaseModel):
    name: str                                  # unique provider id, e.g. "leadmagic_email"
    capability: str                            # e.g. "email" (a Lead/enrichment field)
    capabilities: List[str] = Field(default_factory=list)  # extra fields it can fill
    description: str = ""
    default_confidence: float = 0.7
    cost_per_lookup: float = 0.0
    auth: AuthSpec = Field(default_factory=AuthSpec)
    request: RequestSpec
    response: ResponseSpec = Field(default_factory=ResponseSpec)
    # maps Lead attributes → input keys available to the templates as input.<key>.
    # default identity-ish set is always added (company, website, domain, email, ...).
    input_fields: List[str] = Field(default_factory=list)

    def all_capabilities(self) -> List[str]:
        caps = list(dict.fromkeys([self.capability, *self.capabilities, *self.response.mappings.keys()]))
        return [c for c in caps if c]


def _coerce(raw: Dict[str, Any]) -> Dict[str, Any]:
    """Accept a couple of friendly YAML aliases (request.body → body_template)."""
    raw = dict(raw)
    req = dict(raw.get("request") or {})
    if "body" in req and "body_template" not in req:
        req["body_template"] = req.pop("body")
    raw["request"] = req
    return raw


def load_manifest(path: Path) -> ProviderManifest:
    with open(path, "r") as f:
        raw = yaml.safe_load(f)
    if not isinstance(raw, dict):
        raise ValueError(f"manifest {path} is not a mapping")
    return ProviderManifest(**_coerce(raw))


def load_all_manifests(directory: Optional[Path] = None) -> List[ProviderManifest]:
    """Load every *.yaml/*.yml manifest under the manifests dir (recursive)."""
    directory = directory or MANIFESTS_DIR
    manifests: List[ProviderManifest] = []
    if not directory.exists():
        return manifests
    for p in sorted(directory.rglob("*.y*ml")):
        try:
            manifests.append(load_manifest(p))
        except Exception as e:  # one bad manifest shouldn't kill the rest
            import logging
            logging.getLogger("leadgen.declarative").warning(f"bad manifest {p}: {e}")
    return manifests
