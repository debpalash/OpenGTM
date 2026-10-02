"""How an upsert merges into the lead it matches.

A lead is matched by workspace, company and city. Replacing the matched row with
the incoming record destroyed data in two ways. Every field the incoming record
did not mention was reset to its default, so a collection re-run or a partial
API call blanked the website, phone, status, score and notes that enrichment and
people had already put there. And a different person's name and email silently
took over the lead's single contact slot.

Both lead stores now merge instead:

* A field the caller did not provide, meaning empty or still at its dataclass
  default, never overwrites a stored value. Provided values still overwrite, so
  newer data wins. Clearing a field is an explicit update, not an upsert.
* A lead holds one contact. When the incoming record names a different person
  than the stored contact, the stored contact is kept. Callers that can report
  the collision (the REST API and MCP) ask for ``reject`` and get
  :class:`LeadContactConflict` before anything is written. Bulk paths keep the
  stored contact and carry on.
"""

from __future__ import annotations

from dataclasses import fields as dataclass_fields
from typing import Any, Dict, NamedTuple, Tuple

from apps.api.services.leadgen.models import Lead
from apps.api.services.person_names import name_tokens, names_compatible

CONFLICT_KEEP = "keep"
CONFLICT_REJECT = "reject"

# Describe the lead's one contact rather than the company. A different contact
# contributes none of them to a lead that already has one.
CONTACT_FIELDS = frozenset({
    "contact_person", "contact_title", "email", "email_confidence", "email_provider",
    "phone", "phone_provider", "secondary_emails", "secondary_phones",
})
# Describe one particular email or phone, so they follow its value.
_EMAIL_BOUND = ("email_confidence", "email_provider")
_PHONE_BOUND = ("phone_provider",)
# The match key, tenancy and timestamps are never merged.
_NOT_MERGED = frozenset({"id", "company", "city", "workspace_id", "created_at", "updated_at"})
# What pipelines write when a value is unknown (see Lead.has_website and friends).
_MISSING = frozenset({"", "N/A", "nan"})
_DEFAULTS = {f.name: f.default for f in dataclass_fields(Lead)}


class LeadContactConflict(Exception):
    """An upsert named a different contact than the one stored on the matching lead."""

    def __init__(self, lead_id: int | None, fields: Tuple[str, ...]):
        self.lead_id = lead_id
        self.fields = tuple(fields)
        super().__init__(
            f"lead {lead_id} already has a different contact ({', '.join(self.fields)})"
        )


class MergeResult(NamedTuple):
    changes: Dict[str, Any]   # column -> value to write on the existing lead
    contact_conflict: bool    # the incoming contact was ignored


def _provided(name: str, value: Any) -> bool:
    if value is None:
        return False
    if isinstance(value, str) and value.strip() in _MISSING:
        return False
    return value != _DEFAULTS.get(name)


def _email(value: Any) -> str:
    text = str(value or "").strip().lower()
    return text if "@" in text else ""


def contact_conflict_fields(existing: Lead, incoming: Lead) -> Tuple[str, ...]:
    """Identity fields on which the contacts differ, or ``()`` if they can be one person.

    Names decide when both leads have one. Without two names a different email
    decides. A same-name contact with a new email is the same person.
    """
    if existing.has_contact_person and incoming.has_contact_person:
        if names_compatible(existing.contact_person, incoming.contact_person):
            return ()
        return ("contact_person",)
    old, new = _email(existing.email), _email(incoming.email)
    if old and new and old != new:
        return ("email",)
    return ()


def merge_lead(existing: Lead, incoming: Lead, *, on_contact_conflict: str = CONFLICT_KEEP) -> MergeResult:
    """Return the column changes that fold ``incoming`` into ``existing``.

    Raises :class:`LeadContactConflict` for a different contact when
    ``on_contact_conflict`` is ``"reject"``. Nothing is written by this function.
    """
    conflict = contact_conflict_fields(existing, incoming)
    if conflict and on_contact_conflict == CONFLICT_REJECT:
        raise LeadContactConflict(existing.id, conflict)

    changes: Dict[str, Any] = {}
    for spec in dataclass_fields(Lead):
        name = spec.name
        if name in _NOT_MERGED or (conflict and name in CONTACT_FIELDS):
            continue
        value = getattr(incoming, name)
        if not _provided(name, value) or value == getattr(existing, name):
            continue
        if name == "contact_person" and existing.has_contact_person:
            # Same person: keep the fuller form ("Alice Smith" over "Alice").
            if len(name_tokens(value)) <= len(name_tokens(existing.contact_person)):
                continue
        changes[name] = value

    # Provenance of an email or phone describes that value only. When the value
    # changes, the incoming record's provenance (possibly none) replaces it.
    for key, bound in (("email", _EMAIL_BOUND), ("phone", _PHONE_BOUND)):
        if key in changes:
            for name in bound:
                value = getattr(incoming, name)
                if value != getattr(existing, name):
                    changes[name] = value
    return MergeResult(changes, bool(conflict))
