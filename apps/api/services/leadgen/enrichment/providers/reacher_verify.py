"""
Reacher email-verification provider (self-hosted or commercial-cloud).

Reacher (``reacherhq/check-if-email-exists``) is the strongest tier of the email
cascade: real SMTP RCPT probing + catch-all + disposable + role detection,
unlimited and $0/email when self-hosted. This module wraps it as one more
``Verifier`` for ``email_verify_cascade`` — registered at the FRONT of the chain
when configured, with the existing port-25 SMTP probe as the fallback.

Design (see docs/specs/research-reacher-smtp-verify-spec.md):
  * ``ReacherClient`` — thin async httpx wrapper. POST /v0/check_email
    {"to_email": ...} → {is_reachable, smtp:{is_catch_all,...}, misc:{is_disposable,
    is_role_account}}. Maps ``is_reachable`` to the 4-status contract.
  * ``ReacherVerifier`` — the cascade adapter. ``is_available()`` is True only when
    the per-workspace/global enable flag is set, the GLOBAL url resolves + passes
    the SSRF allowlist, AND the circuit breaker is closed. ``verify()`` NEVER
    raises — any error/timeout becomes a non-definitive UNKNOWN so the cascade
    falls through to SMTP (we never coerce a stall into a false "verified").
  * Circuit breaker (process-global): after N consecutive failures, report
    unavailable for a cooldown so a dead Reacher never adds latency to every check.

SECURITY: ``REACHER_URL`` is GLOBAL/env-only (never per-workspace) — a tenant must
not be able to point our outbound requests at an arbitrary internal host
(confused-deputy / SSRF). Per-workspace config is limited to the enable flag and
an optional cloud API key. The url is validated against a scheme/host allowlist.
"""

import logging
import os
import threading
import time
from typing import Optional
from urllib.parse import urlparse

from apps.api.services.leadgen.enrichment.email_verify_cascade import (
    VALID, INVALID, CATCH_ALL, UNKNOWN, _CONFIDENCE, Verifier, VerifyResult,
)

logger = logging.getLogger("leadgen.enrichment.reacher")

# Config keys (resolved via get_secret for ws-scoped ones; URL is global-only).
ENV_ENABLED = "REACHER_ENABLED"
ENV_URL = "REACHER_URL"
ENV_API_KEY = "REACHER_API_KEY"
ENV_TIMEOUT = "REACHER_TIMEOUT"

DEFAULT_TIMEOUT = 12.0

# Circuit-breaker tunables (global/env).
_BREAKER_THRESHOLD = int(os.getenv("REACHER_BREAKER_THRESHOLD", "3"))
_BREAKER_COOLDOWN = float(os.getenv("REACHER_BREAKER_COOLDOWN", "60"))

# Detail markers for results that must NOT be cached (transient infra failures);
# the cascade inspects these so a flapping Reacher doesn't poison the cache.
ERROR_DETAILS = frozenset({"error", "timeout", "breaker_open", "http_error", "unconfigured"})

_FALSEY = {"", "0", "false", "no", "off"}


def _truthy(val: str) -> bool:
    return (val or "").strip().lower() not in _FALSEY


# ── SSRF allowlist ─────────────────────────────────────────────────────────

# Cloud metadata / link-local endpoints an attacker would target via a confused
# deputy. Even though REACHER_URL is operator-set, we reject these at load as a
# belt-and-suspenders against a misconfiguration.
_BLOCKED_HOSTS = {"169.254.169.254", "metadata.google.internal", "metadata"}


def validate_reacher_url(url: str) -> bool:
    """True if ``url`` is a safe Reacher endpoint: http(s) scheme, real host,
    not a cloud-metadata/link-local address."""
    if not url:
        return False
    try:
        parsed = urlparse(url)
    except Exception:
        return False
    if parsed.scheme not in ("http", "https"):
        return False
    host = (parsed.hostname or "").lower()
    if not host:
        return False
    if host in _BLOCKED_HOSTS:
        return False
    if host.startswith("169.254."):  # link-local block (AWS/Azure/GCP metadata)
        return False
    return True


# ── Circuit breaker (process-global) ───────────────────────────────────────

class _CircuitBreaker:
    """Trip open after N consecutive failures; stay open for a cooldown."""

    def __init__(self, threshold: int, cooldown: float):
        self.threshold = max(1, threshold)
        self.cooldown = cooldown
        self._lock = threading.Lock()
        self._consecutive = 0
        self._open_until = 0.0

    def is_open(self) -> bool:
        with self._lock:
            if self._open_until and time.time() < self._open_until:
                return True
            return False

    def record_success(self) -> None:
        with self._lock:
            self._consecutive = 0
            self._open_until = 0.0

    def record_failure(self) -> None:
        with self._lock:
            self._consecutive += 1
            if self._consecutive >= self.threshold:
                self._open_until = time.time() + self.cooldown
                logger.warning(
                    "Reacher circuit breaker OPEN after %d consecutive failures "
                    "(cooldown %.0fs)", self._consecutive, self.cooldown,
                )

    def reset(self) -> None:
        with self._lock:
            self._consecutive = 0
            self._open_until = 0.0


_breaker = _CircuitBreaker(_BREAKER_THRESHOLD, _BREAKER_COOLDOWN)


# ── Config resolution ──────────────────────────────────────────────────────

def _resolve_global(key: str, default: str = "") -> str:
    """Resolve a GLOBAL setting (settings DB → env), never per-workspace.

    Used for REACHER_URL: a tenant must not be able to override the outbound host.
    """
    try:
        from apps.api.routers.settings import _db_get
        return _db_get(key, default)
    except Exception:
        return os.environ.get(key, default)


def _resolve_ws(workspace_id: Optional[str], key: str, default: str = "") -> str:
    """Resolve a workspace-overridable secret (per-workspace → global → default).

    Defense-in-depth: a key on the GLOBAL-only allowlist (e.g. REACHER_URL) is
    forced through global resolution even if called with a workspace_id, so a
    tenant secret can never override it.
    """
    try:
        from apps.api.routers.settings import is_workspace_settable
        if not is_workspace_settable(key):
            return _resolve_global(key, default)
    except Exception:
        if key == ENV_URL:
            return _resolve_global(key, default)
    try:
        from apps.api.services.workspace.secrets import get_secret
        return get_secret(workspace_id, key, default)
    except Exception:
        return _resolve_global(key, default)


# ── Status mapping ─────────────────────────────────────────────────────────

def map_reacher_status(payload: dict) -> VerifyResult:
    """Map a Reacher /v0/check_email response to the 4-status contract.

    is_reachable: safe→valid | invalid→invalid | risky→catch_all (if catch-all)
    else unknown | unknown→unknown. Disposable→invalid. Role flag surfaced in
    detail (the deliverability tagger demotes role mailboxes separately).
    """
    email = (payload.get("input") or payload.get("syntax", {}).get("address") or "")
    reachable = (payload.get("is_reachable") or "").lower()
    smtp = payload.get("smtp") or {}
    misc = payload.get("misc") or {}
    is_catch_all = bool(smtp.get("is_catch_all"))
    is_disposable = bool(misc.get("is_disposable"))
    is_role = bool(misc.get("is_role_account"))

    detail_bits = []
    if is_role:
        detail_bits.append("role")
    if is_catch_all:
        detail_bits.append("catch_all")

    # Disposable domains are junk regardless of reachability.
    if is_disposable:
        return VerifyResult(email, INVALID, "reacher", _CONFIDENCE[INVALID], "disposable")

    if reachable == "safe":
        status = VALID
    elif reachable == "invalid":
        status = INVALID
    elif reachable == "risky":
        # Catch-all domains accept everything → CATCH_ALL (risky-deliverable).
        # Other "risky" reasons (full mailbox, etc.) are non-definitive → UNKNOWN
        # so the cascade can corroborate via SMTP.
        status = CATCH_ALL if is_catch_all else UNKNOWN
    else:  # "unknown" (e.g. Gmail/Microsoft block probing) or anything unexpected
        status = UNKNOWN

    detail = ",".join(detail_bits) if detail_bits else reachable or "unknown"
    return VerifyResult(email, status, "reacher", _CONFIDENCE[status], detail)


# ── Client ─────────────────────────────────────────────────────────────────

class ReacherClient:
    """Async httpx wrapper around a Reacher /v0/check_email endpoint."""

    def __init__(self, url: str, api_key: str = "", timeout: float = DEFAULT_TIMEOUT):
        self.url = url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    async def check(self, email: str) -> dict:
        """POST the email to Reacher, returning the raw JSON payload.

        Raises on transport/HTTP error (the caller maps that to UNKNOWN + breaker).
        """
        import httpx

        headers = {"Content-Type": "application/json"}
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        async with httpx.AsyncClient(timeout=self.timeout) as client:
            resp = await client.post(
                f"{self.url}/v0/check_email",
                json={"to_email": email},
                headers=headers,
            )
            resp.raise_for_status()
            return resp.json()


# ── Verifier (cascade adapter) ─────────────────────────────────────────────

class ReacherVerifier(Verifier):
    name = "reacher"

    def __init__(self, workspace_id: Optional[str] = None):
        self.workspace_id = workspace_id

    # -- config (resolved lazily so flips take effect without a restart) --
    def _enabled(self) -> bool:
        return _truthy(_resolve_ws(self.workspace_id, ENV_ENABLED, ""))

    def _url(self) -> str:
        # GLOBAL ONLY — never per-workspace (SSRF mitigation).
        return _resolve_global(ENV_URL, "").strip()

    def _api_key(self) -> str:
        return _resolve_ws(self.workspace_id, ENV_API_KEY, "").strip()

    def _timeout(self) -> float:
        try:
            return float(_resolve_global(ENV_TIMEOUT, "") or DEFAULT_TIMEOUT)
        except (TypeError, ValueError):
            return DEFAULT_TIMEOUT

    def is_available(self) -> bool:
        """True only when enabled, the global URL is valid, and breaker is closed."""
        if not self._enabled():
            return False
        url = self._url()
        if not validate_reacher_url(url):
            if url:
                logger.warning("REACHER_URL %r rejected by allowlist — disabling Reacher", url)
            return False
        if _breaker.is_open():
            return False
        return True

    async def verify(self, email: str) -> VerifyResult:
        """Verify via Reacher. NEVER raises — failures degrade to UNKNOWN and feed
        the circuit breaker so a dead endpoint doesn't add latency to every check."""
        if _breaker.is_open():
            return VerifyResult(email, UNKNOWN, self.name, 0.0, "breaker_open")
        url = self._url()
        if not validate_reacher_url(url):
            return VerifyResult(email, UNKNOWN, self.name, 0.0, "unconfigured")

        client = ReacherClient(url, self._api_key(), self._timeout())
        try:
            payload = await client.check(email)
        except Exception as exc:  # noqa: BLE001 — degrade safely, count for breaker
            _breaker.record_failure()
            detail = "timeout" if "timeout" in type(exc).__name__.lower() else "error"
            logger.debug("Reacher verify failed for %s: %s", email, exc)
            return VerifyResult(email, UNKNOWN, self.name, 0.0, detail)

        _breaker.record_success()
        result = map_reacher_status(payload)
        # Reacher echoes the input only sometimes; keep the queried address.
        result.email = email
        return result
