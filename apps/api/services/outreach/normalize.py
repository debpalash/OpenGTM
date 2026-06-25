"""Canonical email normalization (spec §6.4.1).

Applied IDENTICALLY at enroll, send, suppress, webhook and unsubscribe so the
same physical recipient always resolves to the same suppression key.

- Trim, NFKC normalize, lowercase the whole address.
- IDNA-encode (punycode) + lowercase the domain.
- Do NOT strip Gmail dots/plus by default (over-normalizing risks suppressing
  the wrong person). Instead store the lowercased canonical form and, for
  gmail.com/googlemail.com ONLY, additionally compute a dot/plus-collapsed
  variant as a secondary suppression match key.
"""

from __future__ import annotations

import unicodedata
from typing import List

_GMAIL_DOMAINS = {"gmail.com", "googlemail.com"}


def normalize_email(email: str) -> str:
    """Return the canonical normalized form, or "" if not a parseable address."""
    if not email:
        return ""
    e = unicodedata.normalize("NFKC", str(email)).strip().lower()
    if "@" not in e:
        return e  # not an address; return the trimmed/lowered form
    local, _, domain = e.rpartition("@")
    if not local or not domain:
        return e
    try:
        domain = domain.encode("idna").decode("ascii").lower()
    except (UnicodeError, ValueError):
        domain = domain.lower()
    return f"{local}@{domain}"


def _gmail_collapsed(canonical: str) -> str:
    """For Gmail domains: strip dots in local-part and drop +tag. "" otherwise."""
    if "@" not in canonical:
        return ""
    local, _, domain = canonical.rpartition("@")
    if domain not in _GMAIL_DOMAINS:
        return ""
    local = local.split("+", 1)[0].replace(".", "")
    if not local:
        return ""
    return f"{local}@{domain}"


def suppression_match_keys(email: str) -> List[str]:
    """All keys a suppression lookup must check for ``email``.

    Always the exact canonical form; for Gmail domains also the dot/plus-
    collapsed variant. Non-Gmail addresses match ONLY on the exact canonical
    form (RFC-safe).
    """
    canonical = normalize_email(email)
    if not canonical:
        return []
    keys = [canonical]
    collapsed = _gmail_collapsed(canonical)
    if collapsed and collapsed != canonical:
        keys.append(collapsed)
    return keys
