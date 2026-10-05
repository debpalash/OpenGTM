"""Wire protocol v1: length-prefixed JSON frames over an inherited socket.

A frame is a 4-byte big-endian length followed by that many bytes of UTF-8
JSON (one object with a ``"type"``). See docs/plugins/process-abi.md.
"""

from __future__ import annotations

import base64
import dataclasses
import datetime
import decimal
import json
import socket
import struct
import threading
from typing import Any, Optional

PROTOCOL = 1
DEFAULT_MAX_FRAME = 16 << 20


def _default(obj: Any) -> Any:
    """Serialize the few non-JSON types plugins reasonably return."""
    if dataclasses.is_dataclass(obj) and not isinstance(obj, type):
        return dataclasses.asdict(obj)
    if isinstance(obj, (datetime.datetime, datetime.date)):
        return obj.isoformat()
    if isinstance(obj, decimal.Decimal):
        return float(obj)
    if isinstance(obj, (set, frozenset)):
        return sorted(obj, key=repr)
    if isinstance(obj, (bytes, bytearray)):
        return base64.b64encode(bytes(obj)).decode("ascii")
    raise TypeError(f"{type(obj).__name__} is not JSON serializable")


def encode(msg: dict[str, Any]) -> bytes:
    return json.dumps(
        msg, separators=(",", ":"), ensure_ascii=False, allow_nan=False, default=_default
    ).encode("utf-8")


class FrameError(Exception):
    """The peer sent something that is not a valid frame."""


class Connection:
    """A framed, thread-safe-for-writes view of a socket."""

    def __init__(self, sock: socket.socket, max_frame: int = DEFAULT_MAX_FRAME) -> None:
        self._sock = sock
        self._rfile = sock.makefile("rb", buffering=65536)
        self._wlock = threading.Lock()
        self.max_frame = max_frame

    def send(self, msg: dict[str, Any]) -> None:
        body = encode(msg)
        if len(body) > self.max_frame:
            raise FrameError(f"frame of {len(body)} bytes exceeds the {self.max_frame} byte limit")
        data = struct.pack(">I", len(body)) + body
        with self._wlock:
            self._sock.sendall(data)

    def recv(self) -> Optional[dict[str, Any]]:
        """Return the next frame, or None when the host closed the connection."""
        hdr = self._rfile.read(4)
        if not hdr:
            return None
        if len(hdr) < 4:
            raise FrameError("truncated frame header")
        (n,) = struct.unpack(">I", hdr)
        if n == 0 or n > self.max_frame:
            raise FrameError(f"invalid frame length {n}")
        body = self._rfile.read(n)
        if len(body) < n:
            raise FrameError("truncated frame body")
        msg = json.loads(body.decode("utf-8"))
        if not isinstance(msg, dict) or not isinstance(msg.get("type"), str):
            raise FrameError("frame is not an object with a type")
        return msg

    def close(self) -> None:
        try:
            self._rfile.close()
        finally:
            try:
                self._sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            self._sock.close()
