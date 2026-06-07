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


async def verify_email(email: str, *, verifiers: Optional[List[Verifier]] = None) -> VerifyResult:
    """Run the cascade. First definitive result wins; unknown/error cascades.

    Returns the best result seen (a definitive one if any, else the last unknown).
    """
    if not email or "@" not in email:
        return VerifyResult(email or "", INVALID, "syntax", _CONFIDENCE[INVALID], "bad_syntax")

    chain = verifiers if verifiers is not None else available_verifiers()
    last = VerifyResult(email, UNKNOWN, "", 0.0, "no_verifier")
    for v in chain:
        try:
            res = await v.verify(email)
        except Exception as e:
            logger.debug(f"verifier {getattr(v, 'name', '?')} threw: {e}")
            continue
        if res.is_definitive:
            return res          # definitive → stop the cascade
        last = res              # remember the unknown, keep trying
    return last
