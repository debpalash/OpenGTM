"""Exceptions a plugin can raise to report structured failures to the host.

The host decides whether to retry from ``retryable``; the defaults match the
protocol's error-code table (docs/plugins/process-abi.md).
"""

from __future__ import annotations

from typing import Any, Optional


class PluginError(Exception):
    """Base class: a failure with a protocol error code."""

    code = "plugin_exception"
    retryable = False

    def __init__(
        self,
        message: str = "",
        *,
        code: Optional[str] = None,
        retryable: Optional[bool] = None,
        details: Optional[dict[str, Any]] = None,
    ) -> None:
        super().__init__(message)
        self.message = message
        if code is not None:
            self.code = code
        if retryable is not None:
            self.retryable = retryable
        self.details = details or {}


class Retryable(PluginError):
    """A transient failure (upstream outage, throttling): retrying may work."""

    code = "upstream_error"
    retryable = True


class Permanent(PluginError):
    """A failure retrying cannot fix."""

    code = "plugin_exception"
    retryable = False


class InvalidInput(Permanent):
    """The inputs cannot produce a result."""

    code = "invalid_input"


class AuthFailed(Permanent):
    """Credentials are missing or rejected."""

    code = "auth_failed"


class RateLimited(Retryable):
    """The data source throttled the request."""

    code = "rate_limited"


class NoResult(Exception):
    """A provider has no answer for these inputs (not a failure).

    ``reason`` is recorded as the run's provider error, for example
    ``"not_found"``. Equivalent to returning ``None`` from a provider.
    """

    def __init__(self, reason: str = "no_data") -> None:
        super().__init__(reason)
        self.reason = reason


class Cancelled(BaseException):
    """The host cancelled the run (cancel request, timeout or shutdown).

    Derives from BaseException so ``except Exception`` in plugin code does not
    swallow it.
    """


class FetchError(PluginError):
    """``ctx.fetch`` failed at the host (denied, blocked, timeout, ...).

    ``code`` is the host's fetch error code. Transient codes make the error
    retryable when it escapes the handler.
    """

    TRANSIENT = {"timeout", "rate_limited", "fetch_failed", "network_unavailable"}
    PROTOCOL_CODE = {
        "timeout": "timeout",
        "rate_limited": "rate_limited",
        "fetch_failed": "upstream_error",
        "network_unavailable": "upstream_error",
    }

    def __init__(self, code: str, message: str) -> None:
        super().__init__(f"{code}: {message}", code=code, retryable=code in self.TRANSIENT)
        self.fetch_code = code
        self.fetch_message = message
        self.code = self.PROTOCOL_CODE.get(code, code)
