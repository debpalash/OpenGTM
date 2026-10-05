"""Backfill and verify the collection ledger (SQLite ``leads.db`` files -> PostgreSQL).

Same guarantees as :mod:`apps.api.services.workspace.backfill` (read-only
sources, idempotent, resumable by re-running, never clobbers existing PostgreSQL
rows unless ``overwrite``, order-independent SHA-256 checksums, keys and digests
only in reports), applied to:

* ``jobs``       -> ``collection_jobs``
* ``job_stages`` -> ``collection_job_stages`` (``legacy_id`` = the SQLite id, the
  idempotency key)

``llm_usage`` (daily per-provider counters) is deliberately NOT moved: the
PostgreSQL lead store already accumulates the same counters in
``llm_usage_daily`` from its own writers, so merging the two would need a sum,
which cannot be made idempotent. Historical per-host usage stays in the SQLite
file; new usage lands in PostgreSQL.

Which file holds which tenant's rows is the tenant boundary of the old design,
so attribution is deliberate and never guessed:

* a per-workspace file (``data/workspaces/<slug>/leads.db``) belongs to that
  workspace; every row in it is attributed to it;
* the main file (``data/leads.db``) belongs to the ``main`` workspace, except a
  job whose own ``workspace_id`` names another KNOWN workspace (the pipeline
  stamps it when a tenant submits through the shared file): that job, and its
  stages, follow the stamp.  A blank or unknown stamp falls back to ``main``.
  ``reattributed`` in the report counts how many followed a stamp.

Stages whose job is not in the file are orphans: skipped and counted, never
attached to a guessed tenant. ``activity_log`` has no writers and is not moved.
"""

from __future__ import annotations

import os
import sqlite3
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, Iterator, List, Optional, Tuple

from apps.api.services.workspace import pg_meta
from apps.api.services.workspace.backfill import (
    Digest,
    Table,
    TableCheck,
    TableResult,
    _open_source,
    row_hash,
)

JOBS = Table("collection_jobs", (
    ("id", "text"), ("workspace_id", "text"), ("query", "text"), ("intent", "text"),
    ("intent_details", "text"), ("status", "text"), ("tier", "int"), ("attempts", "int"),
    ("max_attempts", "int"), ("leads_found", "int"), ("proxy_used", "text"), ("error", "text"),
    ("created_at", "text"), ("started_at", "text"), ("completed_at", "text")), ("id",), scope="tenant")
STAGES = Table("collection_job_stages", (
    ("workspace_id", "text"), ("job_id", "text"), ("legacy_id", "int"), ("stage", "text"),
    ("status", "text"), ("input_count", "int"), ("output_count", "int"), ("rejected_count", "int"),
    ("details", "text"), ("started_at", "text"), ("completed_at", "text")),
    ("workspace_id", "job_id", "legacy_id"), scope="tenant")
LEDGER_TABLES = (JOBS, STAGES)

_DEFAULTS = {"text": "", "int": 0}


def _clean(table: Table, row: Dict[str, Any]) -> Dict[str, Any]:
    """NULL -> the column's NOT NULL default, identically for source and target."""
    out = dict(row)
    for col, kind in table.columns:
        if out.get(col) is None:
            out[col] = _DEFAULTS[kind]
    return out


@dataclass
class Source:
    label: str
    path: str
    workspace_id: str  # the file's tenant
    is_main: bool


def _workspaces(data_dir: str) -> Dict[str, str]:
    """slug -> workspace id from the source directory (read-only)."""
    conn = _open_source(os.path.join(data_dir, "workspaces.db"))
    if conn is None:
        raise RuntimeError(f"{data_dir}/workspaces.db not found: the ledger backfill needs it to map files to workspaces")
    return {r["slug"]: r["id"] for r in conn.execute("SELECT id, slug FROM workspaces")}


def discover(data_dir: str, main_leads_db: Optional[str] = None) -> Tuple[List[Source], Dict[str, str]]:
    by_slug = _workspaces(data_dir)
    if "main" not in by_slug:
        raise RuntimeError("source has no 'main' workspace")
    sources: List[Source] = []
    main_path = main_leads_db or os.path.join(data_dir, "leads.db")
    if os.path.exists(main_path):
        sources.append(Source("main", main_path, by_slug["main"], True))
    for slug, wid in sorted(by_slug.items()):
        if slug == "main":
            continue
        path = os.path.join(data_dir, "workspaces", slug, "leads.db")
        if os.path.exists(path):
            sources.append(Source(slug, path, wid, False))
    return sources, by_slug


def _has(conn: sqlite3.Connection, table: str) -> bool:
    return conn.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", (table,)).fetchone() is not None


def _cols(conn: sqlite3.Connection, table: str) -> set:
    return {r[1] for r in conn.execute(f"PRAGMA table_info({table})")}


@dataclass
class Extracted:
    jobs: List[Dict[str, Any]] = field(default_factory=list)
    stages: List[Dict[str, Any]] = field(default_factory=list)
    orphan_stages: int = 0
    reattributed: int = 0
    duplicate_jobs: int = 0


def extract(data_dir: str, main_leads_db: Optional[str] = None) -> Extracted:
    """Read every source file into attributed, normalised rows."""
    sources, by_slug = discover(data_dir, main_leads_db)
    known = set(by_slug.values())
    out = Extracted()
    seen_jobs: Dict[str, str] = {}
    for src in sources:
        conn = _open_source(src.path)
        if conn is None:
            continue
        job_ws: Dict[str, str] = {}
        if _has(conn, "jobs"):
            have = _cols(conn, "jobs")
            wanted = [c for c, _ in JOBS.columns if c in have]
            for r in conn.execute(f"SELECT {', '.join(wanted)} FROM jobs"):
                row = dict(zip(wanted, tuple(r)))
                ws = src.workspace_id
                stamp = row.get("workspace_id") or ""
                if src.is_main and stamp in known and stamp != ws:
                    ws = stamp
                    out.reattributed += 1
                row["workspace_id"] = ws
                row = _clean(JOBS, row)
                if row["id"] in seen_jobs:
                    out.duplicate_jobs += 1
                    continue
                seen_jobs[row["id"]] = ws
                job_ws[row["id"]] = ws
                out.jobs.append(row)
        if _has(conn, "job_stages"):
            have = _cols(conn, "job_stages")
            wanted = [c for c in ("id", "job_id", "stage", "status", "input_count", "output_count",
                                  "rejected_count", "details", "started_at", "completed_at") if c in have]
            for r in conn.execute(f"SELECT {', '.join(wanted)} FROM job_stages ORDER BY id"):
                row = dict(zip(wanted, tuple(r)))
                ws = job_ws.get(row.get("job_id"))
                if ws is None:
                    out.orphan_stages += 1
                    continue
                row["legacy_id"] = row.pop("id")
                row["workspace_id"] = ws
                out.stages.append(_clean(STAGES, row))
    return out


def _key(table: Table, row: Dict[str, Any]) -> str:
    return "/".join(str(row[c]) for c in table.pk)


def _conn(workspace_id: str) -> pg_meta.PgMetaConnection:
    return pg_meta.PgMetaConnection(workspace_id=workspace_id, control_plane=False, seed_default=False)


def _upsert(table: Table, overwrite: bool) -> str:
    cols = table.names
    marks = ", ".join("?" for _ in cols)
    if table is STAGES:
        target = "(workspace_id, job_id, legacy_id) WHERE legacy_id IS NOT NULL"
    else:
        target = f"({', '.join(table.pk)})"
    non_pk = [c for c in cols if c not in table.pk]
    action = ("DO UPDATE SET " + ", ".join(f"{c} = EXCLUDED.{c}" for c in non_pk)
              if overwrite and non_pk else "DO NOTHING")
    return f"INSERT INTO {table.name} ({', '.join(cols)}) VALUES ({marks}) ON CONFLICT {target} {action}"


def backfill_ledger(
    data_dir: str,
    *,
    main_leads_db: Optional[str] = None,
    overwrite: bool = False,
    batch: int = 500,
    dry_run: bool = False,
    progress: Callable[[str], None] = lambda m: None,
) -> Dict[str, TableResult]:
    data = extract(data_dir, main_leads_db)
    results: Dict[str, TableResult] = {}
    for table, rows in ((JOBS, data.jobs), (STAGES, data.stages)):
        res = results[table.name] = TableResult(table.name, source_rows=len(rows))
        sql = _upsert(table, overwrite)
        by_ws: Dict[str, List[Dict[str, Any]]] = {}
        for r in rows:
            by_ws.setdefault(r["workspace_id"], []).append(r)
        if dry_run:
            continue
        for ws, grp in by_ws.items():
            for i in range(0, len(grp), batch):
                conn = _conn(ws)
                try:
                    for r in grp[i:i + batch]:
                        changed = max(conn.execute(sql, tuple(r.get(c) for c in table.names)).rowcount, 0)
                        if changed:
                            if overwrite:
                                res.updated += 1
                            else:
                                res.inserted += 1
                        else:
                            res.skipped_existing += 1
                    conn.commit()
                except Exception:
                    conn.rollback()
                    raise
                finally:
                    conn.close()
        progress(f"{table.name}: {res.source_rows} source rows, {res.inserted + res.updated} written")
    if data.orphan_stages:
        progress(f"skipped {data.orphan_stages} orphan stage row(s) whose job is not in the file")
    results["_attribution"] = TableResult(
        "_attribution", source_rows=data.reattributed + data.orphan_stages + data.duplicate_jobs)
    results["_attribution"].updated = data.reattributed        # jobs that followed their own stamp
    results["_attribution"].skipped_existing = data.orphan_stages + data.duplicate_jobs
    return results


def verify_ledger(data_dir: str, *, main_leads_db: Optional[str] = None, key_cap: int = 20) -> List[TableCheck]:
    data = extract(data_dir, main_leads_db)
    workspaces = sorted(set(_workspaces(data_dir).values()) | {r["workspace_id"] for r in data.jobs})
    out: List[TableCheck] = []
    for table, rows in ((JOBS, data.jobs), (STAGES, data.stages)):
        source = {_key(table, r): row_hash(table, r) for r in rows}
        target: Dict[str, int] = {}
        cols = ", ".join(table.names)
        where = " WHERE legacy_id IS NOT NULL" if table is STAGES else ""
        for ws in workspaces:
            conn = _conn(ws)
            try:
                for r in conn.execute(f"SELECT {cols} FROM {table.name}{where}").fetchall():
                    d = _clean(table, dict(r))
                    target[_key(table, d)] = row_hash(table, d)
            finally:
                conn.close()
        sd, td = Digest(), Digest()
        for h in source.values():
            sd.add(h)
        for h in target.values():
            td.add(h)
        out.append(TableCheck(
            table.name, sd.rows, td.rows, sd.hex, td.hex,
            sorted(set(source) - set(target))[:key_cap], sorted(set(target) - set(source))[:key_cap],
            sorted(k for k in set(source) & set(target) if source[k] != target[k])[:key_cap],
            ok=(sd.rows == td.rows and sd.acc == td.acc)))
    return out
