"""Schema-aware CSV imports for self-contained workbooks.

CSV exports from Clay and other GTM tools contain a mix of familiar lead
fields and table-specific columns.  This module maps familiar headers onto the
OpenGTM lead schema and turns every other header into an editable workbook
column, so an import never silently drops data.
"""

from __future__ import annotations

import re
from typing import Any, Iterable, Optional

from apps.api.services.workbook.models import LEAD_FIELD_MAP


_HEADER_ALIASES = {
    "company": "company",
    "company name": "company",
    "organization": "company",
    "organization name": "company",
    "account name": "company",
    "domain": "website",
    "company domain": "website",
    "website": "website",
    "website url": "website",
    "company website": "website",
    "email": "email",
    "email address": "email",
    "work email": "email",
    "business email": "email",
    "phone": "phone",
    "phone number": "phone",
    "mobile": "phone",
    "mobile phone": "phone",
    "contact": "contact_person",
    "contact name": "contact_person",
    "contact person": "contact_person",
    "full name": "contact_person",
    "person name": "contact_person",
    "title": "contact_title",
    "job title": "contact_title",
    "contact title": "contact_title",
    "linkedin": "linkedin_url",
    "linkedin url": "linkedin_url",
    "person linkedin": "linkedin_url",
    "person linkedin url": "linkedin_url",
    "twitter": "twitter_url",
    "twitter url": "twitter_url",
    "city": "city",
    "state": "state",
    "region": "state",
    "address": "address",
    "industry": "specialization",
    "specialization": "specialization",
    "company size": "company_size",
    "employee count": "company_size",
    "employees": "company_size",
    "description": "description",
    "company description": "description",
    "notes": "notes",
    "source": "source",
    "status": "status",
    "score": "score",
}


def normalize_header(value: Any) -> str:
    """Normalize a header for alias and existing-column comparisons."""
    return re.sub(r"\s+", " ", re.sub(r"[^a-z0-9]+", " ", str(value).lower())).strip()


def column_key(value: Any) -> str:
    """Create a JSON-safe, formula-friendly key for a custom CSV column."""
    key = re.sub(r"[^a-z0-9]+", "_", str(value).lower()).strip("_")
    if not key:
        key = "imported_column"
    if key[0].isdigit():
        key = f"column_{key}"
    return key[:100]


def infer_target(header: str) -> str:
    """Return a canonical lead field or a stable custom-column key."""
    normalized = normalize_header(header)
    if normalized in _HEADER_ALIASES:
        return _HEADER_ALIASES[normalized]
    underscored = normalized.replace(" ", "_")
    if underscored in LEAD_FIELD_MAP:
        return underscored
    return column_key(header)


def _headers(rows: Iterable[dict]) -> list[str]:
    ordered: list[str] = []
    seen: set[str] = set()
    for row in rows:
        for raw in row:
            header = str(raw).strip()
            if header and header not in seen:
                seen.add(header)
                ordered.append(header)
    return ordered


def prepare_csv_import(
    rows: list[dict],
    columns_config: list[dict],
    mapping: Optional[dict[str, Optional[str]]] = None,
    *,
    create_columns: bool = True,
) -> tuple[list[dict], list[dict], list[dict], dict[str, Optional[str]]]:
    """Normalize rows and extend workbook columns without losing CSV fields.

    ``mapping`` maps source headers to target field names. A null/empty target
    skips that source field. Missing mapping entries are inferred.
    """
    mapping = mapping or {}
    columns = [dict(col) for col in (columns_config or [])]
    added_columns: list[dict] = []

    # Match imports to existing columns by display name, id, or backing field.
    existing_by_name: dict[str, dict] = {}
    used_ids: set[str] = set()
    used_fields: set[str] = set()
    for col in columns:
        cid = str(col.get("id") or "")
        field = str(col.get("lead_field") or cid)
        if cid:
            used_ids.add(cid)
        # Imported source values can only populate editable/input columns.
        # An AI/enrichment column with the same display name reads from the
        # enrichment overlay and must not swallow an imported CSV field.
        if col.get("type") in ("lead_field", "input"):
            if cid:
                existing_by_name.setdefault(normalize_header(cid), col)
            if field:
                used_fields.add(field)
                existing_by_name.setdefault(normalize_header(field), col)
            if col.get("name"):
                existing_by_name.setdefault(normalize_header(col["name"]), col)

    resolved: dict[str, Optional[str]] = {}
    for header in _headers(rows):
        explicit = header in mapping
        requested = mapping.get(header) if explicit else None
        if explicit and (requested is None or not str(requested).strip()):
            resolved[header] = None
            continue

        requested_text = str(requested).strip() if explicit else ""
        existing = existing_by_name.get(normalize_header(requested_text or header))
        if existing:
            resolved[header] = str(existing.get("lead_field") or existing["id"])
            continue

        target = infer_target(requested_text or header)
        resolved[header] = target
        if not create_columns or target in used_fields:
            continue

        col_id = target
        suffix = 2
        while col_id in used_ids:
            col_id = f"{target}_{suffix}"
            suffix += 1
        display_name = (
            header if not requested_text or target not in LEAD_FIELD_MAP
            else requested_text.replace("_", " ").title()
        )[:255]
        new_col = {
            "id": col_id,
            "name": display_name,
            "type": "lead_field",
            "lead_field": target,
            "width": 200,
        }
        columns.append(new_col)
        added_columns.append(new_col)
        used_ids.add(col_id)
        used_fields.add(target)
        existing_by_name[normalize_header(header)] = new_col

    normalized_rows: list[dict] = []
    for source_row in rows:
        normalized: dict[str, Any] = {}
        for raw_header, value in source_row.items():
            header = str(raw_header).strip()
            target = resolved.get(header)
            if not target:
                continue
            # Prefer a populated value when two source columns map to one field.
            if target not in normalized or normalized[target] in (None, ""):
                normalized[target] = value
        if any(value not in (None, "") for value in normalized.values()):
            normalized_rows.append(normalized)

    return normalized_rows, columns, added_columns, resolved
