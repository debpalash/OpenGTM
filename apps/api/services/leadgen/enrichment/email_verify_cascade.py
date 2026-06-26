"""
Normalized email-verification cascade.

Ported from bricks (validate-email/route.ts — 8-vendor cascade → 4 unified
statuses) + tr4m0ryp/clay-enrichment (smtp_verify/protocol.py port-25 probe +
email_verifier_api.py HTTP fallback). See docs/clay-alternatives-ingestion-
catalog.md (top-10 #4).

Yupcha already has a port-25 SMTP probe with catch-all calibration
(email_verify.probe_domain). This adds:
  1. ONE status contract — every verifier maps to valid | invalid | catch_all |
     unknown — so callers never reason about vendor-specific JSON.
  2. A cascade: free SMTP probe (layer 0) → HTTP verifiers (for cloud envs where
     outbound port 25 is blocked). A DEFINITIVE result (valid/invalid/catch_all)
     stops the chain; unknown/error cascades to the next verifier.
The cascade self-configures: a verifier with no API key reports unavailable and
is skipped, so it works with whatever BYOK keys exist.
"""

import asyncio
import logging
from dataclasses import dataclass
from typing import Awaitable, Callable, List, Optional

logger = logging.getLogger("leadgen.email_verify_cascade")

# The four normalized statuses every verifier maps to.
VALID = "valid"
INVALID = "invalid"
CATCH_ALL = "catch_all"
UNKNOWN = "unknown"

DEFINITIVE = {VALID, INVALID, CATCH_ALL}

_CONFIDENCE = {VALID: 0.95, INVALID: 0.9, CATCH_ALL: 0.5, UNKNOWN: 0.0}


@dataclass
class VerifyResult:
    email: str
    status: str = UNKNOWN          # valid | invalid | catch_all | unknown
    source: str = ""               # which verifier produced it
    confidence: float = 0.0
    detail: str = ""

    @property
    def is_definitive(self) -> bool:
        return self.status in DEFINITIVE

    @property
    def deliverable(self) -> bool:
        # catch_all is "risky deliverable" — usable for cold outreach
        return self.status in (VALID, CATCH_ALL)


# A verifier is an async callable email -> VerifyResult, plus an `is_available()`
# and a `name`. We model them as small objects.
class Verifier:
    name: str = "verifier"

    def is_available(self) -> bool:
        return True

    async def verify(self, email: str) -> VerifyResult:  # pragma: no cover - interface
        raise NotImplementedError


# ── Layer 0: free SMTP probe (wraps the existing probe_domain) ──────────────

class SmtpVerifier(Verifier):
    name = "smtp"

    def is_available(self) -> bool:
        # Available unless SMTP probing is disabled (e.g. port 25 blocked → set
        # WORKBOOK_SMTP_DISABLED=1 in cloud); cascade then falls to HTTP vendors.
        try:
            from apps.api.services.leadgen.enrichment.email_verify import _SMTP_ENABLED
            return bool(_SMTP_ENABLED)
        except Exception:
            return True

    async def verify(self, email: str) -> VerifyResult:
        from apps.api.services.leadgen.enrichment.email_verify import probe_domain
        domain = email.rsplit("@", 1)[-1] if "@" in email else ""
        if not domain:
            return VerifyResult(email, INVALID, self.name, _CONFIDENCE[INVALID], "no_domain")
        # probe_domain is blocking (smtplib) → run off the event loop
        probe = await asyncio.to_thread(probe_domain, domain, [email])
        if not probe.reachable:
            return VerifyResult(email, UNKNOWN, self.name, 0.0, "unreachable")
        if probe.catch_all:
            return VerifyResult(email, CATCH_ALL, self.name, _CONFIDENCE[CATCH_ALL], "catch_all")
        res = (probe.results or {}).get(email)
        if res is True:
            return VerifyResult(email, VALID, self.name, _CONFIDENCE[VALID], "rcpt_accepted")
        if res is False:
            return VerifyResult(email, INVALID, self.name, _CONFIDENCE[INVALID], "rcpt_rejected")
        return VerifyResult(email, UNKNOWN, self.name, 0.0, "soft")


# ── HTTP-verifier adapters (fallback when port 25 is blocked) ──────────────
# Each wraps a vendor and maps its statuses to the 4-status contract. They are
# keyed by env vars and report unavailable when the key is absent.

def _status_from_vendor(raw: str) -> str:
    """Map common vendor status strings to the 4-status contract."""
    r = (raw or "").lower()
    if r in ("valid", "deliverable", "ok", "safe"):
        return VALID
    if r in ("invalid", "undeliverable", "bad", "rejected", "disposable"):
        return INVALID
    if r in ("catch_all", "catchall", "accept_all", "accept-all", "unknown_catch_all"):
        return CATCH_ALL
    return UNKNOWN


# Registry of available verifiers, in cascade order (SMTP first = free).
_VERIFIERS: List[Verifier] = [SmtpVerifier()]


def register_verifier(v: Verifier, *, front: bool = False):
    if front:
        _VERIFIERS.insert(0, v)
    else:
        _VERIFIERS.append(v)


def available_verifiers() -> List[Verifier]:
    out = []
    for v in _VERIFIERS:
        try:
            if v.is_available():
                out.append(v)
        except Exception:
            continue
    return out


def _bootstrap_verifiers(workspace_id: Optional[str] = None) -> List[Verifier]:
    """Build the cascade chain for this workspace.

    Reacher (when enabled + configured + breaker-closed) goes at the FRONT, ahead
    of the registered SMTP/HTTP verifiers. We do NOT mutate the global
    ``_VERIFIERS`` registry with a workspace-scoped Reacher instance — that would
    leak one tenant's config across calls — so we prepend a freshly-resolved
    ReacherVerifier per call instead. When Reacher is unavailable (the default,
    flag off), this returns exactly ``available_verifiers()`` → today's behavior.
    """
    base = available_verifiers()
    try:
        from apps.api.services.leadgen.enrichment.providers.reacher_verify import (
            ReacherVerifier,
        )
        reacher = ReacherVerifier(workspace_id)
        if reacher.is_available():
            return [reacher] + base
    except Exception as e:  # never let optional Reacher break the cascade
        logger.debug(f"reacher bootstrap skipped: {e}")
    return base


def _reacher_active(chain: List[Verifier]) -> bool:
    return any(getattr(v, "name", "") == "reacher" for v in chain)


async def verify_email(
    email: str,
    *,
    verifiers: Optional[List[Verifier]] = None,
    workspace_id: Optional[str] = None,
) -> VerifyResult:
    """Run the cascade. First definitive result wins; unknown/error cascades.

    Returns the best result seen (a definitive one if any, else the last unknown).

    ``workspace_id`` (default None → global config) selects per-workspace Reacher
    config. When an explicit ``verifiers`` list is supplied (tests / custom
    callers) the chain is used as-is with no cache and no Reacher injection, so
    legacy single-arg calls behave exactly as before.
    """
    if not email or "@" not in email:
        return VerifyResult(email or "", INVALID, "syntax", _CONFIDENCE[INVALID], "bad_syntax")

    if verifiers is not None:
        chain = verifiers
        cache = None
    else:
        chain = _bootstrap_verifiers(workspace_id)
        # Engage the per-email cache only when Reacher is in play, so a flag-OFF
        # install is byte-for-byte today's SMTP-only path (no new persistence).
        cache = None
        if _reacher_active(chain):
            try:
                from apps.api.services.leadgen.enrichment.email_verify_cache import (
                    get_verify_cache,
                )
                cache = get_verify_cache()
            except Exception as e:
                logger.debug(f"verify cache unavailable: {e}")

    last = VerifyResult(email, UNKNOWN, "", 0.0, "no_verifier")
    for v in chain:
        name = getattr(v, "name", "?")
        # 1) Cache lookup (per email-hash + verifier).
        if cache is not None:
            try:
                hit = cache.get(email, name)
            except Exception:
                hit = None
            if hit is not None:
                res = VerifyResult(email, hit["status"], hit["source"] or name,
                                   hit["confidence"], hit["detail"])
                if res.is_definitive:
                    return res
                last = res
                continue
        # 2) Live verify.
        try:
            res = await v.verify(email)
        except Exception as e:
            logger.debug(f"verifier {name} threw: {e}")
            continue
        # 3) Cache the result (skip transient infra errors so they self-heal).
        if cache is not None and not _is_transient(res):
            try:
                cache.set(email, name, res.status, source=res.source,
                          detail=res.detail, confidence=res.confidence)
            except Exception:
                pass
        if res.is_definitive:
            return res          # definitive → stop the cascade
        last = res              # remember the unknown, keep trying
    return last


def _is_transient(res: VerifyResult) -> bool:
    """True for UNKNOWN results that represent an infra failure (don't cache)."""
    if res.status != UNKNOWN:
        return False
    try:
        from apps.api.services.leadgen.enrichment.providers.reacher_verify import (
            ERROR_DETAILS,
        )
        return res.detail in ERROR_DETAILS
    except Exception:
        return res.detail in ("error", "timeout", "breaker_open", "unconfigured")
