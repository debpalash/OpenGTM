"""
Chat History — SQLite-backed conversation storage

Stores conversations and messages for the chat interface.
Uses the same data/ directory as LeadDB.

Tenancy (PR-C / OD-6): every conversation is partitioned by
``workspace_id`` + ``user_id`` so one workspace/user can never read, continue,
or delete another's chat history. This is a standalone SQLite store (not a PG
table), so isolation is enforced at the application layer with explicit
``WHERE workspace_id = ? AND user_id = ?`` filters rather than Postgres RLS.

Self-host (single ``main`` workspace, no auth → ``user_id`` is ``None``) keys
on ``workspace_id`` + a stable :data:`SELF_HOST_USER` sentinel, so a keyless
deployment still has a consistent, isolated partition across restarts.
"""

import sqlite3
import os
import uuid
from datetime import datetime, timezone
from typing import Optional


DATA_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(__file__)))), "data")
DB_PATH = os.path.join(DATA_DIR, "chat_history.db")

# Stable partition value for the self-host / keyless path where there is no
# authenticated user (``user_id`` is None). Keying on (workspace_id, this)
# keeps the self-host partition consistent across restarts.
SELF_HOST_USER = "self-host"


def _uid(user_id) -> str:
    """Normalize a user id (int / str / None) into a stable partition string."""
    if user_id is None or user_id == "":
        return SELF_HOST_USER
    return str(user_id)


def _get_db() -> sqlite3.Connection:
    os.makedirs(DATA_DIR, exist_ok=True)
    conn = sqlite3.connect(DB_PATH)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA foreign_keys=ON")
    return conn


def _column_names(conn: sqlite3.Connection, table: str) -> set:
    return {r["name"] for r in conn.execute(f"PRAGMA table_info({table})").fetchall()}


def _init_tables(conn: sqlite3.Connection):
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS conversations (
            id TEXT PRIMARY KEY,
            title TEXT NOT NULL DEFAULT 'New Chat',
            workspace_id TEXT,
            user_id TEXT,
            created_at TEXT NOT NULL DEFAULT (datetime('now')),
            updated_at TEXT NOT NULL DEFAULT (datetime('now'))
        );
        CREATE TABLE IF NOT EXISTS messages (
            id TEXT PRIMARY KEY,
            conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
            role TEXT NOT NULL CHECK(role IN ('user', 'assistant', 'system', 'tool')),
            content TEXT NOT NULL DEFAULT '',
            tool_data TEXT,
            created_at TEXT NOT NULL DEFAULT (datetime('now'))
        );
        CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(conversation_id, created_at);
    """)
    # Idempotent column-adds for DBs created before tenant partitioning landed.
    # (CREATE TABLE IF NOT EXISTS won't add columns to an existing table.)
    cols = _column_names(conn, "conversations")
    if "workspace_id" not in cols:
        conn.execute("ALTER TABLE conversations ADD COLUMN workspace_id TEXT")
    if "user_id" not in cols:
        conn.execute("ALTER TABLE conversations ADD COLUMN user_id TEXT")
    conn.execute(
        "CREATE INDEX IF NOT EXISTS idx_conv_tenant "
        "ON conversations(workspace_id, user_id, updated_at)"
    )
    conn.commit()


# Initialize tables on import
_conn = _get_db()
_init_tables(_conn)
_conn.close()


# ── Public API ───────────────────────────────────────────────────


def create_conversation(workspace_id: str, user_id=None, title: str = "New Chat") -> dict:
    """Create a new conversation in the (workspace, user) partition."""
    if not workspace_id:
        raise ValueError("create_conversation requires a workspace_id")
    uid = _uid(user_id)
    conv_id = str(uuid.uuid4())[:12]
    conn = _get_db()
    try:
        conn.execute(
            "INSERT INTO conversations (id, title, workspace_id, user_id) VALUES (?, ?, ?, ?)",
            (conv_id, title, workspace_id, uid),
        )
        conn.commit()
        now = datetime.now(timezone.utc).isoformat()
        return {"id": conv_id, "title": title, "created_at": now, "updated_at": now}
    finally:
        conn.close()


def list_conversations(workspace_id: str, user_id=None, limit: int = 50) -> list[dict]:
    """List conversations for this (workspace, user), newest first."""
    if not workspace_id:
        raise ValueError("list_conversations requires a workspace_id")
    uid = _uid(user_id)
    conn = _get_db()
    try:
        rows = conn.execute(
            "SELECT id, title, created_at, updated_at FROM conversations "
            "WHERE workspace_id = ? AND user_id = ? ORDER BY updated_at DESC LIMIT ?",
            (workspace_id, uid, limit),
        ).fetchall()
        return [dict(r) for r in rows]
    finally:
        conn.close()


def get_conversation(conv_id: str, workspace_id: str, user_id=None) -> Optional[dict]:
    """Get a single conversation by ID, scoped to (workspace, user).

    Returns None if it doesn't exist OR belongs to another tenant (no
    cross-tenant existence leak).
    """
    if not workspace_id:
        raise ValueError("get_conversation requires a workspace_id")
    uid = _uid(user_id)
    conn = _get_db()
    try:
        row = conn.execute(
            "SELECT id, title, created_at, updated_at FROM conversations "
            "WHERE id = ? AND workspace_id = ? AND user_id = ?",
            (conv_id, workspace_id, uid),
        ).fetchone()
        return dict(row) if row else None
    finally:
        conn.close()


def get_messages(conv_id: str, workspace_id: str, user_id=None) -> list[dict]:
    """Get all messages for a conversation owned by (workspace, user).

    Messages are joined to their conversation so a foreign ``conv_id`` returns
    nothing even if the caller guesses a valid id.
    """
    if not workspace_id:
        raise ValueError("get_messages requires a workspace_id")
    uid = _uid(user_id)
    conn = _get_db()
    try:
        rows = conn.execute(
            "SELECT m.id, m.conversation_id, m.role, m.content, m.tool_data, m.created_at "
            "FROM messages m JOIN conversations c ON c.id = m.conversation_id "
            "WHERE m.conversation_id = ? AND c.workspace_id = ? AND c.user_id = ? "
            "ORDER BY m.created_at ASC",
            (conv_id, workspace_id, uid),
        ).fetchall()
        return [dict(r) for r in rows]
    finally:
        conn.close()


def add_message(conv_id: str, role: str, content: str, tool_data: str = None) -> Optional[dict]:
    """Append a message to a conversation.

    Callers MUST have already established that ``conv_id`` belongs to the active
    (workspace, user) — e.g. it was just returned by :func:`create_conversation`
    or verified via :func:`get_conversation`. As a defense-in-depth backstop the
    INSERT is gated on the conversation still existing; a missing row is a no-op.
    """
    msg_id = str(uuid.uuid4())[:12]
    conn = _get_db()
    try:
        exists = conn.execute(
            "SELECT 1 FROM conversations WHERE id = ?", (conv_id,)
        ).fetchone()
        if not exists:
            return None
        conn.execute(
            "INSERT INTO messages (id, conversation_id, role, content, tool_data) VALUES (?, ?, ?, ?, ?)",
            (msg_id, conv_id, role, content, tool_data),
        )
        conn.execute(
            "UPDATE conversations SET updated_at = datetime('now') WHERE id = ?",
            (conv_id,),
        )
        conn.commit()
        return {"id": msg_id, "conversation_id": conv_id, "role": role, "content": content, "tool_data": tool_data}
    finally:
        conn.close()


def update_title(conv_id: str, title: str, workspace_id: str, user_id=None):
    """Update a conversation's title (scoped to the owning tenant)."""
    if not workspace_id:
        raise ValueError("update_title requires a workspace_id")
    uid = _uid(user_id)
    conn = _get_db()
    try:
        conn.execute(
            "UPDATE conversations SET title = ? WHERE id = ? AND workspace_id = ? AND user_id = ?",
            (title, conv_id, workspace_id, uid),
        )
        conn.commit()
    finally:
        conn.close()


def delete_conversation(conv_id: str, workspace_id: str, user_id=None):
    """Delete a conversation (and its messages) only if it belongs to (workspace, user)."""
    if not workspace_id:
        raise ValueError("delete_conversation requires a workspace_id")
    uid = _uid(user_id)
    conn = _get_db()
    try:
        conn.execute(
            "DELETE FROM conversations WHERE id = ? AND workspace_id = ? AND user_id = ?",
            (conv_id, workspace_id, uid),
        )
        conn.commit()
    finally:
        conn.close()


def auto_title_from_message(text: str) -> str:
    """Generate a short title from the first user message."""
    title = text.strip()[:60]
    if len(text) > 60:
        title = title.rsplit(" ", 1)[0] + "..."
    return title or "New Chat"
