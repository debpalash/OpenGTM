"""Tenant-bound OpenID Connect configuration and authorization-code client."""

import json
import secrets
from urllib.parse import urlparse

from authlib.integrations.starlette_client import OAuth

from apps.api.core.config import settings
from apps.api.services.workspace import manager
from apps.api.services.workspace.secrets import get_secret, set_secret

CONFIG_KEY = "OIDC_CONFIG"
SECRET_KEY = "OIDC_CLIENT_SECRET"


def allowed_issuer_hosts() -> set[str]:
    return {host.strip().lower() for host in settings.SSO_ALLOWED_ISSUER_HOSTS.split(",") if host.strip()}


def validate_issuer(issuer: str) -> str:
    normalized = issuer.strip().rstrip("/")
    parsed = urlparse(normalized)
    if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("OIDC issuer must be a clean HTTPS origin or path")
    if parsed.hostname.lower() not in allowed_issuer_hosts():
        raise ValueError("OIDC issuer host is not approved by SSO_ALLOWED_ISSUER_HOSTS")
    return normalized


def get_config(workspace_id: str) -> dict:
    raw = manager.get_workspace_setting(workspace_id, CONFIG_KEY, "")
    if not raw:
        return {"enabled": False, "issuer": "", "client_id": "", "allowed_domains": [], "auto_provision": False, "default_role": "viewer"}
    return json.loads(raw)


def save_config(workspace_id: str, config: dict, client_secret: str = "") -> dict:
    clean = {
        "enabled": bool(config.get("enabled")),
        "issuer": validate_issuer(str(config.get("issuer", ""))) if config.get("issuer") else "",
        "client_id": str(config.get("client_id", "")).strip(),
        "allowed_domains": sorted({str(item).strip().lower() for item in config.get("allowed_domains", []) if str(item).strip()}),
        "auto_provision": bool(config.get("auto_provision")),
        "default_role": str(config.get("default_role", "viewer")),
    }
    if clean["default_role"] not in {"viewer", "member", "editor"}:
        raise ValueError("OIDC default role must be viewer, member, or editor")
    if clean["enabled"] and (not clean["issuer"] or not clean["client_id"] or not (client_secret.strip() or get_secret(workspace_id, SECRET_KEY))):
        raise ValueError("Enabled OIDC requires issuer, client_id, and client_secret")
    manager.set_workspace_setting(workspace_id, CONFIG_KEY, json.dumps(clean, separators=(",", ":")))
    if client_secret.strip():
        set_secret(workspace_id, SECRET_KEY, client_secret)
    return clean


def client_for(workspace_id: str):
    config = get_config(workspace_id)
    if not config.get("enabled"):
        raise ValueError("Workspace SSO is not enabled")
    issuer = validate_issuer(config["issuer"])
    oauth = OAuth()
    return oauth.register(
        name="workspace_oidc",
        client_id=config["client_id"],
        client_secret=get_secret(workspace_id, SECRET_KEY),
        server_metadata_url=f"{issuer}/.well-known/openid-configuration",
        client_kwargs={"scope": "openid email profile"},
    )


def new_nonce() -> str:
    return secrets.token_urlsafe(32)
