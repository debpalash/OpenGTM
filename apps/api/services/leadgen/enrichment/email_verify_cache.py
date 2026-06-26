"""
Per-email verification cache.

Sibling to ``cache.py`` (the identity-keyed enrichment cache). That one caches
the *winner of a waterfall* keyed by lead identity (name+domain). This one caches
the *deliverability status of a literal email address* keyed by the email itself
plus the verifier that produced it — a different key space, so it lives in its own
table (``email_verify_cache``) inside the SAME SQLite file (``enrichment_cache.db``).

Why a separate cache:
  - The verification result is a property of the *address*, not of a lead's
    identity (two leads with the same email share the answer). Cross-workspace
    sharing of a hashed-email → status mapping is a deliberate cost win, not a
    leak: we store only sha256(lower(email)) + a 4-status verdict, never the
    plaintext address, never tenant data. Same trust model as the global
    enrichment cache.
  - TTLs differ by verdict: a DEFINITIVE result (valid/invalid/catch_all) is
    stable for ~2 weeks; an UNKNOWN is a transient failure (greylist, port-25
    blocked, Reacher down) that must self-heal quickly, so it is cached only
    briefly. Caching UNKNOWN at all just stops a hot loop from hammering a dead
    endpoint within a single run.

SQLite-backed, TTL'd, thread-safe, dependency-free — mirrors ``cache.py``.
"""

import hashlib
import logging
import sqlite3
import threading
import time
from pathlib import Path
from typing import Optional

logger = logging.getLogger("leadgen.enrichment.email_verify_cache")

# Freshness windows. Definitive verdicts decay slowly; unknowns must re-check.
DEFINITIVE_TTL_DAYS = 14
UNKNOWN_TTL_DAYS = 1

# The four normalized statuses (kept in sync with email_verify_cascade).
_DEFINITIVE = {"valid", "invalid", "catch_all"}


def email_key(email: str, verifier: str) -> str:
    """Cache key for (email, verifier): sha256(lower(email)) + verifier name.

    Hashing the address means the cache file never stores a plaintext email —
    only an opaque digest → status. ``verifier`` is part of the key so a future
    second verifier's verdict for the same address is cached independently.
    """
    norm = (email or "").strip().lower()
    digest = hashlib.sha256(norm.encode("utf-8")).hexdigest()
    return f"{digest}:{verifier}"


class EmailVerifyCache:
    """SQLite-backed, TTL'd, thread-safe per-email verification cache."""

    def __init__(self, db_path: Optional[str] = None):
        if db_path is None:
            from apps.api.services.leadgen.config import DATA_DIR
            db_path = str(Path(DATA_DIR) / "enrichment_cache.db")
        Path(db_path).parent.mkdir(parents=True, exist_ok=True)
        self.db_path = db_path
        self._lock = threading.Lock()
        self._conn = sqlite3.connect(db_path, check_same_thread=False)
        self._conn.execute("PRAGMA journal_mode=WAL")
        self._conn.execute("PRAGMA busy_timeout=10000")
        self._conn.execute("""
            CREATE TABLE IF NOT EXISTS email_verify_cache (
                cache_key   TEXT PRIMARY KEY,
                status      TEXT NOT NULL,
                source      TEXT,
                detail      TEXT,
                confidence  REAL DEFAULT 0,
                created_at  REAL,
                expires_at  REAL
            )
        """)
        self._conn.commit()

    def get(self, email: str, verifier: str) -> Optional[dict]:
        """Return {status, source, detail, confidence} on a fresh hit, else None."""
        if not email or not verifier:
            return None
        key = email_key(email, verifier)
        now = time.time()
        with self._lock:
            row = self._conn.execute(
                "SELECT status, source, detail, confidence, expires_at "
                "FROM email_verify_cache WHERE cache_key=?",
                (key,),
            ).fetchone()
        if not row:
            return None
        status, source, detail, confidence, expires_at = row
        if expires_at and expires_at < now:
            return None  # stale (lazy expiry)
        return {
            "status": status,
            "source": source or "",
            "detail": detail or "",
            "confidence": confidence or 0.0,
        }

    def set(self, email: str, verifier: str, status: str, *, source: str = "",
            detail: str = "", confidence: float = 0.0,
            ttl_days: Optional[int] = None) -> None:
        """Cache a verdict. Definitive verdicts get the long TTL, unknown a short one."""
        if not email or not verifier or not status:
            return
        if ttl_days is None:
            ttl_days = DEFINITIVE_TTL_DAYS if status in _DEFINITIVE else UNKNOWN_TTL_DAYS
        key = email_key(email, verifier)
        now = time.time()
        with self._lock:
            self._conn.execute(
                "INSERT INTO email_verify_cache "
                "(cache_key, status, source, detail, confidence, created_at, expires_at) "
                "VALUES (?,?,?,?,?,?,?) "
                "ON CONFLICT(cache_key) DO UPDATE SET "
                "status=excluded.status, source=excluded.source, detail=excluded.detail, "
                "confidence=excluded.confidence, created_at=excluded.created_at, "
                "expires_at=excluded.expires_at",
                (key, status, source, detail, float(confidence or 0.0),
                 now, now + ttl_days * 86400),
            )
            self._conn.commit()

    def purge_expired(self) -> int:
        with self._lock:
            cur = self._conn.execute(
                "DELETE FROM email_verify_cache WHERE expires_at < ?", (time.time(),)
            )
            self._conn.commit()
            return cur.rowcount


# Process-wide singleton (lazy), mirroring cache.get_cache().
_verify_cache: Optional[EmailVerifyCache] = None
_verify_cache_lock = threading.Lock()


def get_verify_cache() -> EmailVerifyCache:
    global _verify_cache
    if _verify_cache is None:
        with _verify_cache_lock:
            if _verify_cache is None:
                _verify_cache = EmailVerifyCache()
    return _verify_cache
