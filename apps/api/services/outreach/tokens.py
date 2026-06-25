"""Unsubscribe HMAC tokens (spec §5.1, §7).

The signing key is derived through the SAME fail-closed ``_load_master_key``
path as secret encryption — NOT a bare ``SECRET_KEY`` read — so a default/
insecure SECRET_KEY in a real deployment cannot forge tokens. Verification is
constant-time (``hmac.compare_digest``). Token scope is per-recipient-per-
workspace (email + workspace_id); ``sequence_id`` is carried as ``source`` only.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import time
from typing import Optional, Tuple

from apps.api.core.config import settings


def _signing_key() -> bytes:
    """Derive a stable HMAC key from the Fernet master key (fail-closed)."""
    from apps.api.services.workspace.secrets import _load_master_key

    master = _load_master_key()  # raises in prod when insecure default
    return hashlib.sha256(b"outreach-unsub:" + master).digest()


def _b64e(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def _b64d(s: str) -> bytes:
    pad = "=" * (-len(s) % 4)
    return base64.urlsafe_b64decode(s + pad)


def make_unsubscribe_token(
    workspace_id: str, email: str, sequence_id: str = "", ttl_days: Optional[int] = None
) -> str:
    ttl = ttl_days if ttl_days is not None else settings.OUTREACH_UNSUB_TTL_DAYS
    exp = int(time.time()) + int(ttl) * 86400
    payload = {
        "ws": workspace_id,
        "email": (email or "").strip().lower(),
        "seq": sequence_id or "",
        "exp": exp,
    }
    body = _b64e(json.dumps(payload, separators=(",", ":"), sort_keys=True).encode("utf-8"))
    sig = _b64e(hmac.new(_signing_key(), body.encode("ascii"), hashlib.sha256).digest())
    return f"{body}.{sig}"


def verify_unsubscribe_token(token: str, *, allow_expired: bool = False) -> Optional[dict]:
    """Return the payload dict if valid (constant-time), else None.

    ``allow_expired=True`` skips the TTL check so the GET landing page can render
    a re-request flow; the POST one-click target uses the default (rejects
    expired) to bound replay.
    """
    if not token or "." not in token:
        return None
    body, _, sig = token.partition(".")
    expected = _b64e(hmac.new(_signing_key(), body.encode("ascii"), hashlib.sha256).digest())
    if not hmac.compare_digest(sig, expected):
        return None
    try:
        payload = json.loads(_b64d(body).decode("utf-8"))
    except (ValueError, json.JSONDecodeError):
        return None
    if not allow_expired and int(payload.get("exp", 0)) < int(time.time()):
        return None
    if not payload.get("ws") or not payload.get("email"):
        return None
    return payload


def unsubscribe_url(workspace_id: str, email: str, sequence_id: str = "") -> str:
    token = make_unsubscribe_token(workspace_id, email, sequence_id)
    base = (settings.OUTREACH_PUBLIC_BASE_URL or "").rstrip("/")
    return f"{base}/api/outreach/unsubscribe?token={token}"
