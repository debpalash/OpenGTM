"""PostgreSQL backend for workspace metadata and global settings (RFC M8).

On a single host the workspace directory (``workspaces``, members, SSO/SCIM,
``workspace_settings`` with the encrypted per-workspace secrets) lives in
``data/workspaces.db`` and the global settings in ``data/data.db``. Neither file
can be shared between hosts. With ``WORKSPACE_META_STORE=postgres`` the same
tables live in PostgreSQL (migration ``9b3d5f7a2c41``) under forced row-level
security, and this module is the only thing that talks to them.

The existing code (``manager.py``, ``scim.py``, ``secrets.py``,
``routers/settings.py``) speaks the sqlite3 connection API with ``?``
placeholders. :class:`PgMetaConnection` implements the small subset of that API
those callers use over a SQLAlchemy connection, so their SQL and logic stay
exactly as they are and a single-host install never touches this module. The
dialect differences it bridges are enumerated in :func:`translate`.

Tenant isolation, per connection and per transaction (``set_config(.., true)``
so nothing outlives the transaction on a pooled connection):

* ``PgMetaConnection(workspace_id=W, control_plane=False)`` is a tenant
  connection: ``app.workspace_id = W`` only. It is what reads and writes
  ``workspace_settings`` (the secrets), whose policy admits nothing else.
* ``PgMetaConnection()`` is the directory connection: ``app.control_plane = on``,
  for the cross-tenant lookups membership checks need ("which workspaces is this
  user in?"). A tenant-bound ORM session never sets that flag, so it can read
  only its own workspace's members, tokens and identities.

Secrets stay encrypted with the existing key mechanism: values are the same
``enc:v1:`` / ``enc:v2:vault:`` envelopes produced by
``services/workspace/secrets.py`` and are copied and compared as opaque
ciphertext. Global settings whose key names a credential are enveloped on write
in PostgreSQL (they were plaintext in ``data.db``).
"""

from __future__ import annotations

import logging
import re
import threading
import time
import uuid
import weakref
from typing import Any, Iterable, Optional, Sequence

from sqlalchemy import text
from sqlalchemy.engine import Engine

logger = logging.getLogger("workspace.pg_meta")

BACKENDS = ("sqlite", "postgres")

# Primary keys of the directory tables, for translating INSERT OR REPLACE.
PRIMARY_KEYS: dict[str, tuple[str, ...]] = {
    "workspaces": ("id",),
    "workspace_settings": ("workspace_id", "key"),
    "workspace_members": ("workspace_id", "user_id"),
    "workspace_member_permissions": ("workspace_id", "user_id", "permission"),
    "workspace_oidc_identities": ("workspace_id", "issuer", "subject"),
    "workspace_scim_tokens": ("workspace_id",),
    "workspace_scim_users": ("workspace_id", "user_id"),
    "workspace_scim_groups": ("id",),
    "workspace_scim_group_members": ("workspace_id", "group_id", "user_id"),
    "user_active_workspace": ("user_id",),
    "app_settings": ("key",),
}

_engine_override: Optional[Engine] = None
_ready_lock = threading.Lock()
_ready: "weakref.WeakSet[Engine]" = weakref.WeakSet()  # engines already role-checked and seeded


# ── selection ───────────────────────────────────────────────────────────────

def backend() -> str:
    """The configured metadata backend: ``sqlite`` (default) or ``postgres``."""
    from apps.api.core.config import settings

    value = (settings.WORKSPACE_META_STORE or "sqlite").strip().lower()
    if value not in BACKENDS:
        raise ValueError(f"WORKSPACE_META_STORE must be one of {BACKENDS}, got {value!r}")
    return value


def is_postgres() -> bool:
    return backend() == "postgres"


def use_engine(engine: Optional[Engine]) -> None:
    """Point the store at an explicit engine (tests, backfill). ``None`` resets."""
    global _engine_override
    _engine_override = engine


def _engine() -> Engine:
    if _engine_override is not None:
        return _engine_override
    from apps.api.database import IS_SQLITE, engine

    if IS_SQLITE:
        raise RuntimeError(
            "WORKSPACE_META_STORE=postgres requires a PostgreSQL DATABASE_URL; "
            "this database is SQLite"
        )
    return engine


def _prepare(engine: Engine) -> None:
    """Once per engine: refuse a role that bypasses RLS, ensure ``main`` exists."""
    if engine in _ready:
        return
    with _ready_lock:
        if engine in _ready:
            return
        from apps.api.core.config import settings

        with engine.begin() as conn:
            row = conn.execute(text(
                "SELECT current_user, rolsuper, rolbypassrls FROM pg_roles "
                "WHERE rolname = current_user")).first()
            unsafe = row is None or row[1] or row[2]
            if unsafe:
                msg = (
                    "The workspace metadata store refuses to run as a role that "
                    "bypasses row-level security (superuser or BYPASSRLS): its "
                    f"policies would be silently inert ({row!r}). Connect as the "
                    "runtime role, e.g. yupcha_app."
                )
                if settings.PG_RLS_REQUIRE_SAFE_ROLE:
                    raise RuntimeError(msg)
                logger.critical(msg)
            conn.execute(text("SELECT set_config('app.control_plane', 'on', true)"))
            now = time.time()
            conn.execute(
                text("INSERT INTO workspaces (id, name, slug, description, icon, created_at, updated_at) "
                     "VALUES (:id, 'Default Workspace', 'main', 'Your main workspace', :icon, :now, :now) "
                     "ON CONFLICT (slug) DO NOTHING"),
                {"id": str(uuid.uuid4()), "icon": "\U0001F3E0", "now": now},
            )
        _ready.add(engine)


# ── SQL translation ─────────────────────────────────────────────────────────

_REPLACE = re.compile(
    r"^\s*INSERT\s+OR\s+REPLACE\s+INTO\s+(\w+)\s*\(([^)]*)\)\s*VALUES\s*(\(.*\))\s*;?\s*$",
    re.IGNORECASE | re.DOTALL,
)
_IGNORE = re.compile(r"^\s*INSERT\s+OR\s+IGNORE\s+INTO\s+(.*?);?\s*$", re.IGNORECASE | re.DOTALL)
_BEGIN_IMMEDIATE = re.compile(r"^\s*BEGIN\s+IMMEDIATE\s*;?\s*$", re.IGNORECASE)


def translate(sql: str) -> str:
    """sqlite3 dialect to PostgreSQL for the statements the callers use.

    * ``?`` placeholders become ``%s`` and literal ``%`` is doubled;
    * ``INSERT OR REPLACE INTO t (cols) VALUES (...)`` becomes an upsert on the
      table's primary key (``PRIMARY_KEYS``);
    * ``INSERT OR IGNORE INTO ...`` becomes ``INSERT ... ON CONFLICT DO NOTHING``;
    * ``BEGIN IMMEDIATE`` (used to serialize a read-then-write) becomes a
      transaction-scoped advisory lock, the PostgreSQL equivalent.

    Everything else the callers send (``ON CONFLICT ... DO UPDATE``, ``UNION``,
    ``LIMIT``, ``COUNT(*)``, ``SELECT *``) is already portable.
    """
    if _BEGIN_IMMEDIATE.match(sql):
        return "SELECT pg_advisory_xact_lock(hashtext('opengtm:workspace-meta'))"
    m = _REPLACE.match(sql)
    if m:
        table, cols, values = m.group(1).lower(), m.group(2), m.group(3)
        pk = PRIMARY_KEYS.get(table)
        if pk is None:
            raise ValueError(f"INSERT OR REPLACE into unknown table {table!r}")
        names = [c.strip() for c in cols.split(",")]
        updates = ", ".join(f"{c} = EXCLUDED.{c}" for c in names if c not in pk)
        action = f"DO UPDATE SET {updates}" if updates else "DO NOTHING"
        sql = (f"INSERT INTO {table} ({cols}) VALUES {values} "
               f"ON CONFLICT ({', '.join(pk)}) {action}")
    else:
        m = _IGNORE.match(sql)
        if m:
            sql = f"INSERT INTO {m.group(1)} ON CONFLICT DO NOTHING"
    return sql.replace("%", "%%").replace("?", "%s")


# ── sqlite3-compatible connection ───────────────────────────────────────────

class MetaRow:
    """``sqlite3.Row`` look-alike: index by position or column name, ``keys()``,
    iteration over values, usable with ``dict(row)``."""

    __slots__ = ("_cols", "_vals")

    def __init__(self, cols: Sequence[str], vals: Sequence[Any]):
        self._cols = tuple(cols)
        self._vals = tuple(vals)

    def __getitem__(self, key):
        if isinstance(key, str):
            try:
                return self._vals[self._cols.index(key)]
            except ValueError:
                raise IndexError(f"No item with that key: {key!r}") from None
        return self._vals[key]

    def keys(self):
        return list(self._cols)

    def __iter__(self):
        return iter(self._vals)

    def __len__(self):
        return len(self._vals)

    def __repr__(self):  # never print values: rows can hold ciphertext or token hashes
        return f"<MetaRow {self._cols}>"


class MetaCursor:
    def __init__(self, rows: list[MetaRow], rowcount: int):
        self._rows = rows
        self._pos = 0
        self.rowcount = rowcount

    def fetchone(self):
        if self._pos >= len(self._rows):
            return None
        row = self._rows[self._pos]
        self._pos += 1
        return row

    def fetchall(self):
        rows, self._pos = self._rows[self._pos:], len(self._rows)
        return rows

    def __iter__(self):
        return iter(self.fetchall())


class PgMetaConnection:
    """The sqlite3 connection subset the callers use, over PostgreSQL.

    The transaction starts on the first statement and the tenant context is
    bound in it before that statement runs; ``commit``/``rollback`` end it and
    the next statement binds again. ``close`` rolls back anything uncommitted
    (sqlite3 does too) and returns the connection to the pool.
    """

    def __init__(
        self,
        engine: Optional[Engine] = None,
        *,
        workspace_id: Optional[str] = None,
        control_plane: bool = True,
    ):
        self._engine = engine or _engine()
        _prepare(self._engine)
        self._conn = self._engine.connect()
        self._workspace_id = workspace_id
        self._control_plane = control_plane
        self._bound = False
        self._closed = False

    def _bind(self) -> None:
        if self._bound:
            return
        self._conn.exec_driver_sql(
            "SELECT set_config('app.control_plane', %s, true)",
            ("on" if self._control_plane else "off",),
        )
        if self._workspace_id:
            self._conn.exec_driver_sql(
                "SELECT set_config('app.workspace_id', %s, true)", (self._workspace_id,))
        self._bound = True

    def execute(self, sql: str, params: Iterable[Any] = ()) -> MetaCursor:
        self._bind()
        result = self._conn.exec_driver_sql(translate(sql), tuple(params))
        if result.returns_rows:
            cols = list(result.keys())
            rows = [MetaRow(cols, r) for r in result.fetchall()]
        else:
            rows = []
        return MetaCursor(rows, result.rowcount if result.rowcount is not None else -1)

    def executemany(self, sql: str, seq: Iterable[Iterable[Any]]) -> MetaCursor:
        total = 0
        for params in seq:
            total += max(self.execute(sql, params).rowcount, 0)
        return MetaCursor([], total)

    def commit(self) -> None:
        self._conn.commit()
        self._bound = False

    def rollback(self) -> None:
        self._conn.rollback()
        self._bound = False

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        try:
            self._conn.rollback()
        finally:
            self._conn.close()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()


def directory() -> PgMetaConnection:
    """Cross-tenant directory connection (workspaces, members, SSO, SCIM)."""
    return PgMetaConnection()


def tenant(workspace_id: str) -> PgMetaConnection:
    """Strictly tenant-scoped connection (``workspace_settings`` / secrets)."""
    if not workspace_id:
        raise ValueError("workspace_id is required")
    return PgMetaConnection(workspace_id=workspace_id, control_plane=False)


def directory_for_workspace(workspace_id: str) -> PgMetaConnection:
    """Directory connection that is also bound to one workspace, for work that
    spans the directory tables and that workspace's settings (deleting it)."""
    return PgMetaConnection(workspace_id=workspace_id, control_plane=True)


# ── global settings (replaces data/data.db ``settings``) ────────────────────

_SENSITIVE_SUFFIXES = ("_KEY", "_TOKEN", "_SECRET", "_PASSWORD", "_PASS", "_PASSPHRASE", "_COOKIE")
_SENSITIVE_PARTS = ("API_KEY", "SECRET", "PASSWORD", "TOKEN", "PRIVATE_KEY")
_ENVELOPES = ("enc:v1:", "enc:v2:vault:")


def is_sensitive_setting(key: str) -> bool:
    """True for keys that name a credential; those are enveloped at rest."""
    k = (key or "").upper()
    return k.endswith(_SENSITIVE_SUFFIXES) or any(p in k for p in _SENSITIVE_PARTS)


def _seal(key: str, value: str) -> str:
    if not value or not is_sensitive_setting(key) or value.startswith(_ENVELOPES):
        return value
    from apps.api.services.workspace.secrets import encrypt_value

    return encrypt_value(value)


def _open(value: Optional[str]) -> Optional[str]:
    if value and value.startswith(_ENVELOPES):
        from apps.api.services.workspace.secrets import decrypt_value

        return decrypt_value(value)
    return value


def settings_get(key: str) -> Optional[str]:
    """The stored value (decrypted) or None when the key is absent."""
    conn = directory()
    try:
        row = conn.execute("SELECT value FROM app_settings WHERE key = ?", (key,)).fetchone()
    finally:
        conn.close()
    return _open(row["value"]) if row else None


def settings_set(key: str, value: str) -> None:
    sealed = _seal(key, value)
    conn = directory()
    try:
        conn.execute(
            "INSERT INTO app_settings (key, value) VALUES (?, ?) "
            "ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()",
            (key, sealed),
        )
        conn.commit()
    finally:
        conn.close()


def settings_keys() -> set[str]:
    conn = directory()
    try:
        return {r["key"] for r in conn.execute("SELECT key FROM app_settings").fetchall()}
    finally:
        conn.close()


def settings_insert_missing(items: Iterable[tuple[str, str]]) -> int:
    """Insert keys that do not exist yet (never overwrites). Returns how many."""
    conn = directory()
    added = 0
    try:
        for key, value in items:
            added += max(conn.execute(
                "INSERT INTO app_settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO NOTHING",
                (key, _seal(key, value)),
            ).rowcount, 0)
        conn.commit()
    finally:
        conn.close()
    return added
