"""Tenant-scoped lead/signal store — backend-detected.

Two implementations behind one factory:

  * :class:`~apps.api.services.leadgen.db.LeadDB` — the legacy single-file
    SQLite store (FTS5, per-workspace files). Default on SQLite / self-host.
  * :class:`PgLeadStore` — the shared, multi-tenant Postgres tables (`leads`,
    `signals`) protected by Row-Level Security. Every read/write is filtered and
    stamped by ``workspace_id`` at the application layer; RLS is the enforced
    DB backstop (see migration c42d0273d9bd).

``get_lead_store(workspace_id, slug)`` picks the right one. On Postgres it also
verifies (once) that the connection role is NOT superuser/BYPASSRLS — otherwise
RLS is silently inert and the PG store would ship with isolation OFF.
"""

from __future__ import annotations

import logging
import threading
from typing import Any, Dict, List, Optional

from apps.api.core.config import settings
from apps.api.database import IS_SQLITE, SessionLocal
from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.orm_models import LeadRow, SignalRow

logger = logging.getLogger("leadgen.store")

# Memoised result of the role-safety check (per process). None = not yet checked.
_role_check_lock = threading.Lock()
_role_check_result: Optional[bool] = None


class RlsRoleError(RuntimeError):
    """Raised when the PG connection role would make RLS silently inert."""


def assert_rls_role(strict: bool = True) -> bool:
    """Verify the live PG role is NOT superuser and NOT BYPASSRLS.

    RLS policies are silently ignored for superusers and roles with the
    BYPASSRLS attribute, so a PG store running under such a role ships with
    tenant isolation OFF. We refuse to use the PG store in that case.

    Returns True when the role is safe. When ``strict`` and the role is unsafe,
    raises :class:`RlsRoleError`; otherwise logs CRITICAL and returns False.
    Memoised: the query runs once per process.
    """
    global _role_check_result
    if _role_check_result is not None:
        if not _role_check_result and strict:
            raise RlsRoleError(_UNSAFE_ROLE_MSG)
        return _role_check_result

    with _role_check_lock:
        if _role_check_result is not None:
            return _role_check_result
        from sqlalchemy import text

        with SessionLocal() as s:
            row = s.execute(
                text(
                    "SELECT current_user, rolsuper, rolbypassrls "
                    "FROM pg_roles WHERE rolname = current_user"
                )
            ).first()
        if row is None:
            safe = False
            detail = "could not resolve current_user in pg_roles"
        else:
            user, is_super, is_bypass = row
            safe = not (is_super or is_bypass)
            detail = (
                f"current_user={user!r} rolsuper={is_super} rolbypassrls={is_bypass}"
            )
        _role_check_result = safe
        if not safe:
            msg = _UNSAFE_ROLE_MSG + f" ({detail})"
            if strict:
                raise RlsRoleError(msg)
            logger.critical(msg)
        else:
            logger.info("RLS role check passed: %s", detail)
        return safe


_UNSAFE_ROLE_MSG = (
    "Refusing to use the Postgres lead store: the connection role is a "
    "superuser or has BYPASSRLS, which makes Row-Level Security silently "
    f"inert. Connect as a dedicated non-super role (e.g. {settings.APP_DB_ROLE}) "
    "with the table grants from the tenancy migration. Set PG_LEAD_STORE=false "
    "to force the legacy SQLite path, or PG_RLS_REQUIRE_SAFE_ROLE=false to "
    "downgrade this to a logged warning (NOT recommended)."
)


def use_pg_store() -> bool:
    """True when the shared RLS-protected Postgres store should be used."""
    if IS_SQLITE or not settings.PG_LEAD_STORE:
        return False
    # Verify the role; honour the fail-fast vs warn toggle.
    return assert_rls_role(strict=settings.PG_RLS_REQUIRE_SAFE_ROLE)


def get_lead_store(workspace_id: str, slug: str):
    """Return the tenant-scoped store for ``workspace_id``.

    Also publishes ``workspace_id`` into the ``current_workspace`` contextvar so
    the SQLAlchemy session hook sets the RLS GUC for Postgres transactions.
    """
    from apps.api.core.tenancy import current_workspace_var

    if workspace_id:
        current_workspace_var.set(workspace_id)

    if use_pg_store():
        return PgLeadStore(workspace_id)

    # Legacy SQLite path (or PG-store disabled): per-workspace file.
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.services.workspace import manager as ws_manager

    return LeadDB(ws_manager.workspace_leads_db_path(slug))


# ── Postgres store ───────────────────────────────────────────────────────────

# Columns on the leads ORM table (so we only persist known fields).
_LEAD_COLUMNS = {c.name for c in LeadRow.__table__.columns}
# Columns that feed the tsvector / ILIKE fallback search.
_SEARCH_COLUMNS = ("company", "city", "specialization", "notes", "description")

_ALLOWED_ORDERS = {
    "score DESC", "score ASC", "company ASC", "company DESC",
    "created_at DESC", "created_at ASC", "updated_at DESC",
    "city ASC", "city DESC", "status ASC",
}


class PgLeadStore:
    """Tenant-scoped lead/signal store over the shared Postgres tables.

    All queries are filtered by ``workspace_id`` (belt) and RLS enforces it
    again at the DB (suspenders). Mirrors the read/write surface of
    :class:`LeadDB` used by ``ctx.lead_db()`` consumers.
    """

    def __init__(self, workspace_id: str):
        if not workspace_id:
            raise ValueError("PgLeadStore requires a non-empty workspace_id")
        self.workspace_id = workspace_id
        # PgLeadStore opens a fresh session per operation; this attribute exists
        # so legacy `isinstance`/`.db_path` probes don't crash.
        self.db_path = None

    # ── session helper ──
    def _session(self):
        # SessionLocal's after_begin hook sets the RLS GUC from the contextvar;
        # make sure THIS workspace is bound before any txn begins.
        from apps.api.core.tenancy import current_workspace_var

        current_workspace_var.set(self.workspace_id)
        return SessionLocal()

    # ── row<->Lead mapping ──
    @staticmethod
    def _row_to_lead(row: LeadRow) -> Lead:
        data = {c: getattr(row, c) for c in _LEAD_COLUMNS if c != "search_tsv"}
        return Lead.from_dict(data)

    def _lead_payload(self, lead: Lead) -> Dict[str, Any]:
        d = lead.to_dict()
        d.pop("id", None)
        # Force tenancy: the row ALWAYS belongs to this store's workspace,
        # regardless of what the dataclass carried (prevents accidental
        # cross-tenant writes; RLS WITH CHECK would reject them anyway).
        d["workspace_id"] = self.workspace_id
        return {k: v for k, v in d.items() if k in _LEAD_COLUMNS}

    # ── CRUD ──
    def upsert_lead(self, lead: Lead) -> int:
        from apps.api.services.leadgen.db import _utcnow

        lead.updated_at = _utcnow().isoformat()
        with self._session() as s, s.begin():
            existing = (
                s.query(LeadRow)
                .filter(
                    LeadRow.workspace_id == self.workspace_id,
                    LeadRow.company == lead.company,
                    LeadRow.city == lead.city,
                )
                .first()
            )
            payload = self._lead_payload(lead)
            if existing:
                for k, v in payload.items():
                    if k == "created_at":
                        continue
                    setattr(existing, k, v)
                lead.id = existing.id
            else:
                row = LeadRow(**payload)
                s.add(row)
                s.flush()
                lead.id = row.id
        return lead.id

    def bulk_upsert(self, leads: List[Lead]) -> int:
        count = 0
        for lead in leads:
            self.upsert_lead(lead)
            count += 1
        return count

    def get_lead(self, lead_id: int) -> Optional[Lead]:
        with self._session() as s:
            row = (
                s.query(LeadRow)
                .filter(
                    LeadRow.workspace_id == self.workspace_id,
                    LeadRow.id == lead_id,
                )
                .first()
            )
            return self._row_to_lead(row) if row else None

    def get_leads(
        self,
        status: Optional[str] = None,
        city: Optional[str] = None,
        source: Optional[str] = None,
        score_min: Optional[int] = None,
        score_max: Optional[int] = None,
        score_tier: Optional[str] = None,
        search: Optional[str] = None,
        workspace_id: Optional[str] = None,  # accepted for signature parity; ignored
        has_email: Optional[bool] = None,
        has_phone: Optional[bool] = None,
        limit: int = 500,
        offset: int = 0,
        order_by: str = "score DESC",
    ) -> List[Lead]:
        from sqlalchemy import text

        with self._session() as s:
            q = s.query(LeadRow).filter(LeadRow.workspace_id == self.workspace_id)
            if has_email is True:
                q = q.filter(LeadRow.email.isnot(None), LeadRow.email != "")
            elif has_email is False:
                q = q.filter((LeadRow.email.is_(None)) | (LeadRow.email == ""))
            if has_phone is True:
                q = q.filter(LeadRow.phone.isnot(None), LeadRow.phone != "")
            elif has_phone is False:
                q = q.filter((LeadRow.phone.is_(None)) | (LeadRow.phone == ""))
            if status:
                q = q.filter(LeadRow.status == status)
            if city:
                q = q.filter(LeadRow.city == city)
            if source:
                q = q.filter(LeadRow.source == source)
            if score_min is not None:
                q = q.filter(LeadRow.score >= score_min)
            if score_max is not None:
                q = q.filter(LeadRow.score <= score_max)
            if score_tier:
                q = q.filter(LeadRow.score_tier == score_tier)
            if search:
                q = q.filter(self._search_clause(search))

            order = order_by if order_by in _ALLOWED_ORDERS else "score DESC"
            col, _, direction = order.partition(" ")
            order_col = getattr(LeadRow, col, LeadRow.score)
            q = q.order_by(order_col.desc() if direction == "DESC" else order_col.asc())
            rows = q.limit(limit).offset(offset).all()
            return [self._row_to_lead(r) for r in rows]

    @staticmethod
    def _search_clause(search: str):
        """Full-text search clause: tsvector @@ to_tsquery, ILIKE fallback.

        Uses websearch_to_tsquery (forgiving of arbitrary user input) against
        the trigger-maintained `search_tsv` GIN column. ORs an ILIKE over the
        searchable columns so short/partial tokens that tsquery would miss
        still match (parity with SQLite FTS prefix behaviour is approximate;
        the parity test pins representative queries).
        """
        from sqlalchemy import text, or_, func

        tsv = text(
            "search_tsv @@ websearch_to_tsquery('english', :q)"
        ).bindparams(q=search)
        like = f"%{search}%"
        ilike_clauses = [getattr(LeadRow, c).ilike(like) for c in _SEARCH_COLUMNS]
        return or_(tsv, *ilike_clauses)

    def count_leads(self, **filters) -> int:
        with self._session() as s:
            q = s.query(LeadRow).filter(LeadRow.workspace_id == self.workspace_id)
            for key, val in filters.items():
                if val is not None and hasattr(LeadRow, key):
                    q = q.filter(getattr(LeadRow, key) == val)
            return q.count()

    def update_status(self, lead_id: int, status: str, note: str = "") -> None:
        from apps.api.services.leadgen.db import _utcnow

        now = _utcnow().isoformat()
        with self._session() as s, s.begin():
            s.query(LeadRow).filter(
                LeadRow.workspace_id == self.workspace_id, LeadRow.id == lead_id
            ).update({"status": status, "updated_at": now})

    def update_lead_fields(self, lead_id: int, fields: Dict[str, Any]) -> None:
        from apps.api.services.leadgen.db import _utcnow

        fields = {k: v for k, v in fields.items() if k in _LEAD_COLUMNS}
        fields["updated_at"] = _utcnow().isoformat()
        with self._session() as s, s.begin():
            s.query(LeadRow).filter(
                LeadRow.workspace_id == self.workspace_id, LeadRow.id == lead_id
            ).update(fields)

    def delete_lead(self, lead_id: int) -> None:
        with self._session() as s, s.begin():
            s.query(LeadRow).filter(
                LeadRow.workspace_id == self.workspace_id, LeadRow.id == lead_id
            ).delete()

    # ── stats / facets ──
    def get_stats(self) -> Dict[str, Any]:
        from sqlalchemy import func

        with self._session() as s:
            base = s.query(LeadRow).filter(
                LeadRow.workspace_id == self.workspace_id
            )
            total = base.count()

            def _group(col):
                rows = (
                    s.query(col, func.count())
                    .filter(LeadRow.workspace_id == self.workspace_id)
                    .group_by(col)
                    .all()
                )
                return {k: v for k, v in rows}

            status_counts = _group(LeadRow.status)
            tier_counts = _group(LeadRow.score_tier)
            source_counts = _group(LeadRow.source)
            city_rows = (
                s.query(LeadRow.city, func.count())
                .filter(LeadRow.workspace_id == self.workspace_id)
                .group_by(LeadRow.city)
                .order_by(func.count().desc())
                .limit(15)
                .all()
            )
            city_counts = {k: v for k, v in city_rows}

            def _nonempty(col):
                return base.filter(col.isnot(None), col != "", col != "N/A").count()

            avg_score = (
                s.query(func.avg(LeadRow.score))
                .filter(LeadRow.workspace_id == self.workspace_id)
                .scalar()
            )

            return {
                "total": total,
                "by_status": status_counts,
                "by_tier": tier_counts,
                "by_city": city_counts,
                "by_source": source_counts,
                "enrichment": {
                    "total": total,
                    "with_email": _nonempty(LeadRow.email),
                    "with_phone": _nonempty(LeadRow.phone),
                    "with_website": _nonempty(LeadRow.website),
                    "with_linkedin": _nonempty(LeadRow.linkedin_url),
                    "with_contact": _nonempty(LeadRow.contact_person),
                    "avg_score": round(avg_score or 0, 1),
                },
            }

    def get_cities(self) -> List[str]:
        with self._session() as s:
            rows = (
                s.query(LeadRow.city)
                .filter(LeadRow.workspace_id == self.workspace_id, LeadRow.city != "")
                .distinct()
                .order_by(LeadRow.city)
                .all()
            )
            return [r[0] for r in rows]

    def get_sources(self) -> List[str]:
        with self._session() as s:
            rows = (
                s.query(LeadRow.source)
                .filter(LeadRow.workspace_id == self.workspace_id, LeadRow.source != "")
                .distinct()
                .order_by(LeadRow.source)
                .all()
            )
            return [r[0] for r in rows]

    # ── signals (tenant-scoped) ──
    def add_signal(self, signal) -> str:
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
            # Automations (§3.4): the ONLY signal event source with a real
            # workspace_id. Fire on_signal rules for the matched lead's workbook
            # rows. fire_key="signal:<pk>" → idempotent across re-inserts. No-op
            # when AUTOMATIONS_ENABLED is off. Inside the same RLS-scoped session.
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

    def get_signals(
        self,
        signal_type: Optional[str] = None,
        lead_id: Optional[int] = None,
        limit: int = 50,
        offset: int = 0,
    ) -> List[dict]:
        with self._session() as s:
            q = s.query(SignalRow).filter(
                SignalRow.workspace_id == self.workspace_id
            )
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

    # ── lifecycle (LeadDB-compatible no-ops) ──
    def close(self) -> None:
        pass

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()
