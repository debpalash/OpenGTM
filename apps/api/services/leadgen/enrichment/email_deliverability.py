"""
Email Deliverability Tagger — normalize a verification result into a confidence tag.

Combines the two existing engines:
  • ``email_verify.classify_email``      → role / free / disposable / syntax (no network)
  • ``email_verify_cascade.verify_email`` → SMTP RCPT + catch-all + HTTP-vendor cascade

and maps the outcome to one of three caller-facing tags on ``Lead.email_confidence``:

    "verified"  — SMTP/vendor confirmed the mailbox is deliverable (valid)
    "risky"     — deliverable-but-unreliable: catch-all domain, OR a role mailbox
                  (info@, sales@ …) that we can't tie to a person
    "unknown"   — could not determine (port 25 blocked, greylist, no verifier)

Hard signals override deliverability:
  • invalid syntax / disposable domain / SMTP hard-reject → ""  (drop the email)

Everything degrades gracefully: when SMTP is disabled and no HTTP verifier key is
present, the cascade returns ``unknown`` and we tag "unknown" — never a false
"verified". No exception escapes ``tag_email_confidence``.
"""

import asyncio
import logging
from dataclasses import dataclass
from typing import Optional

from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.email_deliverability")

VERIFIED = "verified"
RISKY = "risky"
UNKNOWN = "unknown"


@dataclass
class DeliverabilityResult:
    email: str = ""
    confidence: str = UNKNOWN     # verified | risky | unknown | "" (drop)
    status: str = ""              # raw cascade status (valid/invalid/catch_all/unknown)
    source: str = ""             # which verifier produced the status
    is_role: bool = False         # info@, sales@, hr@ …
    is_free: bool = False         # gmail/yahoo/…
    is_disposable: bool = False   # throwaway domain
    catch_all: bool = False

    @property
    def keep(self) -> bool:
        return self.confidence != ""


async def verify_deliverability(email: str, workspace_id: Optional[str] = None) -> DeliverabilityResult:
    """Classify + run the verification cascade, returning a normalized result.

    ``workspace_id`` (default None → global config) selects per-workspace Reacher
    config when threading the cascade.
    """
    from apps.api.services.leadgen.enrichment import email_verify as ev
    from apps.api.services.leadgen.enrichment import email_verify_cascade as cascade

    cls = ev.classify_email(email)
    res = DeliverabilityResult(
        email=email,
        is_role=cls.is_role,
        is_free=cls.is_free,
        is_disposable=cls.is_disposable,
    )

    # Hard syntax/disposable rejects — drop without any network call.
    if not cls.valid_syntax:
        res.confidence = ""
        res.status = cascade.INVALID
        res.source = "syntax"
        return res
    if cls.is_disposable:
        res.confidence = ""
        res.status = cascade.INVALID
        res.source = "disposable"
        return res

    try:
        verdict = await cascade.verify_email(email, workspace_id=workspace_id)
    except Exception as exc:  # noqa: BLE001 — never let verification break the caller
        logger.debug("verify_deliverability cascade failed for %s: %s", email, exc)
        res.confidence = UNKNOWN
        res.status = cascade.UNKNOWN
        return res

    res.status = verdict.status
    res.source = verdict.source
    res.catch_all = verdict.status == cascade.CATCH_ALL

    if verdict.status == cascade.INVALID:
        res.confidence = ""                       # hard reject → drop
    elif verdict.status == cascade.CATCH_ALL:
        res.confidence = RISKY                     # deliverable but unverifiable
    elif verdict.status == cascade.VALID:
        # A confirmed mailbox is "verified" unless it's a role account, which we
        # demote to "risky" (deliverable, but not a real person).
        res.confidence = RISKY if cls.is_role else VERIFIED
    else:  # UNKNOWN
        res.confidence = UNKNOWN

    return res


async def tag_email_confidence(lead: Lead, workspace_id: Optional[str] = None) -> Optional[DeliverabilityResult]:
    """Verify ``lead.email`` and write the tag to ``lead.email_confidence``.

    Returns the DeliverabilityResult (or None if the lead has no email). When the
    email is a hard reject (invalid/disposable), the email is cleared from the lead
    along with its provider/confidence so we never ship a known-bad address.
    ``workspace_id`` (default None → global) threads per-workspace verifier config.
    """
    if not lead.has_email:
        return None

    res = await verify_deliverability(lead.email, workspace_id=workspace_id)

    if not res.keep:
        lead.email = ""
        lead.email_confidence = ""
        lead.email_provider = ""
        return res

    lead.email_confidence = res.confidence
    return res


async def tag_email_confidence_batch(leads, limit: Optional[int] = None,
                                     workspace_id: Optional[str] = None) -> list:
    """Tag deliverability for a batch of leads that hold an email.

    Returns the list of DeliverabilityResults produced (one per processed lead).
    Budget-guarded via ``limit``. Never raises on a single-lead failure.
    ``workspace_id`` (default None → global) threads per-workspace verifier config.
    """
    candidates = [l for l in leads if l.has_email]
    if limit is not None:
        candidates = candidates[:limit]
    results = []
    for lead in candidates:
        try:
            r = await tag_email_confidence(lead, workspace_id=workspace_id)
            if r is not None:
                results.append(r)
        except Exception as exc:  # noqa: BLE001
            logger.debug("tag_email_confidence errored for %s: %s", lead.company, exc)
    return results
