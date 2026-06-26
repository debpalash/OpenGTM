"""In-process search-result cache + per-host adaptive backoff.

Two cooperating, process-local (no DB, no migration) mechanisms that wrap the
keyless/DDG search seam (``proxy_client._ResilientDDGS``) so the ~30 call sites
are upgraded transparently — the same way ``_ResilientDDGS`` already upgrades
them with multi-engine + connection fallback:

  1. **SearchCache** — a short-TTL, bounded LRU keyed on
     ``(tenant, method, normalized_query, max_results, backends)`` that dedupes
     identical searches within a window. Empty results get a *shorter* TTL so a
     momentarily-blocked query isn't pinned as "no data" for the full window,
     but a genuinely dead ``site:`` source isn't re-hammered every row.

  2. **HostBackoff** — per-host adaptive exponential backoff driven by *real*
     rate-limit/block signals (not empty results). A backed-off host is skipped
     so the search falls through to the next connection / keyed engine instead
     of hard-failing like the blunt domain circuit breaker would.

Both are gated by env flags (default ON). With both flags off the wrappers are
no-ops and behaviour is byte-for-byte identical to the pre-change code.

Tenancy
-------
The cache is *shared across workspaces* for keys that carry no tenant-private
input — that cross-workspace dedup of public ``site:``/web SERP rows is the
whole point. But the top-level sourcing query a user submits is free-form text
that *can* contain workspace-private terms (verified by audit: it flows from
``JobRunner.submit(query)`` straight into ``ddgs.text``). So the key is salted
with the active workspace from ``current_workspace_var`` (the same contextvar
that drives RLS, already set per-job and propagated into ``asyncio.to_thread``).
When no workspace is bound (salt == "") the entry is shared. This keeps the
primary win — intra-run dedup, which is per-workspace anyway — while never
leaking a tenant's free-form query across the boundary.

Memory bound: ``SEARCH_CACHE_MAX_ENTRIES`` (default 2000) entries × ~15 dicts ×
~300 B ≈ ~9 MB worst case. Hard-capped by LRU eviction on insert.
"""
from __future__ import annotations

import hashlib
import logging
import os
import random
import threading
import time
from collections import OrderedDict
from typing import List, Optional, Tuple

logger = logging.getLogger("leadgen.search_cache")


# ── Env config (read dynamically so a flag flip / test monkeypatch takes
#    effect without a re-import) ─────────────────────────────────────────────
def _env_bool(name: str, default: bool) -> bool:
    v = os.getenv(name)
    if v is None:
        return default
    return v.strip().lower() not in ("0", "false", "no", "off", "")


def _env_int(name: str, default: int) -> int:
    try:
        return int(os.getenv(name, str(default)))
    except (TypeError, ValueError):
        return default


def _env_float(name: str, default: float) -> float:
    try:
        return float(os.getenv(name, str(default)))
    except (TypeError, ValueError):
        return default


def cache_enabled() -> bool:
    return _env_bool("SEARCH_CACHE_ENABLED", True)


def backoff_enabled() -> bool:
    return _env_bool("SEARCH_BACKOFF_ENABLED", True)


def _ttl() -> int:
    return _env_int("SEARCH_CACHE_TTL", 600)


def _empty_ttl() -> int:
    return _env_int("SEARCH_CACHE_EMPTY_TTL", 60)


def _max_entries() -> int:
    return max(1, _env_int("SEARCH_CACHE_MAX_ENTRIES", 2000))


def _backoff_base() -> float:
    return _env_float("SEARCH_BACKOFF_BASE", 15.0)


def _backoff_cap() -> float:
    return _env_float("SEARCH_BACKOFF_CAP", 900.0)


# ── Tenant salt ────────────────────────────────────────────────────────────
def _tenant_salt() -> str:
    """Active workspace id, or "" when none is bound (shared entry)."""
    try:
        from apps.api.core.tenancy import current_workspace_var

        return current_workspace_var.get() or ""
    except Exception:  # noqa: BLE001 — tenancy import must never break search
        return ""


# ── Block-signal classification ────────────────────────────────────────────
# Defensive string match: ddgs.exceptions types vary by library version, so we
# inspect both the exception type name and its message. Empty-but-no-exception
# is *not* a block (it's legitimate no_data) and never reaches here.
_RL_PATTERNS: Tuple[str, ...] = (
    "ratelimit",
    "rate limit",
    "429",
    "403",
    "202",
    "too many",
    "blocked",
    "captcha",
    "timeout",
    "timed out",
)


def is_block_signal(exc: Optional[BaseException]) -> bool:
    if exc is None:
        return False
    text = f"{type(exc).__name__} {exc}".casefold()
    return any(p in text for p in _RL_PATTERNS)


# ── Cache ──────────────────────────────────────────────────────────────────
class _SearchCache:
    """Process-local LRU with per-entry TTL. Thread-safe (cheap dict ops)."""

    def __init__(self) -> None:
        # key -> (expires_at_monotonic, results)
        self._store: "OrderedDict[str, Tuple[float, List[dict]]]" = OrderedDict()
        self._lock = threading.Lock()

    @staticmethod
    def make_key(
        method: str,
        query: str,
        max_results,
        backends,
        salt: str = "",
    ) -> str:
        # Normalize: strip, casefold, collapse internal whitespace.
        norm = " ".join((query or "").strip().casefold().split())
        raw = f"{salt}|{method}|{norm}|{max_results}|{backends}"
        return hashlib.sha1(raw.encode("utf-8")).hexdigest()

    def get(self, key: str) -> Optional[List[dict]]:
        with self._lock:
            entry = self._store.get(key)
            if entry is None:
                return None
            expires_at, results = entry
            if time.monotonic() > expires_at:
                # TTL-expired on read.
                del self._store[key]
                return None
            self._store.move_to_end(key)
            # Shallow copy of the list so a caller can't mutate the cached value
            # (the dicts are read-only by scoring/dedup/validation).
            return list(results)

    def put(self, key: str, results: List[dict], empty: bool = False) -> None:
        ttl = _empty_ttl() if empty else _ttl()
        with self._lock:
            self._store[key] = (time.monotonic() + ttl, list(results))
            self._store.move_to_end(key)
            max_n = _max_entries()
            while len(self._store) > max_n:
                self._store.popitem(last=False)  # evict oldest (LRU)

    def clear(self) -> None:
        with self._lock:
            self._store.clear()

    def __len__(self) -> int:
        with self._lock:
            return len(self._store)


# ── Per-host adaptive backoff ──────────────────────────────────────────────
class _HostBackoff:
    """Per-host exponential backoff with jitter, capped. Thread-safe."""

    def __init__(self) -> None:
        # host -> (blocked_until_monotonic, consecutive_blocks)
        self._state: dict[str, Tuple[float, int]] = {}
        self._lock = threading.Lock()

    def blocked(self, host: str) -> bool:
        with self._lock:
            st = self._state.get(host)
            if not st:
                return False
            blocked_until, _ = st
            return time.monotonic() < blocked_until

    def record(self, host: str, exc: Optional[BaseException]) -> bool:
        """Classify ``exc``; if a block signal, grow the backoff.

        Returns True when the attempt was recorded as a block.
        """
        if not is_block_signal(exc):
            return False
        cap = _backoff_cap()
        base = _backoff_base()
        with self._lock:
            _, consecutive = self._state.get(host, (0.0, 0))
            consecutive += 1
            # Clamp the exponent so 2**n can't overflow on a long block streak;
            # the result is capped anyway, and 2**30 already dwarfs any cap.
            exp = min(consecutive - 1, 30)
            base_delay = min(base * (2 ** exp), cap)
            delay = min(base_delay + random.uniform(0, 0.3 * base_delay), cap)
            self._state[host] = (time.monotonic() + delay, consecutive)
            logger.debug(
                "backoff: host=%s consecutive=%d delay=%.1fs (%s)",
                host, consecutive, delay, type(exc).__name__,
            )
            return True

    def record_success(self, host: str) -> None:
        with self._lock:
            self._state.pop(host, None)

    # — introspection (tests / observability) —
    def time_remaining(self, host: str) -> float:
        with self._lock:
            st = self._state.get(host)
            if not st:
                return 0.0
            return max(0.0, st[0] - time.monotonic())

    def consecutive(self, host: str) -> int:
        with self._lock:
            st = self._state.get(host)
            return st[1] if st else 0

    def clear(self) -> None:
        with self._lock:
            self._state.clear()


# ── Module singletons + public API ─────────────────────────────────────────
_cache = _SearchCache()
_backoff = _HostBackoff()


def make_cache_key(method: str, query: str, max_results, backends) -> str:
    return _SearchCache.make_key(method, query, max_results, backends, _tenant_salt())


def get_cached(key: str) -> Optional[List[dict]]:
    return _cache.get(key)


def put_cached(key: str, results: List[dict], empty: bool = False) -> None:
    _cache.put(key, results, empty=empty)


def host_blocked(host: str) -> bool:
    """True when ``host`` is in backoff (always False if backoff is disabled)."""
    if not backoff_enabled():
        return False
    return _backoff.blocked(host)


def record_block(host: str, exc: Optional[BaseException]) -> bool:
    if not backoff_enabled():
        return False
    return _backoff.record(host, exc)


def record_success(host: str) -> None:
    if not backoff_enabled():
        return
    _backoff.record_success(host)


def cache_size() -> int:
    return len(_cache)


def reset_all() -> None:
    """Clear cache + backoff state. Primarily for test isolation."""
    _cache.clear()
    _backoff.clear()
