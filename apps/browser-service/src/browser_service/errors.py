"""Error envelope. Messages are fixed per code so no upstream text (page
content, library exception strings, URLs with tokens) ever reaches the caller;
details go to the service log only."""

from __future__ import annotations

ERRORS: dict[str, tuple[int, str]] = {
    "unauthorized": (401, "missing or invalid bearer token"),
    "bad_request": (400, "invalid request"),
    "not_found": (404, "no such endpoint"),
    "method_not_allowed": (405, "method not allowed"),
    "too_large": (413, "request or response exceeds the size cap"),
    "egress_blocked": (403, "destination is not allowed"),
    "busy": (429, "browser capacity exhausted, retry later"),
    "timeout": (504, "deadline exceeded"),
    "navigation_failed": (502, "the page could not be loaded"),
    "upstream_error": (502, "the upstream service failed"),
    "engine_unavailable": (503, "the requested engine is not available"),
    "browser_unavailable": (503, "the browser is not available"),
    "internal": (500, "internal error"),
}


class ServiceError(Exception):
    def __init__(self, code: str) -> None:
        if code not in ERRORS:
            code = "internal"
        super().__init__(code)
        self.code = code
        self.status, self.message = ERRORS[code]

    def body(self) -> dict:
        return {"error": {"code": self.code, "message": self.message}}
