"""
Compile a ProviderManifest into a DeclarativeProvider(EnrichmentProvider).

The compiled provider builds its HTTP request from the manifest's templates +
the lead's data, calls the endpoint, detects the vendor error envelope, and
projects the response onto enrichment fields — so it drops straight into the
existing WaterfallEnricher / provider registry.
"""

import logging
import os
from typing import Any, Callable, Dict

import httpx

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.enrichment.declarative.manifest import ProviderManifest
from apps.api.services.leadgen.enrichment.declarative.template import (
    render_string, render_template, project_response, project_value,
)

logger = logging.getLogger("leadgen.declarative")


class MissingApiKeyError(Exception):
    """Raised (and caught → provider unavailable) when a required key is absent."""


def _default_env_resolver(var: str) -> str:
    """Resolve an env/settings var: settings DB first, then environment."""
    try:
        from apps.api.database import get_setting
        v = get_setting(var, "") or os.getenv(var, "")
    except Exception:
        v = os.getenv(var, "")
    return v or ""


def _domain_of(website: str) -> str:
    if not website:
        return ""
    d = website.strip().lower()
    for p in ("https://", "http://", "www."):
        d = d.removeprefix(p)
    return d.split("/")[0].split("?")[0]


def _lead_input_ctx(lead: Lead) -> Dict[str, Any]:
    """Build the `input.*` context the manifest templates can reference."""
    contact = getattr(lead, "contact_person", "") or ""
    first = last = ""
    if contact:
        parts = contact.split()
        first = parts[0] if parts else ""
        last = parts[-1] if len(parts) > 1 else ""
    website = getattr(lead, "website", "") or ""
    return {
        "company": getattr(lead, "company", "") or "",
        "company_name": getattr(lead, "company", "") or "",
        "website": website,
        "domain": _domain_of(website),
        "email": getattr(lead, "email", "") or "",
        "linkedin_url": getattr(lead, "linkedin_url", "") or "",
        "city": getattr(lead, "city", "") or "",
        "contact_person": contact,
        "full_name": contact,
        "first_name": first,
        "last_name": last,
        "phone": getattr(lead, "phone", "") or "",
    }


class DeclarativeProvider(EnrichmentProvider):
    """An EnrichmentProvider whose behavior comes entirely from a manifest."""

    def __init__(self, manifest: ProviderManifest, env_resolver: Callable[[str], str] = None):
        self.manifest = manifest
        self.name = manifest.name
        self.capabilities = manifest.all_capabilities()
        self.default_confidence = manifest.default_confidence
        self.cost_per_lookup = manifest.cost_per_lookup
        self._env = env_resolver or _default_env_resolver

    # ── availability (used by the capability registry) ──
    def is_available(self) -> bool:
        ev = self.manifest.auth.env_var
        if self.manifest.auth.type == "none" or not ev:
            return True
        return bool(self._env(ev))

    def _build_auth(self, headers: Dict[str, str], params: Dict[str, str]):
        auth = self.manifest.auth
        if auth.type == "none":
            return
        if auth.env_var and not self._env(auth.env_var):
            raise MissingApiKeyError(f"{self.name}: missing {auth.env_var}")
        value = render_string(auth.value or "", {}, self._env) if auth.value else self._env(auth.env_var or "")
        if auth.type == "bearer":
            headers["Authorization"] = f"Bearer {value}"
        elif auth.type == "header":
            headers[auth.param or "Authorization"] = value
        elif auth.type == "query":
            params[auth.param or "api_key"] = value

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        import time
        t0 = time.monotonic()
        ctx = {"input": _lead_input_ctx(lead)}
        req = self.manifest.request

        url = render_string(req.url, ctx, self._env)
        headers = {k: render_string(str(v), ctx, self._env) for k, v in (req.headers or {}).items()}
        params = {k: render_string(str(v), ctx, self._env) for k, v in (req.query or {}).items()}
        body = render_template(req.body_template, ctx, self._env) if req.body_template is not None else None

        # Fail fast on a config error (missing key) before any network/DNS work.
        try:
            self._build_auth(headers, params)
        except MissingApiKeyError as e:
            return EnrichmentResult(success=False, error=str(e))

        # SSRF guard: a manifest may template lead-controlled data (domain, etc.)
        # into the URL; never let that point us at a private/metadata host.
        # resolve=True also resolves the host and rejects if it maps to a
        # private/metadata IP (closes most of the DNS-rebinding gap).
        try:
            from apps.api.core.url_guard import check_url
            check_url(url, allow_http=True, resolve=True)
        except Exception as e:
            return EnrichmentResult(success=False, error=f"blocked_url: {str(e)[:60]}")

        try:
            async with httpx.AsyncClient(timeout=req.timeout) as client:
                resp = await client.request(
                    req.method.upper(), url, headers=headers or None,
                    params=params or None, json=body if body is not None else None,
                )
            duration_ms = (time.monotonic() - t0) * 1000.0
            if resp.status_code >= 400:
                return EnrichmentResult(success=False, error=f"http_{resp.status_code}", duration_ms=duration_ms)
            try:
                data = resp.json()
            except Exception:
                return EnrichmentResult(success=False, error="non_json_response", duration_ms=duration_ms)

            # vendor error envelope (some APIs return {error:true} on a 200)
            rs = self.manifest.response
            if rs.error_path and project_value(data, f"$.{rs.error_path}"):
                msg = project_value(data, f"$.{rs.error_message_path}") if rs.error_message_path else "provider_error"
                return EnrichmentResult(success=False, error=str(msg)[:120], duration_ms=duration_ms)

            fields = project_response(data, rs.mappings)
            return EnrichmentResult(
                success=bool(fields),
                fields=fields,
                confidence=self.default_confidence,
                duration_ms=duration_ms,
                error=None if fields else "no_data",
            )
        except httpx.TimeoutException:
            return EnrichmentResult(success=False, error="timeout")
        except Exception as e:
            return EnrichmentResult(success=False, error=str(e)[:120])


def compile_manifest(manifest: ProviderManifest, env_resolver: Callable[[str], str] = None) -> DeclarativeProvider:
    return DeclarativeProvider(manifest, env_resolver=env_resolver)
