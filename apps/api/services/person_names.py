"""Tolerant comparison of person names for identity decisions.

Two records are only treated as different people on name evidence when the names
clearly disagree. Case, accents, punctuation, word order and honorifics do not
count as disagreement, a short name matches a longer one that contains it
("Alice" and "Alice Smith"), and an initial matches the name it abbreviates
("A. Smith" and "Alice Smith"). A blank or placeholder name is compatible with
anything because it carries no evidence.
"""

from __future__ import annotations

import re
import unicodedata

_TOKEN = re.compile(r"[^\W_]+")
# Values pipelines write when a name is unknown (see Lead.has_contact_person).
_PLACEHOLDERS = frozenset({("unknown",), ("n", "a"), ("nan",), ("none",)})


def name_tokens(name) -> tuple[str, ...]:
    """Lower-cased, accent-free word tokens of ``name``; ``()`` when blank or a placeholder."""
    text = unicodedata.normalize("NFKD", str(name or ""))
    text = "".join(ch for ch in text if not unicodedata.combining(ch)).casefold()
    tokens = tuple(_TOKEN.findall(text))
    return () if tokens in _PLACEHOLDERS else tokens


def _token_match(a: str, b: str) -> bool:
    return a == b or (len(a) == 1 and b.startswith(a)) or (len(b) == 1 and a.startswith(b))


def names_compatible(first, second) -> bool:
    """True unless the two names clearly describe different people."""
    a, b = name_tokens(first), name_tokens(second)
    if not a or not b:
        return True
    small, large = (a, b) if len(a) <= len(b) else (b, a)
    return all(any(_token_match(token, other) for other in large) for token in small)
