"""
Light output normalization for enrichment fields.

Deliberately dependency-free: `phonenumbers` is NOT a project dependency, so
phone normalization is a cheap regex pass — E.164 when the input already
carries an international prefix (`+` or `00`), separator-stripping otherwise.
Never raises; unparseable input is returned unchanged.
"""

import re
from typing import Any, Callable, Dict

# E.164: "+" then up to 15 digits.
_E164_MAX_DIGITS = 15
_NON_DIGIT_RE = re.compile(r"[^\d]")


def normalize_phone(value: Any) -> Any:
    """Best-effort phone normalization (E.164 if cheaply possible).

    - "+1 (415) 555-0132"  -> "+14155550132"
    - "0044 20 7946 0958"  -> "+442079460958"   (00 international prefix)
    - "415.555.0132"       -> "4155550132"      (no country info — just cleaned)
    - non-strings / garbage pass through unchanged.
    """
    if not isinstance(value, str):
        return value
    s = value.strip()
    if not s:
        return value

    had_plus = s.startswith("+")
    digits = _NON_DIGIT_RE.sub("", s)
    if not digits or len(digits) > _E164_MAX_DIGITS + 2:
        return value  # not phone-shaped; leave untouched

    if had_plus:
        return f"+{digits[:_E164_MAX_DIGITS]}"
    if digits.startswith("00") and len(digits) > 8:
        # "00" international dialing prefix -> "+"
        return f"+{digits[2:2 + _E164_MAX_DIGITS]}"
    return digits


# field name -> normalizer. Applied by the declarative compiler after response
# projection (built-in providers may call these directly).
FIELD_NORMALIZERS: Dict[str, Callable[[Any], Any]] = {
    "phone": normalize_phone,
    "mobile_phone": normalize_phone,
}


def normalize_fields(fields: Dict[str, Any]) -> Dict[str, Any]:
    """Apply per-field normalizers; unknown fields pass through unchanged."""
    if not fields:
        return fields
    out = dict(fields)
    for name, fn in FIELD_NORMALIZERS.items():
        if name in out:
            try:
                out[name] = fn(out[name])
            except Exception:  # normalization must never break enrichment
                pass
    return out
