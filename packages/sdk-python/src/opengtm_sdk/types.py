"""Value types shared by plugins and the SDK."""

from __future__ import annotations

import json as _json
from dataclasses import dataclass, field
from typing import Any, Mapping, Optional


@dataclass
class Record:
    """One output row: normalized ``fields`` plus the plugin's own ``evidence``.

    Handlers may yield plain dicts instead; they are treated as ``fields``.
    The host adds the evidence it observed itself (every fetch it performed).
    """

    fields: dict[str, Any]
    evidence: Optional[dict[str, Any]] = None


@dataclass(frozen=True)
class Limits:
    """Manifest limits the host enforces on this run."""

    timeout_seconds: float
    max_pages: int
    max_response_bytes: int
    memory_mb: int


@dataclass(frozen=True)
class Response:
    """The result of ``ctx.fetch``."""

    status: int
    headers: Mapping[str, str]
    body: bytes
    evidence: Optional[Mapping[str, Any]] = None

    @property
    def ok(self) -> bool:
        return 200 <= self.status < 300

    @property
    def text(self) -> str:
        return self.body.decode("utf-8", errors="replace")

    def json(self) -> Any:
        """Parse the body as JSON (raises ``ValueError`` when it is not)."""
        return _json.loads(self.body)


@dataclass
class _Pending:
    """A fetch waiting for its reply."""

    event: Any = field(default=None)
    reply: Optional[dict[str, Any]] = None
