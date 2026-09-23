"""Exact v2 cell selection shared by queued execution paths."""


def uses_legacy_leads(workbook) -> bool:
    return workbook.source_type == "leads_filter" and (workbook.source_config or {}).get("row_storage_version") != 2


def mark_row_storage(workbook) -> None:
    workbook.source_config = {**(workbook.source_config or {}), "row_storage_version": 2}


def selected_cell_count(rows, columns, row_columns=None):
    """Summarize known stored scope without inventing counts for old payloads."""
    if not isinstance(rows, list) or not isinstance(columns, list):
        return None
    if any(type(row) is not int or row <= 0 for row in rows) or any(not isinstance(col, str) or not col for col in columns):
        return None
    if row_columns is None:
        return len(set(rows)) * len(set(columns))
    try:
        restrict_work_items([], row_columns)
        if set(row_columns) != {str(row) for row in rows}:
            return None
        if any(set(ids) - set(columns) for ids in row_columns.values()):
            return None
        return sum(len(set(ids)) for ids in row_columns.values())
    except (ValueError, TypeError):
        return None


def allows_automatic_retry(column: dict) -> bool:
    if column.get("type") == "output":
        return False
    if column.get("type") == "http":
        return str(column.get("http_method") or "GET").upper() in {"GET", "HEAD", "OPTIONS"}
    return True


def row_execution_data(row, columns: list[dict] | None = None) -> dict:
    """Hydrate saved dependencies, then assert database execution identities."""
    data = dict(row.data or {})
    values, aliases = {}, {}
    for column in columns or []:
        cid = column.get("id")
        cell = (row.enrichments or {}).get(cid)
        if isinstance(cell, dict) and (cell.get("status") != "complete" or cell.get("value") is None):
            # Imported/previously materialized row data can also contain an old
            # computed value. A failed snapshot must invalidate that copy, not
            # merely refrain from overlaying the failed enrichment value.
            data.pop(cid, None)
            if column.get("name"):
                data.pop(column["name"], None)
        if isinstance(cell, dict) and cell.get("status") == "complete" and cell.get("value") is not None:
            values[cid] = cell["value"]
            if column.get("name"):
                aliases[column["name"]] = cell["value"]
    # Stable IDs take precedence over display aliases. Failed/deleted outputs
    # must not reappear merely because an old value remains in stored evidence.
    return {**data, **aliases, **values, "id": row.lead_id or row.id,
            "__row_id": row.id, "__lead_id": row.lead_id}


def restrict_work_items(work_items: list, row_columns: dict | None) -> list:
    """Intersect planned cells with an explicit row-ID/column-ID allowlist.

    Missing rows and empty lists mean no cells, never all cells. Reject malformed
    durable payloads rather than interpreting them as unrestricted execution.
    """
    if row_columns is None:
        return work_items
    if not isinstance(row_columns, dict):
        raise ValueError("Invalid per-row column scope")
    scope = {}
    for row_id, column_ids in row_columns.items():
        key = str(row_id)
        if not key.isdecimal() or int(key) <= 0 or str(int(key)) != key:
            raise ValueError("Invalid row identity in cell scope")
        if not isinstance(column_ids, list) or any(not isinstance(c, str) or not c for c in column_ids):
            raise ValueError("Invalid column identities in cell scope")
        if key in scope:
            raise ValueError("Duplicate row identity in cell scope")
        scope[key] = set(column_ids)
    selected = []
    for lead, columns in work_items:
        row_id = lead.get("__row_id")
        if row_id is None:
            raise ValueError("Per-row column scope requires workbook row identities")
        allowed = scope.get(str(row_id), set())
        retained = [column for column in columns if column.get("id") in allowed]
        if retained:
            selected.append((lead, retained))
    return selected
