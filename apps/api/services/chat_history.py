"""
Chat History — SQLite-backed conversation storage

Stores conversations and messages for the chat interface.
Uses the same data/ directory as LeadDB.
"""

import sqlite3
import os
import uuid
from datetime import datetime
from typing import Optional


DATA_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(__file__)))), "data")
DB_PATH = os.path.join(DATA_DIR, "chat_history.db")


def _get_db() -> sqlite3.Connection:
    os.makedirs(DATA_DIR, exist_ok=True)
    conn = sqlite3.connect(DB_PATH)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA foreign_keys=ON")
    return conn


def _init_tables(conn: sqlite3.Connection):
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS conversations (
            id TEXT PRIMARY KEY,
            title TEXT NOT NULL DEFAULT 'New Chat',
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
    conn.commit()


# Initialize tables on import
_conn = _get_db()
_init_tables(_conn)
_conn.close()


# ── Public API ───────────────────────────────────────────────────


def create_conversation(title: str = "New Chat") -> dict:
    """Create a new conversation, return its data."""
    conv_id = str(uuid.uuid4())[:12]
    conn = _get_db()
    try:
        conn.execute(
            "INSERT INTO conversations (id, title) VALUES (?, ?)",
            (conv_id, title),
        )
        conn.commit()
        return {"id": conv_id, "title": title, "created_at": datetime.utcnow().isoformat(), "updated_at": datetime.utcnow().isoformat()}
    finally:
        conn.close()


def list_conversations(limit: int = 50) -> list[dict]:
    """List all conversations, newest first."""
    conn = _get_db()
    try:
        rows = conn.execute(
            "SELECT id, title, created_at, updated_at FROM conversations ORDER BY updated_at DESC LIMIT ?",
            (limit,),
        ).fetchall()
        return [dict(r) for r in rows]
    finally:
        conn.close()


def get_conversation(conv_id: str) -> Optional[dict]:
    """Get a single conversation by ID."""
    conn = _get_db()
    try:
        row = conn.execute(
            "SELECT id, title, created_at, updated_at FROM conversations WHERE id = ?",
            (conv_id,),
        ).fetchone()
        return dict(row) if row else None
    finally:
        conn.close()


def get_messages(conv_id: str) -> list[dict]:
    """Get all messages for a conversation, ordered by creation time."""
    conn = _get_db()
    try:
        rows = conn.execute(
            "SELECT id, conversation_id, role, content, tool_data, created_at FROM messages WHERE conversation_id = ? ORDER BY created_at ASC",
            (conv_id,),
        ).fetchall()
        return [dict(r) for r in rows]
    finally:
        conn.close()


def add_message(conv_id: str, role: str, content: str, tool_data: str = None) -> dict:
    """Add a message to a conversation."""
    msg_id = str(uuid.uuid4())[:12]
    conn = _get_db()
    try:
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


def update_title(conv_id: str, title: str):
    """Update a conversation's title."""
    conn = _get_db()
    try:
        conn.execute("UPDATE conversations SET title = ? WHERE id = ?", (title, conv_id))
        conn.commit()
    finally:
        conn.close()


def delete_conversation(conv_id: str):
    """Delete a conversation and all its messages."""
    conn = _get_db()
    try:
        conn.execute("DELETE FROM conversations WHERE id = ?", (conv_id,))
        conn.commit()
    finally:
        conn.close()


def auto_title_from_message(text: str) -> str:
    """Generate a short title from the first user message."""
    title = text.strip()[:60]
    if len(text) > 60:
        title = title.rsplit(" ", 1)[0] + "..."
    return title or "New Chat"
