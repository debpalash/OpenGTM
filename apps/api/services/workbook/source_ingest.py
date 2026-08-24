"""Transactional source-record upserts for workbook connectors."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable

from sqlalchemy import func, text
from sqlalchemy.orm import Session

from apps.api.services.connectors.contracts import ConnectorRecord
from apps.api.services.workbook.models import WorkbookRow


@dataclass(frozen=True)
class SourceIngestSummary:
    added: int
    updated: int
    skipped: int
    added_row_ids: tuple[int, ...]


def lock_source_ingest(db: Session, workbook_id: str, provider: str) -> None:
    """Serialize one provider's page commits for a workbook on PostgreSQL."""
    if db.get_bind().dialect.name == "postgresql":
        db.execute(
            text("SELECT pg_advisory_xact_lock(hashtext(:lock_key))"),
            {"lock_key": f"source_ingest:{workbook_id}:{provider}"},
        )


def upsert_source_records(
    db: Session,
    *,
    workbook_id: str,
    workspace_id: str,
    provider: str,
    records: Iterable[ConnectorRecord],
    reset_rows: bool = False,
) -> SourceIngestSummary:
    """Upsert a connector page using its stable source identity.

    The caller owns the transaction so row changes and cursor/checkpoint updates
    commit atomically. A crash can therefore never persist a page while leaving
    its cursor behind.
    """
    lock_source_ingest(db, workbook_id, provider)
    if reset_rows:
        db.query(WorkbookRow).filter(
            WorkbookRow.workbook_id == workbook_id
        ).delete(synchronize_session=False)
        db.flush()

    existing_rows = db.query(WorkbookRow).filter(
        WorkbookRow.workbook_id == workbook_id,
        WorkbookRow.source_provider == provider,
        WorkbookRow.source_record_id.isnot(None),
    ).all()
    existing = {str(row.source_record_id): row for row in existing_rows}
    max_position = db.query(func.max(WorkbookRow.position)).filter(
        WorkbookRow.workbook_id == workbook_id
    ).scalar()
    next_position = int(max_position) + 1 if max_position is not None else 0

    added = updated = skipped = 0
    added_row_ids: list[int] = []
    seen: set[str] = set()
    for record in records:
        record_id = str(record.record_id or "").strip()
        if not record_id or record.provider != provider or record_id in seen:
            skipped += 1
            continue
        seen.add(record_id)
        data = dict(record.data)
        data["_source"] = {
            "provider": provider,
            "record_id": record_id,
            "rank": record.rank,
            "url": record.source_url,
            "fetched_at": record.fetched_at.isoformat(),
        }
        row = existing.get(record_id)
        if row is None:
            position = record.rank if reset_rows else next_position
            next_position += 1
            row = WorkbookRow(
                workbook_id=workbook_id,
                workspace_id=workspace_id,
                position=position,
                data=data,
                enrichments={},
                lead_id=None,
                source_provider=provider,
                source_record_id=record_id,
                source_rank=record.rank,
                source_fetched_at=record.fetched_at,
            )
            db.add(row)
            db.flush()
            existing[record_id] = row
            added += 1
            added_row_ids.append(row.id)
            continue

        merged = {**(row.data or {}), **data}
        changed = (
            merged != (row.data or {})
            or row.source_rank != record.rank
            or row.source_fetched_at != record.fetched_at
        )
        if changed:
            row.data = merged
            row.source_rank = record.rank
            row.source_fetched_at = record.fetched_at
            if reset_rows:
                row.position = record.rank
            updated += 1
        else:
            skipped += 1

    return SourceIngestSummary(
        added=added,
        updated=updated,
        skipped=skipped,
        added_row_ids=tuple(added_row_ids),
    )
