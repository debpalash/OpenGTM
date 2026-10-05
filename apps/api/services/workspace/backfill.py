"""Idempotent, verifiable backfill of the single-host metadata stores into PostgreSQL.

Sources (opened READ-ONLY; nothing in them is ever created, altered or deleted):

* ``<data-dir>/workspaces.db``: workspaces, members, permissions, OIDC
  identities, SCIM tokens/users/groups, active workspace, and
  ``workspace_settings`` (which holds the envelope-encrypted per-workspace
  secrets);
* ``<data-dir>/data.db`` table ``settings``: the global settings. Values whose
  key names a credential were plaintext there and are enveloped on the way in
  (``pg_meta.is_sensitive_setting``), using the configured key mechanism.

Targets are the PostgreSQL tables of migration ``9b3d5f7a2c41``, written through
:mod:`pg_meta` so the runtime role and forced row-level security apply to the
backfill exactly as they do to the application.

Guarantees:

* **Idempotent and resumable.** Rows are upserted by primary key in batches of
  their own transaction, so a crash loses at most one batch and re-running
  continues: rows already present are skipped (default) or refreshed
  (``overwrite=True``). Running it N times equals running it once.
* **Non-destructive.** The default never changes a row that already exists in
  PostgreSQL, so a re-run after cut-over cannot clobber newer writes; pass
  ``overwrite=True`` only before cut-over, while the source is still the truth.
* **Verified.** :func:`verify` compares, per table, the row count and an
  order-independent checksum (sum of per-row SHA-256 mod 2^256) computed
  identically over the source rows and the rows read back through the
  application path. Ciphertext is compared as the opaque string it is; the
  global-settings checksum hashes the decrypted value so the enveloping step is
  checked too, and only digests, counts and keys are ever reported, never values.
* **Secrets stay secret.** Nothing here logs a value. ``check_decrypt`` proves
  every envelope decrypts with the current key and reports only counts and keys.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import sqlite3
from dataclasses import asdict, dataclass, field
from typing import Any, Dict, Iterable, Iterator, List, Optional, Sequence, Tuple

from apps.api.services.workspace import pg_meta

logger = logging.getLogger("workspace.backfill")

MOD = 1 << 256


@dataclass(frozen=True)
class Table:
    name: str
    columns: Tuple[Tuple[str, str], ...]  # (column, kind) kind in text|int|real
    pk: Tuple[str, ...]
    scope: str = "directory"  # "directory" or "tenant" (workspace_settings)
    source: str = "workspaces.db"

    @property
    def names(self) -> List[str]:
        return [c for c, _ in self.columns]


TABLES: Tuple[Table, ...] = (
    Table("workspaces", (("id", "text"), ("name", "text"), ("slug", "text"), ("description", "text"),
                         ("icon", "text"), ("owner_id", "int"), ("created_at", "real"),
                         ("updated_at", "real")), ("id",)),
    Table("workspace_members", (("workspace_id", "text"), ("user_id", "int"), ("role", "text"),
                                ("created_at", "real")), ("workspace_id", "user_id")),
    Table("workspace_member_permissions", (("workspace_id", "text"), ("user_id", "int"), ("permission", "text"),
                                           ("effect", "text"), ("updated_at", "real")),
          ("workspace_id", "user_id", "permission")),
    Table("workspace_oidc_identities", (("workspace_id", "text"), ("issuer", "text"), ("subject", "text"),
                                        ("user_id", "int"), ("email", "text"), ("created_at", "real")),
          ("workspace_id", "issuer", "subject")),
    Table("workspace_scim_tokens", (("workspace_id", "text"), ("token_hash", "text"), ("token_prefix", "text"),
                                    ("created_at", "real"), ("expires_at", "real"), ("last_used_at", "real"),
                                    ("created_by", "int")), ("workspace_id",)),
    Table("workspace_scim_users", (("workspace_id", "text"), ("user_id", "int"), ("external_id", "text"),
                                   ("display_name", "text"), ("active", "int"), ("created_at", "real"),
                                   ("updated_at", "real")), ("workspace_id", "user_id")),
    Table("workspace_scim_groups", (("id", "text"), ("workspace_id", "text"), ("external_id", "text"),
                                    ("display_name", "text"), ("created_at", "real"), ("updated_at", "real")),
          ("id",)),
    Table("workspace_scim_group_members", (("workspace_id", "text"), ("group_id", "text"), ("user_id", "int"),
                                           ("created_at", "real")), ("workspace_id", "group_id", "user_id")),
    Table("user_active_workspace", (("user_id", "int"), ("workspace_id", "text")), ("user_id",)),
    Table("workspace_settings", (("workspace_id", "text"), ("key", "text"), ("value", "text")),
          ("workspace_id", "key"), scope="tenant"),
)
SETTINGS = Table("app_settings", (("key", "text"), ("value", "text")), ("key",), source="data.db")
BY_NAME = {t.name: t for t in TABLES + (SETTINGS,)}
ALL_STORES = ("meta", "settings")


# ── normalisation and hashing ───────────────────────────────────────────────

def _norm(kind: str, value: Any) -> Any:
    if value is None:
        return None
    if kind == "real":
        return repr(float(value))
    if kind == "int":
        return int(value)
    return str(value)


def row_hash(table: Table, row: Dict[str, Any], *, override: Optional[Dict[str, Any]] = None) -> int:
    values = dict(row)
    if override:
        values.update(override)
    payload = json.dumps([_norm(k, values.get(c)) for c, k in table.columns],
                         ensure_ascii=True, separators=(",", ":"))
    return int.from_bytes(hashlib.sha256(payload.encode("ascii")).digest(), "big")


@dataclass
class Digest:
    rows: int = 0
    acc: int = 0

    def add(self, h: int) -> None:
        self.rows += 1
        self.acc = (self.acc + h) % MOD

    @property
    def hex(self) -> str:
        return f"{self.acc:064x}"


# ── sources ─────────────────────────────────────────────────────────────────

def _open_source(path: str) -> Optional[sqlite3.Connection]:
    if not os.path.exists(path):
        return None
    conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    conn.row_factory = sqlite3.Row
    return conn


def _source_rows(conn: Optional[sqlite3.Connection], table: Table) -> Iterator[Dict[str, Any]]:
    if conn is None:
        return
    have = {r[1] for r in conn.execute(f"PRAGMA table_info({table.name if table is not SETTINGS else 'settings'})")}
    if not have:
        return
    cols = [c for c in table.names if c in have]
    src = "settings" if table is SETTINGS else table.name
    for row in conn.execute(f"SELECT {', '.join(cols)} FROM {src}"):
        yield {c: row[c] for c in cols}


def _global_setting_plain(value: Optional[str]) -> str:
    """The plaintext of a stored global setting (decrypting an envelope)."""
    return pg_meta._open(value) or ""


# ── target helpers ──────────────────────────────────────────────────────────

def _target_rows(table: Table, workspace_ids: Iterable[str]) -> Iterator[Dict[str, Any]]:
    cols = ", ".join(table.names)
    if table.scope == "tenant":
        for wid in sorted(set(workspace_ids)):
            conn = pg_meta.tenant(wid)
            try:
                for r in conn.execute(f"SELECT {cols} FROM {table.name}").fetchall():
                    yield dict(r)
            finally:
                conn.close()
        return
    conn = pg_meta.directory()
    try:
        for r in conn.execute(f"SELECT {cols} FROM {table.name}").fetchall():
            yield dict(r)
    finally:
        conn.close()


def _upsert_sql(table: Table, overwrite: bool) -> str:
    cols = table.names
    marks = ", ".join("?" for _ in cols)
    non_pk = [c for c in cols if c not in table.pk]
    if overwrite and non_pk:
        action = "DO UPDATE SET " + ", ".join(f"{c} = EXCLUDED.{c}" for c in non_pk)
    else:
        action = "DO NOTHING"
    return (f"INSERT INTO {table.name} ({', '.join(cols)}) VALUES ({marks}) "
            f"ON CONFLICT ({', '.join(table.pk)}) {action}")


@dataclass
class TableResult:
    table: str
    source_rows: int = 0
    inserted: int = 0
    skipped_existing: int = 0
    updated: int = 0


def _reconcile_default_workspace(source_main: Optional[Dict[str, Any]], overwrite: bool) -> Optional[str]:
    """The target auto-creates a ``main`` workspace the first time it is opened,
    with a fresh id; the source has its own. Slug is unique, so they cannot
    both exist. An untouched target ``main`` is replaced by the source's;
    one that already has dependent rows is a conflict the operator must settle."""
    if not source_main:
        return None
    conn = pg_meta.directory()
    try:
        row = conn.execute("SELECT id FROM workspaces WHERE slug = 'main'").fetchone()
        if row is None or row["id"] == source_main["id"]:
            return None
        target_id = row["id"]
        deps = 0
        for t in TABLES:
            if t.name == "workspaces":
                continue
            if t.scope == "tenant":
                continue  # needs its own binding; checked below
            deps += conn.execute(
                f"SELECT count(*) AS n FROM {t.name} WHERE workspace_id = ?", (target_id,)).fetchone()["n"]
        conn.close()
        conn = pg_meta.tenant(target_id)
        deps += conn.execute("SELECT count(*) AS n FROM workspace_settings").fetchone()["n"]
        conn.close()
        if deps:
            raise RuntimeError(
                "PostgreSQL already has a 'main' workspace "
                f"({target_id}) with {deps} dependent rows that differs from the source's "
                f"({source_main['id']}). Resolve it manually before backfilling.")
        conn = pg_meta.directory()
        conn.execute("DELETE FROM workspaces WHERE id = ?", (target_id,))
        conn.commit()
        logger.info("replaced the untouched auto-created 'main' workspace %s with the source's %s",
                    target_id, source_main["id"])
        return target_id
    finally:
        conn.close()


def backfill(
    data_dir: str,
    *,
    stores: Sequence[str] = ALL_STORES,
    overwrite: bool = False,
    batch: int = 500,
    dry_run: bool = False,
    progress=lambda msg: None,
) -> Dict[str, TableResult]:
    """Copy the selected stores. Safe to run any number of times."""
    results: Dict[str, TableResult] = {}
    plan: List[Tuple[Table, Optional[sqlite3.Connection]]] = []
    if "meta" in stores:
        ws = _open_source(os.path.join(data_dir, "workspaces.db"))
        plan += [(t, ws) for t in TABLES]
    if "settings" in stores:
        plan.append((SETTINGS, _open_source(os.path.join(data_dir, "data.db"))))

    if not dry_run:
        main_src = None
        for t, conn in plan:
            if t.name == "workspaces":
                main_src = next((r for r in _source_rows(conn, t) if r.get("slug") == "main"), None)
        _reconcile_default_workspace(main_src, overwrite)

    for table, conn in plan:
        res = results[table.name] = TableResult(table.name)
        pending: List[Dict[str, Any]] = []

        def flush():
            nonlocal pending
            if pending and not dry_run:
                _write_batch(table, pending, overwrite, res)
                progress(f"{table.name}: {res.source_rows} rows seen, {res.inserted + res.updated} written")
            pending = []

        for row in _source_rows(conn, table):
            res.source_rows += 1
            if table is SETTINGS:
                row = dict(row, value=pg_meta._seal(row["key"], row.get("value") or ""))
            pending.append(row)
            if len(pending) >= batch:
                flush()
        flush()
        progress(f"{table.name}: done ({res.source_rows} source rows)")
    return results


def _write_batch(table: Table, rows: List[Dict[str, Any]], overwrite: bool, res: TableResult) -> None:
    sql = _upsert_sql(table, overwrite)
    groups: Dict[Optional[str], List[Dict[str, Any]]] = {}
    for r in rows:
        groups.setdefault(r["workspace_id"] if table.scope == "tenant" else None, []).append(r)
    for wid, grp in groups.items():
        conn = pg_meta.tenant(wid) if table.scope == "tenant" else pg_meta.directory()
        try:
            for r in grp:
                params = tuple(r.get(c) for c in table.names)
                changed = max(conn.execute(sql, params).rowcount, 0)
                if changed:
                    if overwrite:
                        res.updated += 1  # inserted or refreshed
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


# ── verification ────────────────────────────────────────────────────────────

@dataclass
class TableCheck:
    table: str
    source_rows: int
    target_rows: int
    source_digest: str
    target_digest: str
    missing_keys: List[str] = field(default_factory=list)   # in source, absent in PostgreSQL
    extra_keys: List[str] = field(default_factory=list)     # in PostgreSQL only
    changed_keys: List[str] = field(default_factory=list)   # same key, different content
    ok: bool = False


def _key_of(table: Table, row: Dict[str, Any]) -> str:
    return "/".join(str(row[c]) for c in table.pk)


def verify(data_dir: str, *, stores: Sequence[str] = ALL_STORES, key_cap: int = 20) -> List[TableCheck]:
    """Compare every selected table between the sources and PostgreSQL."""
    plan: List[Tuple[Table, Optional[sqlite3.Connection]]] = []
    if "meta" in stores:
        ws = _open_source(os.path.join(data_dir, "workspaces.db"))
        plan += [(t, ws) for t in TABLES]
    if "settings" in stores:
        plan.append((SETTINGS, _open_source(os.path.join(data_dir, "data.db"))))

    source: Dict[str, Dict[str, int]] = {}
    for table, conn in plan:
        rows = {}
        for r in _source_rows(conn, table):
            if table is SETTINGS:
                r = dict(r, value=_global_setting_plain(r.get("value")))
            rows[_key_of(table, r)] = row_hash(table, r)
        source[table.name] = rows
    settings_ws = set()
    for table, conn in plan:
        if table.name == "workspace_settings":
            settings_ws = {k.split("/")[0] for k in source[table.name]}
    all_ws = settings_ws | {r["id"] for r in _target_rows(BY_NAME["workspaces"], [])}

    out: List[TableCheck] = []
    for table, _ in plan:
        target: Dict[str, int] = {}
        for r in _target_rows(table, all_ws):
            if table is SETTINGS:
                r = dict(r, value=_global_setting_plain(r.get("value")))
            target[_key_of(table, r)] = row_hash(table, r)
        s = source[table.name]
        sd, td = Digest(), Digest()
        for h in s.values():
            sd.add(h)
        for h in target.values():
            td.add(h)
        missing = sorted(set(s) - set(target))
        extra = sorted(set(target) - set(s))
        changed = sorted(k for k in set(s) & set(target) if s[k] != target[k])
        out.append(TableCheck(
            table.name, sd.rows, td.rows, sd.hex, td.hex,
            missing[:key_cap], extra[:key_cap], changed[:key_cap],
            ok=(sd.rows == td.rows and sd.acc == td.acc),
        ))
    return out


def check_decrypt() -> Dict[str, Any]:
    """Prove every stored envelope decrypts with the CURRENT key mechanism.
    Reports counts and the offending ``workspace/key`` names, never values."""
    from apps.api.services.workspace.secrets import decrypt_value

    report: Dict[str, Any] = {"envelopes": 0, "ok": 0, "failed": [], "plaintext_secret_like": []}
    ws_ids = [r["id"] for r in _target_rows(BY_NAME["workspaces"], [])]
    for r in _target_rows(BY_NAME["workspace_settings"], ws_ids):
        value = r.get("value") or ""
        if value.startswith(pg_meta._ENVELOPES):
            report["envelopes"] += 1
            try:
                decrypt_value(value)
                report["ok"] += 1
            except Exception as exc:  # noqa: BLE001 - never include the value
                report["failed"].append(f"{r['workspace_id']}/{r['key']}: {type(exc).__name__}")
        elif value and pg_meta.is_sensitive_setting(r["key"]):
            report["plaintext_secret_like"].append(f"{r['workspace_id']}/{r['key']}")
    for r in _target_rows(SETTINGS, []):
        value = r.get("value") or ""
        if value.startswith(pg_meta._ENVELOPES):
            report["envelopes"] += 1
            try:
                decrypt_value(value)
                report["ok"] += 1
            except Exception as exc:  # noqa: BLE001
                report["failed"].append(f"global/{r['key']}: {type(exc).__name__}")
        elif value and pg_meta.is_sensitive_setting(r["key"]):
            report["plaintext_secret_like"].append(f"global/{r['key']}")
    report["all_decrypt"] = report["ok"] == report["envelopes"]
    return report


def summarize(checks: List[TableCheck]) -> Dict[str, Any]:
    return {"ok": all(c.ok for c in checks), "tables": [asdict(c) for c in checks]}
