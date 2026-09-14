"""Shared, workspace-scoped signal store over the ORM ``signals`` table.

This is the single canonical write+emit path for buying signals on BOTH backends:

  * **Postgres** — the shared, RLS-protected ``signals`` table. Tenancy is
    enforced by the ``workspace_id`` belt filter (this module) AND the RLS GUC
    suspenders (``database.py`` ``after_begin`` hook).
  * **SQLite / self-host** — the SAME ORM ``signals`` table (created
    unconditionally by the tenancy migration). RLS is inert here, so the
    ``workspace_id`` belt filter is the ONLY isolation — it is therefore applied
    on every read and stamped on every write, never optional.

``add_signal`` is idempotent on the deterministic ``signals.id`` (``s.get`` then
insert-only-when-absent) and fires ``on_signal`` automations exactly once via
``emit_signal_matches`` — only on the inserted path, inside the same transaction.
Re-writing the same id (a re-scan / re-poll of the same underlying event) is a
no-op and does NOT re-fire. ``PgLeadStore.add_signal`` and the legacy signal
scanner both delegate here so PG and SQLite share one code path.
"""

from __future__ import annotations

import base64
import binascii
import json
import logging
from typing import Dict, List, Optional

from apps.api.database import SessionLocal
from apps.api.services.leadgen.orm_models import SignalRow
from sqlalchemy import and_, func, or_
from sqlalchemy.exc import IntegrityError

logger = logging.getLogger("signals.store")


class SignalStore:
    """Tenant-scoped read/write surface over the ORM ``signals`` table.

    Every query is filtered by ``workspace_id`` (belt); on Postgres RLS enforces
    it again at the DB (suspenders). Opens a fresh session per operation and
    binds the workspace into the ``current_workspace`` contextvar first so the
    SQLAlchemy ``after_begin`` hook sets the RLS GUC before any txn begins.
    """

    def __init__(self, workspace_id: str):
        if not workspace_id:
            raise ValueError("SignalStore requires a non-empty workspace_id")
        self.workspace_id = workspace_id

    # ── session helper ──
    def _session(self):
        from apps.api.core.tenancy import current_workspace_var

        current_workspace_var.set(self.workspace_id)
        return SessionLocal()

    # ── canonical write + emit ──
    def add_signal(self, signal) -> str:
        """Idempotent write of ``signal`` + exactly-once ``on_signal`` emit.

        ``signal`` is anything with the :class:`~apps.api.services.signals.monitor.Signal`
        attribute surface (``id``, ``lead_id``, ``company``, ``signal_type`` …).
        The row's ``workspace_id`` is ALWAYS force-stamped to this store's
        workspace (never trusted from the dataclass) so a write can never land in
        another tenant's partition. Emit runs only when a row is actually
        inserted, in the SAME transaction, and never breaks the write path.
        """
        inserted = False
        with self._session() as s, s.begin():
            exists = s.get(SignalRow, signal.id)
            if exists is None:
                try:
                    # A concurrent poller may win after the read above. Keep
                    # that expected uniqueness race inside a savepoint so the
                    # outer transaction remains usable and does not fail the
                    # whole poll attempt.
                    with s.begin_nested():
                        s.add(
                            SignalRow(
                                id=signal.id,
                                workspace_id=self.workspace_id,
                                lead_id=signal.lead_id,
                                company=signal.company,
                                signal_type=signal.signal_type,
                                title=signal.title,
                                description=signal.description,
                                source=signal.source,
                                source_url=signal.source_url,
                                weight=signal.weight,
                                created_at=signal.created_at,
                                read=False,
                            )
                        )
                        s.flush()
                    inserted = True
                except IntegrityError:
                    # Only suppress the deterministic-ID race. If the row is
                    # not visible to this tenant, the collision is unexpected
                    # (or RLS-hidden) and must fail closed.
                    s.expire_all()
                    existing = s.get(SignalRow, signal.id)
                    if existing is None or existing.workspace_id != self.workspace_id:
                        raise
            # Automations (§3.4): fire on_signal rules for the matched lead's
            # workbook rows. fire_key="signal:<pk>" → idempotent across
            # re-inserts. No-op when AUTOMATIONS_ENABLED is off. Inside the same
            # (RLS-scoped on PG) transaction.
            if inserted:
                try:
                    from apps.api.services.automations import events as _auto_events

                    _auto_events.emit_signal_matches(
                        s, self.workspace_id,
                        [{"signal_pk": signal.id, "signal_type": signal.signal_type,
                          "lead_id": signal.lead_id}],
                    )
                except Exception as _e:  # never break the signal write path
                    logger.warning("automations signal emit failed: %s", _e)
        return signal.id

    # ── reads (tenant-scoped) ──
    def get_signals(
        self,
        signal_type: Optional[str] = None,
        lead_id: Optional[int] = None,
        limit: int = 50,
        offset: int = 0,
    ) -> List[dict]:
        with self._session() as s:
            q = s.query(SignalRow).filter(SignalRow.workspace_id == self.workspace_id)
            if signal_type:
                q = q.filter(SignalRow.signal_type == signal_type)
            if lead_id is not None:
                q = q.filter(SignalRow.lead_id == lead_id)
            rows = (
                q.order_by(SignalRow.created_at.desc(), SignalRow.id.desc())
                .limit(limit)
                .offset(offset)
                .all()
            )
            return [self._signal_to_dict(r) for r in rows]

    def get_signals_page(
        self,
        *,
        signal_types: Optional[List[str]] = None,
        lead_id: Optional[int] = None,
        lead_ids: Optional[List[int]] = None,
        companies: Optional[List[str]] = None,
        limit: int = 50,
        cursor: Optional[str] = None,
        offset: int = 0,
    ) -> dict:
        """Return one stable keyset-paginated signal page."""
        if limit < 1:
            raise ValueError("limit must be positive")
        with self._session() as s:
            q = s.query(SignalRow).filter(SignalRow.workspace_id == self.workspace_id)
            if signal_types:
                q = q.filter(SignalRow.signal_type.in_(signal_types))
            if lead_id is not None:
                q = q.filter(SignalRow.lead_id == lead_id)
            if lead_ids is not None or companies is not None:
                identity_filters = []
                if lead_ids:
                    identity_filters.append(SignalRow.lead_id.in_(lead_ids))
                normalized_companies = sorted({value.strip().casefold() for value in companies or [] if value.strip()})
                if normalized_companies:
                    identity_filters.append(func.lower(SignalRow.company).in_(normalized_companies))
                if not identity_filters:
                    return {"signals": [], "next_cursor": None, "has_more": False}
                q = q.filter(or_(*identity_filters))
            if cursor:
                created_at, signal_id = self._decode_cursor(cursor)
                q = q.filter(or_(
                    SignalRow.created_at < created_at,
                    and_(SignalRow.created_at == created_at, SignalRow.id < signal_id),
                ))
            query = q.order_by(SignalRow.created_at.desc(), SignalRow.id.desc()).limit(limit + 1)
            if offset and not cursor:
                query = query.offset(offset)
            rows = query.all()
            has_more = len(rows) > limit
            page_rows = rows[:limit]
            return {
                "signals": [self._signal_to_dict(row) for row in page_rows],
                "next_cursor": self._encode_cursor(page_rows[-1]) if has_more else None,
                "has_more": has_more,
            }

    @staticmethod
    def _encode_cursor(row: SignalRow) -> str:
        return SignalStore._encode_cursor_values(row.created_at, row.id)

    @staticmethod
    def encode_cursor_from_dict(row: dict) -> str:
        return SignalStore._encode_cursor_values(row["created_at"], row["id"])

    @staticmethod
    def _encode_cursor_values(created_at: float, signal_id: str) -> str:
        payload = json.dumps([created_at, signal_id], separators=(",", ":")).encode()
        return base64.urlsafe_b64encode(payload).decode().rstrip("=")

    @staticmethod
    def _decode_cursor(cursor: str) -> tuple[float, str]:
        try:
            padding = "=" * (-len(cursor) % 4)
            value = json.loads(base64.urlsafe_b64decode(cursor + padding))
            if not isinstance(value, list) or len(value) != 2 or not isinstance(value[1], str):
                raise ValueError
            return float(value[0]), value[1]
        except (ValueError, TypeError, UnicodeDecodeError, json.JSONDecodeError, binascii.Error) as exc:
            raise ValueError("invalid signal cursor") from exc

    def get_signal_counts(self) -> Dict[str, int]:
        from sqlalchemy import func

        with self._session() as s:
            rows = (
                s.query(SignalRow.signal_type, func.count())
                .filter(SignalRow.workspace_id == self.workspace_id)
                .group_by(SignalRow.signal_type)
                .all()
            )
            result = {k: v for k, v in rows}
            result["total"] = (
                s.query(SignalRow)
                .filter(SignalRow.workspace_id == self.workspace_id)
                .count()
            )
            result["unread"] = (
                s.query(SignalRow)
                .filter(
                    SignalRow.workspace_id == self.workspace_id,
                    SignalRow.read.is_(False),
                )
                .count()
            )
            return result

    def mark_signals_read(self, signal_ids: List[str]) -> None:
        if not signal_ids:
            return
        with self._session() as s, s.begin():
            s.query(SignalRow).filter(
                SignalRow.workspace_id == self.workspace_id,
                SignalRow.id.in_(signal_ids),
            ).update({"read": True}, synchronize_session=False)

    @staticmethod
    def _signal_to_dict(row: SignalRow) -> dict:
        return {
            "id": row.id,
            "workspace_id": row.workspace_id,
            "lead_id": row.lead_id,
            "company": row.company,
            "signal_type": row.signal_type,
            "title": row.title,
            "description": row.description,
            "source": row.source,
            "source_url": row.source_url,
            "weight": row.weight,
            "created_at": row.created_at,
            "read": 1 if row.read else 0,
        }


def get_signal_store(workspace_id: str) -> SignalStore:
    """Return the workspace-scoped ORM signal store.

    Also publishes ``workspace_id`` into the ``current_workspace`` contextvar so
    the SQLAlchemy session hook sets the RLS GUC for any Postgres transaction.
    Works identically on SQLite (RLS inert; belt filter is the isolation).
    """
    from apps.api.core.tenancy import current_workspace_var

    if workspace_id:
        current_workspace_var.set(workspace_id)
    return SignalStore(workspace_id)
