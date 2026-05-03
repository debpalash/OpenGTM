"""
Memory Service — OpenMemory wrapper

Provides persistent AI memory for the chat system.
Falls back gracefully if openmemory-py is not installed.
"""

import os
import logging

logger = logging.getLogger(__name__)

_memory_instance = None
_memory_available = None


def _check_available() -> bool:
    """Check if openmemory-py is installed."""
    global _memory_available
    if _memory_available is not None:
        return _memory_available
    try:
        from openmemory.client import Memory
        _memory_available = True
    except ImportError:
        logger.warning("openmemory-py not installed — chat memory disabled. pip install openmemory-py")
        _memory_available = False
    return _memory_available


def _get_memory():
    """Get or create the singleton Memory instance."""
    global _memory_instance
    if _memory_instance is not None:
        return _memory_instance
    if not _check_available():
        return None
    try:
        from openmemory.client import Memory

        data_dir = os.path.join(
            os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(__file__)))),
            "data",
        )
        os.makedirs(data_dir, exist_ok=True)

        _memory_instance = Memory(
            storage_path=os.path.join(data_dir, "openmemory.db"),
        )
        logger.info("OpenMemory initialized at %s", data_dir)
        return _memory_instance
    except Exception as e:
        logger.error("Failed to initialize OpenMemory: %s", e)
        _memory_available = False
        return None


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
