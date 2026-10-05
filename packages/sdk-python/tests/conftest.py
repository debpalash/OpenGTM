"""A fake OpenGTM host for testing the SDK in-process over a socket pair."""

from __future__ import annotations

import json
import socket
import threading
import time
from pathlib import Path
from typing import Any, Callable, Optional

import pytest

from opengtm_sdk import _wire, runtime

SCHEMA_PATH = Path(__file__).resolve().parents[2] / "contracts" / "plugin-process.v1.schema.json"


def _validators():
    try:
        import jsonschema
    except ImportError:  # the schema checks are skipped without jsonschema
        return None
    doc = json.loads(SCHEMA_PATH.read_text())
    resolver_doc = doc

    def make(def_name: str):
        schema = {"$ref": f"#/$defs/{def_name}", "$defs": resolver_doc["$defs"]}
        return jsonschema.Draft202012Validator(schema)

    return {"in": make("PluginMessage"), "out": make("HostMessage")}


class FakeHost:
    """Runs ``runtime.serve`` on a thread and plays the host's side."""

    def __init__(
        self,
        handlers: dict[str, Callable[..., Any]],
        *,
        kind: str,
        inputs: Optional[dict[str, Any]] = None,
        secrets: Optional[dict[str, str]] = None,
        fetch: Optional[Callable[[dict[str, Any]], dict[str, Any]]] = None,
        max_frame: int = 1 << 20,
        timeout: float = 20,
    ) -> None:
        self.frames: list[dict[str, Any]] = []  # frames from the plugin
        self.sent: list[dict[str, Any]] = []  # frames from the host
        self.exit_code: Optional[int] = None
        self._validators = _validators()
        self._fetch = fetch
        a, b = socket.socketpair()
        self._conn = _wire.Connection(a)
        self._plugin_conn = _wire.Connection(b)
        self._init = {
            "type": "init",
            "protocol": 1,
            "run_id": "run-1",
            "plugin": {"name": "test_plugin", "version": "0.1.0", "kind": kind},
            "inputs": inputs or {},
            "secrets": secrets or {},
            "limits": {
                "timeout_seconds": timeout,
                "max_pages": 10,
                "max_response_bytes": 1 << 20,
                "memory_mb": 64,
                "max_frame_bytes": max_frame,
            },
            "deadline_unix_ms": int((time.time() + timeout) * 1000),
        }
        self._thread = threading.Thread(target=self._serve, args=(handlers,), daemon=True)
        self._handlers = handlers

    def _serve(self, handlers: dict[str, Callable[..., Any]]) -> None:
        self.exit_code = runtime.serve(self._plugin_conn, handlers)

    def _record(self, frame: dict[str, Any], direction: str) -> None:
        if self._validators is not None:
            errors = list(self._validators[direction].iter_errors(frame))
            assert not errors, f"{direction} frame violates the contract: {errors[0].message}\n{frame}"

    def _send(self, frame: dict[str, Any]) -> None:
        self._record(frame, "out")
        self.sent.append(frame)
        self._conn.send(frame)

    def run(self, *, on_frame: Optional[Callable[[dict[str, Any]], None]] = None) -> dict[str, Any]:
        """Drive one run to its terminal frame and return it."""
        self._thread.start()
        hello = self._conn.recv()
        assert hello is not None and hello["type"] == "hello"
        self._record(hello, "in")
        self.frames.append(hello)
        assert hello["protocols"] == [1]
        self._send(self._init)
        terminal = None
        while True:
            frame = self._conn.recv()
            if frame is None:
                break
            self._record(frame, "in")
            self.frames.append(frame)
            if frame["type"] == "fetch" and self._fetch is not None:
                reply = {"type": "fetch_result", "id": frame["id"], **self._fetch(frame)}
                self._send(reply)
            if on_frame is not None:
                on_frame(frame)
            if frame["type"] in ("result", "failure"):
                terminal = frame
                break
        self._thread.join(timeout=10)
        self._conn.close()
        assert terminal is not None, f"plugin ended without a terminal frame: {self.frames}"
        return terminal

    def cancel(self, reason: str = "cancelled") -> None:
        self._send({"type": "cancel", "reason": reason, "grace_ms": 1000})

    def send(self, frame: dict[str, Any]) -> None:
        self._conn.send(frame)

    def records(self) -> list[dict[str, Any]]:
        out: list[dict[str, Any]] = []
        for f in self.frames:
            if f["type"] in ("records", "result"):
                out.extend(f.get("records", []))
        return out


@pytest.fixture
def host():
    return FakeHost


def response(status: int = 200, body: Any = None, **extra: Any) -> dict[str, Any]:
    if not isinstance(body, str):
        body = json.dumps(body if body is not None else {})
    return {"status": status, "headers": {"content-type": "application/json"}, "body": body, **extra}
