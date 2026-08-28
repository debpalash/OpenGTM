"""
Cross-provider enrichment cache.

Ported from masteranime/enrichment-kit (canonicalKey) + nimajnebrevilo/GTM-Engine
(getAnyCachedEnrichment). See docs/research/clay-alternatives-ingestion-catalog.md
(top-10 #2). The single biggest cost lever Yupcha was missing: cache the WINNER
of a waterfall keyed by the lead's *identity* (not by provider), so if Apollo
already found this person's email, we never call Prospeo/Hunter again — for this
lead or any other lead with the same identity.

SQLite-backed, TTL'd, dependency-free. Safe to call from the waterfall hot path.
"""

import hashlib
import logging
import re
import sqlite3
import threading
import time
from pathlib import Path
from typing import Any, Optional

logger = logging.getLogger("leadgen.enrichment.cache")

# Per-field freshness. Emails decay ~22-30%/yr (see clay benchmarks) → 30 days.
DEFAULT_TTL_DAYS = 30
TTL_DAYS = {
    "email": 30,
    "phone": 60,
    "linkedin_url": 90,
    "company_size": 90,
    "description": 180,
    "founded_year": 365,
}

_LEGAL_SUFFIX_RE = re.compile(
    r"\b(inc|incorporated|llc|l\.l\.c|ltd|limited|corp|corporation|co|company|gmbh|"
    r"ag|sarl|sas|sa|bv|plc|pvt|pte|llp|group|holdings?)\b\.?",
    re.IGNORECASE,
)


def _norm_company(name: str) -> str:
    if not name:
        return ""
    n = name.lower()
    n = _LEGAL_SUFFIX_RE.sub("", n)
    n = re.sub(r"[^a-z0-9]+", "", n)
    return n


def _norm_domain(website: str) -> str:
    if not website:
        return ""
    d = website.strip().lower()
    for p in ("https://", "http://", "www."):
        d = d.removeprefix(p)
    d = d.split("/")[0].split("?")[0]
    parts = [x for x in d.split(".") if x]
    return ".".join(parts[-2:]) if len(parts) >= 2 else d


def _norm_name(name: str) -> str:
    return re.sub(r"\s+", " ", (name or "").strip().lower())


def canonical_key(field_name: str, lead) -> Optional[str]:
    """Build the identity cache key for (field_name, lead).

    Identity is what determines the lookup result, NOT the provider:
      - person fields (email/phone/linkedin/contact_*) → name + domain
      - company fields → domain (or normalized company name as fallback)
    Returns None when there isn't enough identity to key on (don't cache).
    """
    domain = _norm_domain(getattr(lead, "website", "") or "")
    company = _norm_company(getattr(lead, "company", "") or "")
    name = _norm_name(getattr(lead, "contact_person", "") or "")

    person_fields = {"email", "phone", "linkedin_url", "contact_person", "contact_title"}
    if field_name in person_fields:
        ident_domain = domain or company
        if not ident_domain or not name:
            return None  # need a person + an org to key a person-level lookup
        ident = f"person|{name}|{ident_domain}"
    else:
        ident_company = domain or company
        if not ident_company:
            return None
        ident = f"company|{ident_company}"

    raw = f"{field_name}|{ident}"
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()[:24]


class EnrichmentCache:
    """SQLite-backed, TTL'd, thread-safe cross-provider value cache."""

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
            CREATE TABLE IF NOT EXISTS enrichment_cache (
                cache_key   TEXT NOT NULL,
                field_name  TEXT NOT NULL,
                value       TEXT,
                confidence  REAL DEFAULT 0,
                provider    TEXT,
                created_at  REAL,
                expires_at  REAL,
                PRIMARY KEY (cache_key, field_name)
            )
        """)
        self._conn.commit()

    def get(self, cache_key: str, field_name: str) -> Optional[dict]:
        """Return {value, confidence, provider} if a fresh hit exists, else None."""
        if not cache_key:
            return None
        now = time.time()
        with self._lock:
            row = self._conn.execute(
                "SELECT value, confidence, provider, expires_at FROM enrichment_cache "
                "WHERE cache_key=? AND field_name=?",
                (cache_key, field_name),
            ).fetchone()
        if not row:
            return None
        value, confidence, provider, expires_at = row
        if expires_at and expires_at < now:
            return None  # stale (lazy expiry)
        return {"value": value, "confidence": confidence or 0.0, "provider": provider or "cache"}

    def set(self, cache_key: str, field_name: str, value: Any, confidence: float = 0.0,
            provider: str = "", ttl_days: Optional[int] = None):
        if not cache_key or value in (None, "", []):
            return
        ttl = (ttl_days if ttl_days is not None else TTL_DAYS.get(field_name, DEFAULT_TTL_DAYS))
        now = time.time()
        with self._lock:
            self._conn.execute(
                "INSERT INTO enrichment_cache "
                "(cache_key, field_name, value, confidence, provider, created_at, expires_at) "
                "VALUES (?,?,?,?,?,?,?) "
                "ON CONFLICT(cache_key, field_name) DO UPDATE SET "
                "value=excluded.value, confidence=excluded.confidence, "
                "provider=excluded.provider, created_at=excluded.created_at, "
                "expires_at=excluded.expires_at",
                (cache_key, field_name, str(value), float(confidence or 0.0), provider, now, now + ttl * 86400),
            )
            self._conn.commit()

    def purge_expired(self) -> int:
        with self._lock:
            cur = self._conn.execute("DELETE FROM enrichment_cache WHERE expires_at < ?", (time.time(),))
            self._conn.commit()
            return cur.rowcount


# Process-wide singleton (lazy).
_cache: Optional[EnrichmentCache] = None
_cache_lock = threading.Lock()


def get_cache() -> EnrichmentCache:
    global _cache
    if _cache is None:
        with _cache_lock:
            if _cache is None:
                _cache = EnrichmentCache()
    return _cache
