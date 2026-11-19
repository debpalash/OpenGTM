"""Per-backend memory for the keyless search backends that ddgs fans a query out to.

ddgs builds a fresh engine set per query and swallows every non-200 answer, so before this module each query asked
every configured backend again: a backend that had just answered 429 (brave) or 403 (mojeek) was asked again on the
next query, and a backend that timed out (duckduckgo) cost a full search timeout every time. This keeps what each
backend last answered and leaves a backend alone while it is cooling down.

This is not evasion: nothing here changes how a request looks, rotates an address or retries a block. It only stops
asking a backend that has already said no. The honest answer to blocked keyless search is a search API key
(Brave, Serper, Tavily, ...) or a SearXNG instance of your own; see docs/operations/search-backends.md.
"""

from __future__ import annotations

import logging
import threading
import time
from collections.abc import Callable

logger = logging.getLogger("browser_service.scrapers.backend_health")

# An answer that means "you are blocked or throttled": ask again much later.
BLOCK_STATUSES = frozenset({202, 403, 429, 503})
BLOCK_COOLDOWN_S = 300.0
BLOCK_COOLDOWN_MAX_S = 3600.0
# A timeout or any other failure may be a bad moment: ask again soon, backing off while it keeps happening.
FAIL_COOLDOWN_S = 60.0
FAIL_COOLDOWN_MAX_S = 600.0


class BackendHealth:
    def __init__(self, clock: Callable[[], float] = time.monotonic) -> None:
        self._clock = clock
        self._lock = threading.Lock()
        self._until: dict[str, float] = {}
        self._streak: dict[str, int] = {}

    def blocked(self, name: str) -> bool:
        with self._lock:
            return self._until.get(name, 0.0) > self._clock()

    def seconds_left(self, name: str) -> float:
        with self._lock:
            return max(0.0, self._until.get(name, 0.0) - self._clock())

    def all_blocked(self, names: list[str]) -> bool:
        return bool(names) and all(self.blocked(n) for n in names)

    def allowed(self, names: list[str]) -> list[str]:
        """The backends not cooling down, in the given order."""
        return [n for n in names if not self.blocked(n)]

    def record_status(self, name: str, status: int) -> None:
        if status == 200:
            self._reset(name)
        elif status in BLOCK_STATUSES:
            self._cool(name, BLOCK_COOLDOWN_S, BLOCK_COOLDOWN_MAX_S, f"HTTP {status}")
        else:
            self._cool(name, FAIL_COOLDOWN_S, FAIL_COOLDOWN_MAX_S, f"HTTP {status}")

    def record_failure(self, name: str, why: str = "timeout") -> None:
        self._cool(name, FAIL_COOLDOWN_S, FAIL_COOLDOWN_MAX_S, why)

    def _reset(self, name: str) -> None:
        with self._lock:
            self._until.pop(name, None)
            self._streak.pop(name, None)

    def _cool(self, name: str, base: float, cap: float, why: str) -> None:
        with self._lock:
            n = self._streak.get(name, 0) + 1
            self._streak[name] = n
            wait = min(cap, base * (2 ** (n - 1)))
            self._until[name] = self._clock() + wait
        logger.info("search backend %s cooling down for %.0fs (%s, %d in a row)", name, wait, why, n)


health = BackendHealth()


def install(target: BackendHealth | None = None) -> bool:
    """Wrap ddgs' engine request so it consults and feeds ``target``. Safe to call twice; returns False (and
    changes nothing) when the installed ddgs has no such hook, so an upgrade can never break search."""
    h = target or health
    try:
        from ddgs.base import BaseSearchEngine
    except Exception:  # pragma: no cover - ddgs is a hard dependency; this guards a renamed module
        return False
    original = getattr(BaseSearchEngine, "request", None)
    if original is None:
        return False
    if getattr(original, "_backend_health", None) is h:
        return True
    original = getattr(original, "_wrapped", original)

    def request(self, *args, **kwargs):
        name = getattr(self, "name", type(self).__name__)
        if h.blocked(name):
            return None  # same as an engine that returned nothing, without another request to a backend that said no
        client = getattr(self, "http_client", None)
        send = getattr(client, "request", None)
        if client is None or send is None:
            return original(self, *args, **kwargs)
        status = {"code": None}

        def watched(*a, **k):
            resp = send(*a, **k)
            status["code"] = getattr(resp, "status_code", None)
            return resp

        try:
            client.request = watched
            try:
                return original(self, *args, **kwargs)
            finally:
                client.request = send
        except Exception as exc:
            h.record_failure(name, type(exc).__name__)
            raise
        finally:
            if isinstance(status["code"], int):
                h.record_status(name, status["code"])

    request._backend_health = h  # type: ignore[attr-defined]
    request._wrapped = original  # type: ignore[attr-defined]
    BaseSearchEngine.request = request  # type: ignore[method-assign]
    return True
