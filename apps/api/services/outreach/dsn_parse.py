"""DSN / ARF / heuristic bounce parsing (bounce-ingestion spec, parse-scope v1).

Pure functions over ``email`` std-lib only — NO I/O, NO DB. Given the raw bytes
of an inbound message, classify it into zero or more :class:`BounceRecord`s:

  * **RFC 3464** ``multipart/report; report-type=delivery-status`` — the formal
    Delivery Status Notification. ``Status: 5.x.x`` → hard, ``4.x.x`` → soft.
  * **RFC 5965 ARF** ``multipart/report; report-type=feedback-report`` — a
    feedback-loop / complaint report → ``complaint``.
  * **Heuristic** — a conservative token set over subject + first text part for
    providers that send a plain-text bounce with no MIME report. ``unknown`` when
    unsure (the caller never feeds ``unknown`` to the circuit breaker).

The original (our) Message-ID is recovered from the embedded ``message/rfc822``
or ``text/rfc822-headers`` part; that hard match is the v1 anti-spoof basis.
"""

from __future__ import annotations

import email
import re
from dataclasses import dataclass
from email.message import Message
from typing import List, Optional

# Hard-bounce token set (mirrors sending._is_hard_bounce, extended). Lower-cased.
_HARD_TOKENS = (
    "mailbox unavailable", "user unknown", "no such user", "recipient rejected",
    "does not exist", "mailbox not found", "address rejected", "unknown user",
    "550", "551", "553", "554", "5.1.1", "5.1.2", "5.1.10", "5.4.1",
)
# Subject/body markers that this is *a bounce at all* (conservative gate).
_BOUNCE_MARKERS = (
    "delivery status notification", "undeliverable", "mail delivery failed",
    "returned mail", "delivery failure", "failure notice", "delivery has failed",
    "could not be delivered", "mail delivery subsystem",
)

_MSGID_RE = re.compile(r"<[^>@\s]+@[^>\s]+>")
_MSGID_HEADER_RE = re.compile(r"message-id:\s*(<[^>]+>)", re.IGNORECASE)
_EMAIL_RE = re.compile(r"[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}")


@dataclass
class BounceRecord:
    """One classified feedback record extracted from an inbound message."""

    kind: str  # hard | soft | complaint | unknown
    original_message_id: str = ""
    recipient: str = ""
    diagnostic: str = ""


def source_message_id(raw: bytes) -> str:
    """The inbound message's OWN Message-ID (the ledger's secondary dedup key)."""
    try:
        return (email.message_from_bytes(raw).get("Message-ID") or "").strip()
    except Exception:
        return ""


def parse_message(raw: bytes) -> List[BounceRecord]:
    """Classify an inbound message into zero or more :class:`BounceRecord`s."""
    try:
        msg = email.message_from_bytes(raw)
    except Exception:
        return []

    ctype = msg.get_content_type()
    report_type = (msg.get_param("report-type") or "").lower()

    if ctype == "multipart/report" and report_type == "delivery-status":
        recs = _parse_dsn(msg)
        if recs:
            return recs
    if ctype == "multipart/report" and report_type == "feedback-report":
        recs = _parse_arf(msg)
        if recs:
            return recs
    # Fallback: plain-text bounce heuristic (also covers report-type-less DSNs).
    return _parse_heuristic(msg)


# ── RFC 3464 DSN ──────────────────────────────────────────────────────────────

def _parse_dsn(msg: Message) -> List[BounceRecord]:
    ds_part: Optional[Message] = None
    orig_part: Optional[Message] = None
    for part in msg.walk():
        ct = part.get_content_type()
        if ct == "message/delivery-status" and ds_part is None:
            ds_part = part
        elif ct in ("message/rfc822", "text/rfc822-headers") and orig_part is None:
            orig_part = part

    if ds_part is None:
        return []

    original_mid = _extract_original_message_id(orig_part)
    records: List[BounceRecord] = []
    for block in _status_blocks(ds_part):
        status = (block.get("Status") or "").strip()
        action = (block.get("Action") or "").strip().lower()
        final_rcpt = _addr(block.get("Final-Recipient") or block.get("Original-Recipient") or "")
        diag = (block.get("Diagnostic-Code") or "").strip()[:500]

        kind = _classify_status(status, action)
        if kind is None:
            continue  # per-message block (Reporting-MTA only) → skip
        records.append(BounceRecord(
            kind=kind,
            original_message_id=original_mid,
            recipient=final_rcpt,
            diagnostic=diag or f"dsn status={status} action={action}",
        ))
    return records


def _classify_status(status: str, action: str) -> Optional[str]:
    if status.startswith("5."):
        return "hard"
    if status.startswith("4."):
        return "soft"
    # No usable Status — fall back to Action when present.
    if action == "failed":
        return "hard"
    if action in ("delayed", "delivered", "relayed", "expanded"):
        return "soft" if action == "delayed" else None
    return None


def _status_blocks(ds_part: Message) -> List[Message]:
    """Per-recipient field groups of a message/delivery-status part.

    Python parses message/delivery-status into a list of header-only Messages
    (one per RFC822-ish block). We tolerate a non-list payload by re-parsing the
    raw text into blank-line-separated blocks.
    """
    payload = ds_part.get_payload()
    blocks: List[Message] = []
    if isinstance(payload, list):
        blocks = [p for p in payload if isinstance(p, Message)]
    if blocks:
        return blocks
    # Defensive fallback: parse the raw text into blocks ourselves.
    raw = ds_part.get_payload(decode=True)
    if not raw:
        return []
    text = raw.decode("utf-8", errors="replace")
    for chunk in re.split(r"\n\s*\n", text):
        chunk = chunk.strip()
        if chunk:
            blocks.append(email.message_from_string(chunk))
    return blocks


def _addr(value: str) -> str:
    """Parse a DSN ``*-Recipient`` value (``rfc822; user@example.com``)."""
    if not value:
        return ""
    tail = value.split(";", 1)[-1].strip()
    m = _EMAIL_RE.search(tail)
    return (m.group(0).lower() if m else tail).strip("<> ")


def _extract_original_message_id(part: Optional[Message]) -> str:
    if part is None:
        return ""
    ct = part.get_content_type()
    if ct == "message/rfc822":
        sub = part.get_payload()
        if isinstance(sub, list) and sub:
            sub = sub[0]
        if isinstance(sub, Message):
            return (sub.get("Message-ID") or "").strip()
    elif ct == "text/rfc822-headers":
        raw = part.get_payload(decode=True)
        if raw:
            hdrs = email.message_from_bytes(raw)
            return (hdrs.get("Message-ID") or "").strip()
    return ""


# ── RFC 5965 ARF complaint ────────────────────────────────────────────────────

def _parse_arf(msg: Message) -> List[BounceRecord]:
    fb_part: Optional[Message] = None
    orig_part: Optional[Message] = None
    for part in msg.walk():
        ct = part.get_content_type()
        if ct == "message/feedback-report" and fb_part is None:
            fb_part = part
        elif ct in ("message/rfc822", "text/rfc822-headers") and orig_part is None:
            orig_part = part

    original_mid = _extract_original_message_id(orig_part)
    recipient = ""
    feedback_type = "abuse"
    if fb_part is not None:
        for block in _status_blocks(fb_part):
            recipient = _addr(block.get("Original-Rcpt-To") or "") or recipient
            feedback_type = (block.get("Feedback-Type") or feedback_type).strip()
    if not recipient:
        recipient = _embedded_recipient(orig_part)

    return [BounceRecord(
        kind="complaint",
        original_message_id=original_mid,
        recipient=recipient,
        diagnostic=f"arf feedback-type={feedback_type}",
    )]


def _embedded_recipient(part: Optional[Message]) -> str:
    if part is None:
        return ""
    if part.get_content_type() == "message/rfc822":
        sub = part.get_payload()
        if isinstance(sub, list) and sub:
            sub = sub[0]
        if isinstance(sub, Message):
            m = _EMAIL_RE.search(sub.get("To") or "")
            return m.group(0).lower() if m else ""
    return ""


# ── Plain-text heuristic ──────────────────────────────────────────────────────

def _parse_heuristic(msg: Message) -> List[BounceRecord]:
    subject = msg.get("Subject", "") or ""
    body = _first_text(msg)
    blob = f"{subject}\n{body}".lower()

    has_hard = any(t in blob for t in _HARD_TOKENS)
    looks_bounce = any(m in blob for m in _BOUNCE_MARKERS)
    if not (has_hard or looks_bounce):
        return []  # ordinary reply / autoresponder / marketing → not a bounce

    original_mid = _find_message_id(body)
    recipient = _find_recipient(body)
    # Conservative: only call it a hard bounce when a hard token is present;
    # otherwise it is bounce-shaped but ambiguous → unknown (no breaker action).
    kind = "hard" if has_hard else "unknown"
    return [BounceRecord(
        kind=kind,
        original_message_id=original_mid,
        recipient=recipient,
        diagnostic="heuristic plain-text bounce",
    )]


def _first_text(msg: Message) -> str:
    for part in msg.walk():
        if part.get_content_type() == "text/plain":
            raw = part.get_payload(decode=True)
            if raw:
                return raw.decode("utf-8", errors="replace")
    if not msg.is_multipart():
        raw = msg.get_payload(decode=True)
        if raw:
            return raw.decode("utf-8", errors="replace")
    return ""


def _find_message_id(body: str) -> str:
    if not body:
        return ""
    m = _MSGID_HEADER_RE.search(body)
    if m:
        return m.group(1).strip()
    m = _MSGID_RE.search(body)
    return m.group(0).strip() if m else ""


def _find_recipient(body: str) -> str:
    if not body:
        return ""
    m = _EMAIL_RE.search(body)
    return m.group(0).lower() if m else ""
