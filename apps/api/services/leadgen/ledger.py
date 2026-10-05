"""The collection job ledger, on SQLite or PostgreSQL (RFC M8).

The lead-collection pipeline records its jobs and stages in a per-workspace
SQLite file (:class:`~apps.api.services.leadgen.db.LeadDB`). That file cannot be
shared between hosts: a job claimed by a worker on one host would be invisible
to the API on another. With ``COLLECTION_LEDGER_STORE=postgres`` the same ledger
lives in ``collection_jobs`` / ``collection_job_stages`` (migration
``b5d7f9a1c3e6``) under forced row-level security, and callers get a
:class:`PgCollectionLedger` from :func:`open_job_ledger` instead of a
``LeadDB``. Every other behaviour of the default (SQLite) path is unchanged.

:class:`PgCollectionLedger` implements the job/stage/usage surface of ``LeadDB``
that the pipeline and the job routes use, plus a ``conn`` that accepts the raw
``jobs`` / ``job_stages`` SQL a few call sites still issue (table names are
rewritten to the ``collection_*`` tables). Every connection is bound to one
workspace for the duration of its transactions, so the policy, not just a
``WHERE`` clause, confines it to that tenant. LLM usage is delegated to the
tenant lead store, which already keeps it in ``llm_usage_daily``.
"""

from __future__ import annotations

import re
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional

from apps.api.services.workspace import pg_meta

LEDGER_BACKENDS = ("sqlite", "postgres")

_TABLE_REWRITE = re.compile(r"\b(FROM|UPDATE|INTO|JOIN)(\s+)(jobs|job_stages)\b", re.IGNORECASE)
_REWRITE = {"jobs": "collection_jobs", "job_stages": "collection_job_stages"}

JOB_COLUMNS = ("id", "query", "intent", "intent_details", "status", "tier", "attempts", "max_attempts",
               "leads_found", "proxy_used", "error", "created_at", "started_at", "completed_at",
               "workspace_id")
STAGE_COLUMNS = ("id", "job_id", "stage", "status", "input_count", "output_count", "rejected_count",
                 "details", "started_at", "completed_at")


def backend() -> str:
    from apps.api.core.config import settings

    value = (settings.COLLECTION_LEDGER_STORE or "sqlite").strip().lower()
    if value not in LEDGER_BACKENDS:
        raise ValueError(f"COLLECTION_LEDGER_STORE must be one of {LEDGER_BACKENDS}, got {value!r}")
    return value


def is_postgres() -> bool:
    return backend() == "postgres"


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


class _LedgerConnection(pg_meta.PgMetaConnection):
    """Tenant-bound connection that maps the SQLite ledger's table names."""

    def __init__(self, workspace_id: str, engine=None):
        super().__init__(engine, workspace_id=workspace_id, control_plane=False,
                         seed_default=False, end_idle_reads=True)

    def execute(self, sql, params=()):
        return super().execute(
            _TABLE_REWRITE.sub(lambda m: f"{m.group(1)}{m.group(2)}{_REWRITE[m.group(3).lower()]}", sql),
            params,
        )


class PgCollectionLedger:
    """``LeadDB``'s job/stage/usage API over the PostgreSQL ledger."""

    def __init__(self, workspace_id: str, engine=None):
        if not workspace_id:
            raise ValueError("workspace_id is required for the PostgreSQL collection ledger")
        self.workspace_id = workspace_id
        self.conn = _LedgerConnection(workspace_id, engine)

    # -- jobs ----------------------------------------------------------

    def create_job(self, job_id: str, query: str, intent: str = "market_search",
                   intent_details: str = "{}") -> str:
        self.conn.execute(
            "INSERT INTO collection_jobs (id, workspace_id, query, intent, intent_details, created_at) "
            "VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING",
            (job_id, self.workspace_id, query, intent, intent_details, _now()),
        )
        self.conn.commit()
        return job_id

    def claim_job(self) -> Optional[Dict[str, Any]]:
        """Claim the next pending job of this workspace. SKIP LOCKED, so racing
        workers on different hosts never take the same one."""
        row = self.conn.execute(
            "SELECT * FROM collection_jobs WHERE status = 'pending' "
            "ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED"
        ).fetchone()
        if not row:
            self.conn.commit()
            return None
        self.conn.execute(
            "UPDATE collection_jobs SET status = 'running', started_at = ?, attempts = attempts + 1 WHERE id = ?",
            (_now(), row["id"]),
        )
        self.conn.commit()
        return dict(row)

    def complete_job(self, job_id: str, leads_found: int = 0):
        self.conn.execute(
            "UPDATE collection_jobs SET status = 'done', completed_at = ?, leads_found = ? WHERE id = ?",
            (_now(), leads_found, job_id),
        )
        self.conn.commit()

    def fail_job(self, job_id: str, error: str):
        """Re-queues while attempts remain, like the SQLite ledger."""
        row = self.conn.execute(
            "SELECT attempts, max_attempts FROM collection_jobs WHERE id = ?", (job_id,)).fetchone()
        if row and row["attempts"] < row["max_attempts"]:
            self.conn.execute(
                "UPDATE collection_jobs SET status = 'pending', error = ? WHERE id = ?", (error, job_id))
        else:
            self.conn.execute(
                "UPDATE collection_jobs SET status = 'failed', error = ?, completed_at = ? WHERE id = ?",
                (error, _now(), job_id))
        self.conn.commit()

    def cancel_job(self, job_id: str):
        self.conn.execute(
            "UPDATE collection_jobs SET status = 'cancelled', error = 'Cancelled by user', completed_at = ? "
            "WHERE id = ? AND status IN ('running', 'pending')",
            (_now(), job_id))
        self.conn.commit()

    def delete_job(self, job_id: str, keep_leads: bool = False):
        """Delete the job and its stages. Lead rows are not in this ledger: the
        callers delete them through the tenant lead store first (``keep_leads``
        is accepted for signature parity and must be True here)."""
        if not keep_leads:
            raise ValueError("PgCollectionLedger does not hold leads; delete them via the lead store")
        self.conn.execute("DELETE FROM collection_job_stages WHERE job_id = ?", (job_id,))
        self.conn.execute("DELETE FROM collection_jobs WHERE id = ?", (job_id,))
        self.conn.commit()

    def retry_job(self, job_id: str):
        self.conn.execute(
            "UPDATE collection_jobs SET status = 'pending', error = '', completed_at = '', attempts = 0 "
            "WHERE id = ? AND status IN ('failed', 'cancelled')", (job_id,))
        self.conn.commit()

    def get_jobs(self, status: Optional[str] = None, limit: int = 50) -> List[Dict[str, Any]]:
        cols = ", ".join(JOB_COLUMNS)
        if status:
            rows = self.conn.execute(
                f"SELECT {cols} FROM collection_jobs WHERE status = ? ORDER BY created_at DESC LIMIT ?",
                (status, limit)).fetchall()
        else:
            rows = self.conn.execute(
                f"SELECT {cols} FROM collection_jobs ORDER BY created_at DESC LIMIT ?", (limit,)).fetchall()
        self.conn.commit()
        return [dict(r) for r in rows]

    # -- stages --------------------------------------------------------

    def create_stage(self, job_id: str, stage: str) -> int:
        row = self.conn.execute(
            "INSERT INTO collection_job_stages (workspace_id, job_id, stage, status, started_at) "
            "VALUES (?, ?, ?, 'running', ?) RETURNING id",
            (self.workspace_id, job_id, stage, _now())).fetchone()
        self.conn.commit()
        return int(row["id"]) if row else 0

    def complete_stage(self, stage_id: int, input_count: int = 0, output_count: int = 0,
                       rejected_count: int = 0, details: str = "{}", status: str = "done"):
        self.conn.execute(
            "UPDATE collection_job_stages SET status = ?, input_count = ?, output_count = ?, "
            "rejected_count = ?, details = ?, completed_at = ? WHERE id = ?",
            (status, input_count, output_count, rejected_count, details, _now(), stage_id))
        self.conn.commit()

    def get_job_stages(self, job_id: str) -> List[Dict[str, Any]]:
        rows = self.conn.execute(
            f"SELECT {', '.join(STAGE_COLUMNS)} FROM collection_job_stages WHERE job_id = ? ORDER BY id",
            (job_id,)).fetchall()
        self.conn.commit()
        return [dict(r) for r in rows]

    def get_job_detail(self, job_id: str) -> Optional[Dict[str, Any]]:
        row = self.conn.execute(
            f"SELECT {', '.join(JOB_COLUMNS)} FROM collection_jobs WHERE id = ?", (job_id,)).fetchone()
        self.conn.commit()
        if not row:
            return None
        job = dict(row)
        job["stages"] = self.get_job_stages(job_id)
        return job

    # -- LLM usage: already tenant-scoped in PostgreSQL ----------------

    def _usage_store(self):
        from apps.api.services.leadgen.store import PgLeadStore

        return PgLeadStore(self.workspace_id)

    def record_llm_usage(self, *args, **kwargs):
        return self._usage_store().record_llm_usage(*args, **kwargs)

    def get_llm_usage(self, date: Optional[str] = None):
        return self._usage_store().get_llm_usage(date)

    def get_llm_usage_total(self):
        return self._usage_store().get_llm_usage_total()

    def close(self):
        self.conn.close()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()


def open_job_ledger(workspace_id: str, slug: str):
    """The job ledger for one workspace: ``LeadDB`` over the workspace's SQLite
    file (default), or the PostgreSQL ledger when COLLECTION_LEDGER_STORE=postgres."""
    if is_postgres():
        from apps.api.services.leadgen.store import use_pg_store

        if not use_pg_store():
            raise RuntimeError(
                "COLLECTION_LEDGER_STORE=postgres requires the PostgreSQL lead store "
                "(a PostgreSQL DATABASE_URL with PG_LEAD_STORE=true) so leads and their "
                "ledger live in the same database")
        return PgCollectionLedger(workspace_id)
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.services.workspace.manager import workspace_leads_db_path

    return LeadDB(workspace_leads_db_path(slug))
