"""The ``Context`` handed to every plugin handler."""

from __future__ import annotations

import asyncio
import base64
import datetime as _dt
import threading
import time
from typing import Any, Callable, Mapping, Optional
from urllib.parse import urlencode

from . import _wire
from .errors import AuthFailed, Cancelled, FetchError
from .types import Limits, Response, _Pending


class Context:
    """Everything a handler may use: inputs, secrets, fetch, logging, progress.

    A plugin has no other way out of its process: network access goes through
    :meth:`fetch` (checked against the manifest's ``capabilities.network`` and
    the host's SSRF guard, robots.txt and rate limits), and secrets come from
    :meth:`secret` (only those declared in ``capabilities.secrets``).
    """

    def __init__(self, conn: Optional[_wire.Connection], init: Mapping[str, Any]) -> None:
        plugin = init.get("plugin") or {}
        limits = init.get("limits") or {}
        self.run_id: str = init.get("run_id", "")
        self.plugin_name: str = plugin.get("name", "")
        self.plugin_version: str = plugin.get("version", "")
        self.kind: str = plugin.get("kind", "")
        self.inputs: dict[str, Any] = dict(init.get("inputs") or {})
        self.config: dict[str, Any] = dict(init.get("config") or {})
        self.limits = Limits(
            timeout_seconds=float(limits.get("timeout_seconds", 20)),
            max_pages=int(limits.get("max_pages", 10)),
            max_response_bytes=int(limits.get("max_response_bytes", 5 << 20)),
            memory_mb=int(limits.get("memory_mb", 64)),
        )
        self.deadline: float = float(init.get("deadline_unix_ms", 0)) / 1000.0
        # CONNECT proxy for libraries that open their own TLS connections (vendor
        # SDKs, a Playwright browser); None when the host offers none. Only hosts
        # in capabilities.network are reachable through it. HTTPS_PROXY is set too.
        self.proxy_url: Optional[str] = (init.get("proxy") or {}).get("url")
        # Set by the plugin when it wants to report them.
        self.cost_usd: Optional[float] = None
        self.pages: Optional[int] = None
        self.stopped: str = ""

        self._conn = conn
        self._secrets: dict[str, str] = dict(init.get("secrets") or {})
        self._cancel = threading.Event()
        self._cancel_reason = ""
        self._lock = threading.Lock()
        self._pending: dict[int, _Pending] = {}
        self._next_id = 0
        self._on_cancel: list[Callable[[], None]] = []

    # -- secrets -------------------------------------------------------------

    def secret(self, name: str) -> str:
        """Return a declared secret, or raise ``AuthFailed`` when it is not set."""
        value = self._secrets.get(name)
        if not value:
            raise AuthFailed(f"secret {name} is not configured for this workspace")
        return value

    # -- cancellation --------------------------------------------------------

    @property
    def cancelled(self) -> bool:
        """True once the host asked the run to stop. Long loops should poll it."""
        return self._cancel.is_set()

    def check_cancelled(self) -> None:
        """Raise ``Cancelled`` when the host asked the run to stop."""
        if self._cancel.is_set():
            raise Cancelled(self._cancel_reason or "cancelled")

    def time_left(self) -> float:
        """Seconds until the host's deadline (never negative)."""
        return max(0.0, self.deadline - time.time()) if self.deadline else float("inf")

    def _cancel_now(self, reason: str) -> None:
        self._cancel_reason = reason
        self._cancel.set()
        with self._lock:
            pending = list(self._pending.values())
            callbacks = list(self._on_cancel)
        for p in pending:
            p.event.set()
        for cb in callbacks:
            try:
                cb()
            except Exception:  # noqa: BLE001 - never let a hook break the reader
                pass

    # -- host calls ----------------------------------------------------------

    def _send(self, msg: dict[str, Any]) -> None:
        if self._conn is None:
            raise RuntimeError("this context has no host connection")
        self._conn.send(msg)

    def _dispatch_reply(self, msg: dict[str, Any]) -> None:
        with self._lock:
            pending = self._pending.get(int(msg.get("id", -1)))
        if pending is not None:
            pending.reply = msg
            pending.event.set()

    def fetch(
        self,
        url: str,
        *,
        method: str = "GET",
        headers: Optional[Mapping[str, str]] = None,
        params: Optional[Mapping[str, Any]] = None,
        body: Any = None,
        json: Any = None,
    ) -> Response:
        """Perform one HTTP request through the host's guarded egress client.

        ``body`` may be ``str`` or ``bytes``; ``json`` is encoded and sets the
        content type. Raises ``FetchError`` when the host refuses or the request
        fails; an HTTP error status is a normal :class:`Response`.
        """
        self.check_cancelled()
        if params:
            url += ("&" if "?" in url else "?") + urlencode(params, doseq=True)
        hdrs = dict(headers or {})
        msg: dict[str, Any] = {"type": "fetch", "method": method.upper(), "url": url}
        if json is not None:
            body = _wire.encode(json).decode("utf-8")
            if not any(k.lower() == "content-type" for k in hdrs):
                hdrs["Content-Type"] = "application/json"
        if hdrs:
            msg["headers"] = hdrs
        if isinstance(body, str):
            msg["body"] = body
        elif body is not None:
            msg["body_base64"] = base64.b64encode(bytes(body)).decode("ascii")

        pending = _Pending(event=threading.Event())
        with self._lock:
            self._next_id += 1
            fetch_id = self._next_id
            self._pending[fetch_id] = pending
        msg["id"] = fetch_id
        try:
            self._send(msg)
            pending.event.wait()
        finally:
            with self._lock:
                self._pending.pop(fetch_id, None)
        self.check_cancelled()
        reply = pending.reply
        if reply is None:
            raise FetchError("fetch_failed", "the host closed the connection")
        if reply.get("error"):
            err = reply["error"]
            raise FetchError(str(err.get("code", "fetch_failed")), str(err.get("message", "")))
        raw = base64.b64decode(reply["body_base64"]) if reply.get("body_base64") else (reply.get("body") or "").encode("utf-8")
        return Response(
            status=int(reply.get("status", 0)),
            headers=dict(reply.get("headers") or {}),
            body=raw,
            evidence=reply.get("evidence"),
        )

    async def afetch(self, url: str, **kwargs: Any) -> Response:
        """Awaitable :meth:`fetch` for ``async def`` handlers."""
        loop = asyncio.get_running_loop()
        return await loop.run_in_executor(None, lambda: self.fetch(url, **kwargs))

    # -- reporting -----------------------------------------------------------

    def log(self, message: str, level: str = "info") -> None:
        """Send a log line to the host (bounded and redacted there)."""
        if self._conn is not None:
            self._conn.send({"type": "log", "level": level, "message": str(message)})

    def progress(
        self,
        *,
        pages: Optional[int] = None,
        records: Optional[int] = None,
        fraction: Optional[float] = None,
        message: str = "",
    ) -> None:
        """Report progress (best effort; the host rate-limits it)."""
        if self._conn is None:
            return
        msg: dict[str, Any] = {"type": "progress"}
        if pages is not None:
            msg["pages"] = pages
        if records is not None:
            msg["records"] = records
        if fraction is not None:
            msg["fraction"] = fraction
        if message:
            msg["message"] = message
        self._conn.send(msg)

    def add_cost(self, usd: float) -> None:
        """Add to the run's reported cost in US dollars."""
        self.cost_usd = (self.cost_usd or 0.0) + float(usd)

    def evidence(self, url: Optional[str] = None, **extra: Any) -> dict[str, Any]:
        """Build the plugin's own evidence for a record: a source and a time."""
        ev: dict[str, Any] = {"fetched_at": _dt.datetime.now(_dt.timezone.utc).isoformat()}
        if url:
            ev["source_url"] = url
        ev.update(extra)
        return ev
