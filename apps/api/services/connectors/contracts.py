"""Typed contracts shared by external data connectors.

Connectors must distinguish a complete source page from a target-satisfying
collection and from a partial result. Returning an unannotated ``list[dict]``
makes upstream failures indistinguishable from a genuinely small result set.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Iterable, Mapping, Optional


def utcnow() -> datetime:
    return datetime.now(timezone.utc)


@dataclass(frozen=True)
class ConnectorRecord:
    provider: str
    record_id: str
    data: Mapping[str, Any]
    rank: int
    source_url: Optional[str] = None
    fetched_at: datetime = field(default_factory=utcnow)

    def as_dict(self) -> dict[str, Any]:
        return dict(self.data)


@dataclass(frozen=True)
class ConnectorPage:
    provider: str
    records: tuple[ConnectorRecord, ...]
    page: int
    page_size: int
    source_total: Optional[int]
    total_pages: Optional[int]
    has_more: bool

    @classmethod
    def from_records(
        cls,
        *,
        provider: str,
        records: Iterable[ConnectorRecord],
        page: int,
        page_size: int,
        source_total: Optional[int] = None,
        total_pages: Optional[int] = None,
    ) -> "ConnectorPage":
        materialized = tuple(records)
        if total_pages is not None:
            has_more = page < total_pages
        else:
            has_more = len(materialized) >= page_size
        return cls(
            provider=provider,
            records=materialized,
            page=page,
            page_size=page_size,
            source_total=source_total,
            total_pages=total_pages,
            has_more=has_more,
        )


@dataclass(frozen=True)
class ConnectorCollection:
    provider: str
    records: tuple[ConnectorRecord, ...]
    requested_count: int
    pages_fetched: int
    next_page: int
    source_total: Optional[int]
    target_met: bool
    exhausted: bool
    max_pages_reached: bool = False
    warnings: tuple[str, ...] = ()

    @property
    def partial(self) -> bool:
        return not self.target_met

    @property
    def status(self) -> str:
        return "complete" if self.target_met else "partial"

    def summary(self, *, include_records: bool = True) -> dict[str, Any]:
        result: dict[str, Any] = {
            "provider": self.provider,
            "requested_count": self.requested_count,
            "returned_count": len(self.records),
            "pages_fetched": self.pages_fetched,
            "next_page": self.next_page,
            "source_total": self.source_total,
            "target_met": self.target_met,
            "exhausted": self.exhausted,
            "partial": self.partial,
            "status": self.status,
            "max_pages_reached": self.max_pages_reached,
            "warnings": list(self.warnings),
        }
        if include_records:
            result["records"] = [record.as_dict() for record in self.records]
        return result
