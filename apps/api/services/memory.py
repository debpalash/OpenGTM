"""
Memory Service — cross-conversation chat memory.

Preferred backend is OpenMemory (if openmemory-py is installed). When it's not,
we fall back to a lightweight, dependency-free SQLite store with keyword+recency
recall — so chat memory works out of the box (no external SDK, no API key) and
transparently upgrades to OpenMemory if that package is ever added.
"""

import os
import re
import json
import sqlite3
import logging
from datetime import datetime, timezone

logger = logging.getLogger(__name__)

_memory_instance = None
_memory_available = None

_STOPWORDS = {
    "the", "a", "an", "and", "or", "but", "for", "to", "of", "in", "on", "at",
    "is", "are", "was", "were", "be", "with", "my", "me", "i", "you", "it",
    "this", "that", "show", "find", "get", "what", "how", "do", "does", "can",
}


def _data_dir() -> str:
    d = os.path.join(
        os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(__file__)))),
        "data",
    )
    os.makedirs(d, exist_ok=True)
    return d


def _tokens(text: str) -> set:
    return {w for w in re.findall(r"[a-z0-9]+", (text or "").lower())
            if len(w) > 2 and w not in _STOPWORDS}


class _BuiltinMemory:
    """Lightweight SQLite memory: keyword-overlap + recency recall. No deps."""

    def __init__(self, path: str):
        self.path = path
        with self._conn() as c:
            c.execute("""
                CREATE TABLE IF NOT EXISTS memories (
                    id         INTEGER PRIMARY KEY AUTOINCREMENT,
                    user_id    TEXT NOT NULL,
                    text       TEXT NOT NULL,
                    metadata   TEXT DEFAULT '{}',
                    created_at TEXT NOT NULL
                )
            """)
            c.execute("CREATE INDEX IF NOT EXISTS ix_mem_user ON memories(user_id)")

    def _conn(self):
        conn = sqlite3.connect(self.path)
        conn.row_factory = sqlite3.Row
        return conn

    def add(self, text: str, user_id: str = "default", metadata: dict = None):
        with self._conn() as c:
            c.execute(
                "INSERT INTO memories (user_id, text, metadata, created_at) VALUES (?, ?, ?, ?)",
                (user_id, text, json.dumps(metadata or {}), datetime.now(timezone.utc).isoformat()),
            )
        return {"ok": True}

    def search(self, query: str, user_id: str = "default", limit: int = 5) -> list:
        q = _tokens(query)
        with self._conn() as c:
            rows = c.execute(
                "SELECT text, metadata, created_at FROM memories WHERE user_id = ? "
                "ORDER BY id DESC LIMIT 500", (user_id,),
            ).fetchall()
        scored = []
        for r in rows:
            overlap = len(q & _tokens(r["text"])) if q else 0
            if overlap > 0:
                scored.append((overlap, r))
        # Highest keyword overlap first; recency (row order) breaks ties.
        scored.sort(key=lambda t: t[0], reverse=True)
        return [{"memory": r["text"], "metadata": json.loads(r["metadata"] or "{}"),
                 "created_at": r["created_at"]} for _, r in scored[:limit]]

    def get_all(self, user_id: str = "default") -> list:
        with self._conn() as c:
            rows = c.execute(
                "SELECT text, metadata, created_at FROM memories WHERE user_id = ? ORDER BY id DESC",
                (user_id,),
            ).fetchall()
        return [{"memory": r["text"], "metadata": json.loads(r["metadata"] or "{}"),
                 "created_at": r["created_at"]} for r in rows]


def _check_available() -> bool:
    """Memory is always available — OpenMemory if installed, else the builtin store."""
    global _memory_available
    if _memory_available is None:
        _memory_available = True
    return _memory_available


def _get_memory():
    """Get or create the singleton Memory backend (OpenMemory preferred)."""
    global _memory_instance
    if _memory_instance is not None:
        return _memory_instance
    # Prefer OpenMemory if the SDK is installed.
    try:
        from openmemory.client import Memory
        _memory_instance = Memory(storage_path=os.path.join(_data_dir(), "openmemory.db"))
        logger.info("Chat memory: OpenMemory backend")
        return _memory_instance
    except ImportError:
        pass
    except Exception as e:
        logger.warning("OpenMemory init failed (%s); using builtin memory", e)
    # Dependency-free fallback.
    _memory_instance = _BuiltinMemory(os.path.join(_data_dir(), "chat_memory.db"))
    logger.info("Chat memory: builtin SQLite backend")
    return _memory_instance


# ── Public API ───────────────────────────────────────────────────


def add_memory(text: str, user_id: str = "default", metadata: dict = None):
    """Store a memory. Returns None if unavailable."""
    mem = _get_memory()
    if not mem:
        return None
    try:
        return mem.add(text, user_id=user_id, metadata=metadata or {})
    except Exception as e:
        logger.warning("Memory add failed: %s", e)
        return None


def search_memory(query: str, user_id: str = "default", limit: int = 5) -> list[dict]:
    """Search memories. Returns empty list if unavailable."""
    mem = _get_memory()
    if not mem:
        return []
    try:
        results = mem.search(query, user_id=user_id, limit=limit)
        if isinstance(results, list):
            return results
        # Some versions return a dict with "results" key
        if isinstance(results, dict) and "results" in results:
            return results["results"][:limit]
        return []
    except Exception as e:
        logger.warning("Memory search failed: %s", e)
        return []


def get_all_memories(user_id: str = "default") -> list[dict]:
    """Get all memories for a user. Returns empty list if unavailable."""
    mem = _get_memory()
    if not mem:
        return []
    try:
        results = mem.get_all(user_id=user_id)
        if isinstance(results, list):
            return results
        if isinstance(results, dict) and "results" in results:
            return results["results"]
        return []
    except Exception as e:
        logger.warning("Memory get_all failed: %s", e)
        return []


def is_available() -> bool:
    """Check if memory system is operational."""
    return _check_available()
