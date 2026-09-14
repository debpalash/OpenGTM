"""
Provider manifest model + loader.

A manifest declares a provider's capability, auth, HTTP request, and how to map
the response onto Yupcha's flat enrichment fields — no Python needed. Manifests
live in `manifests/<capability>/<provider>.yaml`.
"""

import hashlib
import os
import re
from pathlib import Path
from typing import Any, Dict, List, Optional, Literal

import yaml
from pydantic import BaseModel, Field

MANIFESTS_DIR = Path(__file__).parent / "manifests"
TRUST_STORE = Path(__file__).resolve().parents[6] / "docs" / "connectors" / "trusted-publishers.json"


def _signature_policy() -> str:
    value = os.getenv("CONNECTOR_SIGNATURE_POLICY", "optional").strip().lower()
    return value if value in {"optional", "required"} else "required"


class AuthSpec(BaseModel):
    # type: header | bearer | query | none
    type: Literal["header", "bearer", "query", "none"] = "none"
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
    manifest_version: Literal["1"] = "1"
    name: str                                  # unique provider id, e.g. "leadmagic_email"
    display_name: str = ""
    author: str = "community"
    homepage: str = ""
    license: str = "Apache-2.0"
    tags: List[str] = Field(default_factory=list)
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

    def catalog_entry(self) -> Dict[str, Any]:
        return {
            "manifest_version": self.manifest_version,
            "id": self.name,
            "name": self.display_name or self.name.replace("_", " ").title(),
            "author": self.author,
            "homepage": self.homepage,
            "license": self.license,
            "description": self.description,
            "capabilities": self.all_capabilities(),
            "tags": self.tags,
            "cost_per_lookup": self.cost_per_lookup,
            "default_confidence": self.default_confidence,
            "credential_key": self.auth.env_var,
        }


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
            from .signing import verify_manifest
            signature = verify_manifest(p, TRUST_STORE)
            if signature["status"] in {"invalid", "untrusted"} or (_signature_policy() == "required" and signature["status"] != "trusted"):
                raise ValueError(f"connector signature is {signature['status']}")
            manifests.append(load_manifest(p))
        except Exception as e:  # one bad manifest shouldn't kill the rest
            import logging
            logging.getLogger("leadgen.declarative").warning(f"bad manifest {p}: {e}")
    return manifests


def validate_manifest_directory(directory: Optional[Path] = None, *, signature_policy: Optional[str] = None, trust_store: Optional[Path] = None) -> Dict[str, Any]:
    """Fail-loud compatibility report used by CI and connector contributors."""
    directory = directory or MANIFESTS_DIR
    from .signing import canonical_manifest, verify_manifest
    errors, manifests, names = [], [], set()
    policy = signature_policy or _signature_policy()
    if policy not in {"optional", "required"}: policy = "required"
    trusted_keys = trust_store or TRUST_STORE
    for path in sorted(directory.rglob("*.y*ml")) if directory.exists() else []:
        try:
            manifest = load_manifest(path)
            if not re.fullmatch(r"[a-z][a-z0-9_]{2,63}", manifest.name):
                raise ValueError("name must match ^[a-z][a-z0-9_]{2,63}$")
            if manifest.name in names:
                raise ValueError(f"duplicate provider id '{manifest.name}'")
            names.add(manifest.name)
            if manifest.request.method.upper() not in {"GET", "POST", "PUT", "PATCH"}:
                raise ValueError("request.method must be GET, POST, PUT, or PATCH")
            if not manifest.request.url.startswith("https://"):
                raise ValueError("connector request URL must use HTTPS")
            if not manifest.response.mappings:
                raise ValueError("response.mappings must not be empty")
            if not 0 <= manifest.default_confidence <= 1 or manifest.cost_per_lookup < 0:
                raise ValueError("confidence must be 0..1 and cost must be non-negative")
            if not 0 < manifest.request.timeout <= 120:
                raise ValueError("request.timeout must be between 0 and 120 seconds")
            if manifest.auth.type != "none" and not manifest.auth.env_var:
                raise ValueError("authenticated connectors require auth.env_var")
            signature = verify_manifest(path, trusted_keys)
            if signature["status"] in {"invalid", "untrusted"} or (policy == "required" and signature["status"] != "trusted"):
                raise ValueError(f"connector signature is {signature['status']}: {signature.get('error', '')}".rstrip())
            manifests.append({
                "path": str(path.relative_to(directory)),
                "manifest_sha256": hashlib.sha256(canonical_manifest(path)).hexdigest(),
                "signature": signature,
                **manifest.catalog_entry(),
            })
        except Exception as exc:
            errors.append({"path": str(path), "error": str(exc)})
    return {"ok": not errors, "manifest_version": "1", "signature_policy": policy, "count": len(manifests), "connectors": manifests, "errors": errors}
