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

import logging
from typing import Dict, List, Optional

from apps.api.database import SessionLocal
from apps.api.services.leadgen.orm_models import SignalRow

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
                inserted = True
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
            if lead_id:
                q = q.filter(SignalRow.lead_id == lead_id)
            rows = (
                q.order_by(SignalRow.created_at.desc())
                .limit(limit)
                .offset(offset)
                .all()
            )
            return [self._signal_to_dict(r) for r in rows]

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
