"""Handler registration and the run loop that talks to the host."""

from __future__ import annotations

import asyncio
import inspect
import os
import signal
import socket
import sys
import threading
import traceback
import _thread
from typing import Any, Callable, Optional, TypeVar, overload

from . import _wire
from .context import Context
from .errors import Cancelled, FetchError, NoResult, PluginError
from .types import Record

__version__ = "0.1.0"
SDK_NAME = "opengtm-sdk"

KINDS = ("provider", "scraper", "function", "tool")
F = TypeVar("F", bound=Callable[..., Any])

_REGISTRY: dict[str, Callable[..., Any]] = {}

# Records are streamed in batches so a scraper's output is not bounded by one
# frame: flush at this many records or this many encoded bytes.
_BATCH_RECORDS = 200
_BATCH_BYTES = 1 << 20


def _register(kind: str, fn: Optional[F]) -> Any:
    def deco(f: F) -> F:
        if kind in _REGISTRY and _REGISTRY[kind] is not f:
            raise RuntimeError(f"a {kind} handler is already registered ({_REGISTRY[kind].__name__})")
        _REGISTRY[kind] = f
        return f

    return deco(fn) if fn is not None else deco


@overload
def provider(fn: F) -> F: ...
@overload
def provider() -> Callable[[F], F]: ...
def provider(fn: Optional[F] = None) -> Any:
    """Register the plugin's provider handler.

    The handler receives the manifest inputs as keyword arguments and returns a
    dict of fields (or a :class:`Record`), or ``None``/raises ``NoResult`` when
    it has no answer::

        @provider
        def enrich(ctx, domain: str):
            ...
            return {"company_size": 250}
    """
    return _register("provider", fn)


@overload
def scraper(fn: F) -> F: ...
@overload
def scraper() -> Callable[[F], F]: ...
def scraper(fn: Optional[F] = None) -> Any:
    """Register the plugin's scraper handler: return or yield records
    (dicts or :class:`Record`). Generators and ``async`` generators work."""
    return _register("scraper", fn)


@overload
def function(fn: F) -> F: ...
@overload
def function() -> Callable[[F], F]: ...
def function(fn: Optional[F] = None) -> Any:
    """Register a workbook function: the return value becomes ``{"result": value}``."""
    return _register("function", fn)


@overload
def tool(fn: F) -> F: ...
@overload
def tool() -> Callable[[F], F]: ...
def tool(fn: Optional[F] = None) -> Any:
    """Register an agent tool: the return value becomes ``{"output": value}``."""
    return _register("tool", fn)


def _harden() -> None:
    """Make this process non-dumpable (Linux).

    Other processes of the same OS user (another plugin running at the same
    time) can then no longer read ``/proc/<pid>/environ`` or ``/proc/<pid>/mem``
    or ptrace this one. Secrets arrive in the init frame, so they live only in
    this process's memory. Set ``OPENGTM_SDK_DUMPABLE=1`` to keep the process
    attachable while debugging.
    """
    if not sys.platform.startswith("linux") or os.environ.get("OPENGTM_SDK_DUMPABLE") == "1":
        return
    try:
        import ctypes

        ctypes.CDLL(None, use_errno=True).prctl(4, 0, 0, 0, 0)  # PR_SET_DUMPABLE
    except Exception:  # noqa: BLE001 - hardening is best effort
        pass


def _bind(handler: Callable[..., Any], ctx: Context) -> dict[str, Any]:
    """Map manifest inputs onto the handler's parameters."""
    sig = inspect.signature(handler)
    params = list(sig.parameters.values())
    takes_kwargs = any(p.kind is inspect.Parameter.VAR_KEYWORD for p in params)
    names = {p.name for p in params if p.kind in (p.POSITIONAL_OR_KEYWORD, p.KEYWORD_ONLY)}
    kwargs = {k: v for k, v in ctx.inputs.items() if takes_kwargs or k in names}
    missing = [
        p.name
        for p in params[1:]  # params[0] is ctx
        if p.kind in (p.POSITIONAL_OR_KEYWORD, p.KEYWORD_ONLY)
        and p.default is inspect.Parameter.empty
        and p.name not in kwargs
    ]
    if missing:
        from .errors import InvalidInput

        raise InvalidInput(f"missing required input(s): {', '.join(missing)}")
    return kwargs


def _as_record(item: Any) -> Record:
    if isinstance(item, Record):
        return item
    if isinstance(item, dict):
        return Record(fields=item)
    raise TypeError(f"records must be dicts or Record, got {type(item).__name__}")


def _record_frame(r: Record) -> dict[str, Any]:
    out: dict[str, Any] = {"fields": r.fields}
    if r.evidence is not None:
        out["evidence"] = r.evidence
    return out


class _Sink:
    """Collects records and streams full batches to the host."""

    def __init__(self, ctx: Context) -> None:
        self.ctx = ctx
        self.batch: list[dict[str, Any]] = []
        self.size = 0
        self.count = 0

    def add(self, r: Record) -> None:
        self.ctx.check_cancelled()
        frame = _record_frame(r)
        n = len(_wire.encode(frame)) + 1
        limit = (self.ctx._conn.max_frame if self.ctx._conn else _wire.DEFAULT_MAX_FRAME) - 4096
        if n > limit:
            raise PluginError(f"a record of {n} bytes does not fit one frame", code="output_too_large")
        if self.batch and (len(self.batch) >= _BATCH_RECORDS or self.size + n > min(_BATCH_BYTES, limit)):
            self.flush()
        self.batch.append(frame)
        self.size += n
        self.count += 1

    def flush(self) -> None:
        if self.batch:
            self.ctx._send({"type": "records", "records": self.batch})
            self.batch, self.size = [], 0


async def _collect_async(it: Any, sink: _Sink) -> None:
    if inspect.isawaitable(it):
        it = await it
    if hasattr(it, "__aiter__"):
        async for item in it:
            sink.add(_as_record(item))
        return
    _collect(it, sink)


def _collect(it: Any, sink: _Sink) -> None:
    if it is None:
        return
    if isinstance(it, (dict, Record)):
        sink.add(_as_record(it))
        return
    for item in it:
        sink.add(_as_record(item))


def _emit(kind: str, value: Any, sink: _Sink) -> Optional[str]:
    """Turn a handler's return value into records; returns a provider error."""
    if kind == "provider":
        if value is None or (isinstance(value, dict) and not value):
            return "no_data"
        sink.add(_as_record(value))
    elif kind == "function":
        sink.add(Record(fields={"result": value}))
    elif kind == "tool":
        sink.add(Record(fields={"output": value}))
    elif kind == "scraper":
        _collect(value, sink)
    return None


def _run_handler(kind: str, handler: Callable[..., Any], ctx: Context, sink: _Sink) -> Optional[str]:
    """Execute the handler; returns a provider error string or None."""
    kwargs = _bind(handler, ctx)
    is_async = inspect.iscoroutinefunction(handler) or inspect.isasyncgenfunction(handler)
    try:
        if is_async:
            return _run_async(kind, handler, ctx, sink, kwargs)
        return _emit(kind, _interruptible(ctx, lambda: handler(ctx, **kwargs)), sink)
    except NoResult as e:
        return e.reason


def _interruptible(ctx: Context, call: Callable[[], Any]) -> Any:
    """Run a synchronous handler so a cancel request can interrupt it.

    On POSIX the reader thread sends SIGUSR1 to the main thread; the handler
    raises ``Cancelled`` there, which also wakes ``time.sleep`` and blocking
    socket calls. Elsewhere it falls back to ``KeyboardInterrupt``, which stops
    pure-Python loops only. Either way the host kills the process group after
    its grace period if the plugin does not stop.
    """
    if threading.current_thread() is not threading.main_thread():
        return call()  # cannot interrupt a non-main thread; cancellation is cooperative
    state = {"active": True}
    lock = threading.Lock()
    main_id = threading.get_ident()
    use_signal = hasattr(signal, "pthread_kill") and hasattr(signal, "SIGUSR1")
    previous = None

    def on_signal(signum: int, frame: Any) -> None:
        raise Cancelled(ctx._cancel_reason or "cancelled")

    if use_signal:
        previous = signal.signal(signal.SIGUSR1, on_signal)

    def interrupt() -> None:
        with lock:
            if not state["active"]:
                return
            if use_signal:
                signal.pthread_kill(main_id, signal.SIGUSR1)
            else:
                _thread.interrupt_main()

    ctx._on_cancel.append(interrupt)
    try:
        return call()
    except KeyboardInterrupt:
        if ctx.cancelled:
            raise Cancelled(ctx._cancel_reason) from None
        raise
    finally:
        with lock:
            state["active"] = False
        if use_signal:
            signal.signal(signal.SIGUSR1, previous if previous is not None else signal.SIG_DFL)


def _run_async(kind: str, handler: Callable[..., Any], ctx: Context, sink: _Sink, kwargs: dict[str, Any]) -> Optional[str]:
    async def main() -> Optional[str]:
        task = asyncio.current_task()
        loop = asyncio.get_running_loop()
        ctx._on_cancel.append(lambda: loop.call_soon_threadsafe(task.cancel))
        value = handler(ctx, **kwargs)
        if kind == "scraper":
            await _collect_async(value, sink)
            return None
        if inspect.isawaitable(value):
            value = await value
        return _emit(kind, value, sink)

    try:
        return asyncio.run(main())
    except asyncio.CancelledError:
        raise Cancelled(ctx._cancel_reason or "cancelled") from None


def _failure(exc: BaseException) -> dict[str, Any]:
    """Translate an exception into a protocol failure frame."""
    if isinstance(exc, Cancelled):
        return {"type": "failure", "code": "cancelled", "message": str(exc) or "cancelled", "retryable": False}
    if isinstance(exc, PluginError):
        f: dict[str, Any] = {
            "type": "failure",
            "code": exc.code,
            "message": exc.message or exc.code,
            "retryable": bool(exc.retryable),
        }
        if exc.details:
            f["details"] = exc.details
        if isinstance(exc, FetchError):
            f["details"] = {**f.get("details", {}), "fetch_code": exc.fetch_code}
        return f
    tb = "".join(traceback.format_exception(type(exc), exc, exc.__traceback__))
    return {
        "type": "failure",
        "code": "plugin_exception",
        "message": f"{type(exc).__name__}: {exc}",
        "retryable": False,
        "details": {"traceback": tb[-4000:]},
    }


def serve(
    conn: _wire.Connection,
    handlers: Optional[dict[str, Callable[..., Any]]] = None,
    *,
    exit_on_disconnect: bool = True,
) -> int:
    """Speak the protocol on ``conn``: hello, init, run the handler, report.

    Returns the process exit code. ``run()`` calls this with the inherited
    socket; tests call it with one end of a socket pair.
    """
    handlers = _REGISTRY if handlers is None else handlers
    conn.send(
        {
            "type": "hello",
            "protocols": [_wire.PROTOCOL],
            "sdk": {"name": SDK_NAME, "version": __version__, "language": "python"},
            "kinds": sorted(handlers),
        }
    )
    init = conn.recv()
    if init is None:
        return 0  # the host went away before assigning work
    if init.get("type") != "init":
        conn.send({"type": "failure", "code": "protocol_error", "message": f"expected init, got {init.get('type')!r}"})
        return 70
    conn.max_frame = int((init.get("limits") or {}).get("max_frame_bytes") or conn.max_frame)
    ctx = Context(conn, init)

    finished = threading.Event()

    def reader() -> None:
        while True:
            try:
                msg = conn.recv()
            except Exception:  # noqa: BLE001 - a broken socket means the host is gone
                msg = None
            if msg is None:
                if finished.is_set():
                    return
                # The host closed the connection mid-run: nobody is waiting for us.
                ctx._cancel_now("host closed the connection")
                if exit_on_disconnect:
                    os._exit(70)
                return
            kind = msg.get("type")
            if kind == "fetch_result":
                ctx._dispatch_reply(msg)
            elif kind == "cancel":
                ctx._cancel_now(str(msg.get("reason") or "cancelled"))
            # Unknown host messages are ignored: the protocol grows by adding types.

    threading.Thread(target=reader, name="opengtm-sdk-reader", daemon=True).start()

    handler = handlers.get(ctx.kind)
    try:
        if handler is None:
            raise PluginError(
                f"this plugin registers {sorted(handlers) or 'no handlers'} but the manifest kind is {ctx.kind!r}",
                code="plugin_exception",
            )
        sink = _Sink(ctx)
        provider_error = _run_handler(ctx.kind, handler, ctx, sink)
        result: dict[str, Any] = {"type": "result", "records": sink.batch}
        if provider_error:
            result["provider_error"] = provider_error
        if ctx.cost_usd is not None:
            result["cost_usd"] = ctx.cost_usd
        if ctx.pages is not None:
            result["pages"] = ctx.pages
        if ctx.stopped:
            result["stopped"] = ctx.stopped
        finished.set()  # before the send: the host may hang up the moment it has our result
        conn.send(result)
    except BaseException as exc:  # noqa: BLE001 - everything becomes a structured failure
        if isinstance(exc, (SystemExit, GeneratorExit)):
            raise
        finished.set()
        try:
            conn.send(_failure(exc))
        except Exception:  # noqa: BLE001
            return 70
    return 0


def run(*, exit: bool = True) -> int:
    """Run the registered handler against the host that started this process.

    Put ``run()`` at the bottom of the plugin file::

        if __name__ == "__main__":
            run()
    """
    _harden()
    fd = int(os.environ.get("OPENGTM_PLUGIN_FD", "3"))
    try:
        sock = socket.socket(fileno=fd)
    except OSError as e:
        print(
            f"opengtm_sdk: file descriptor {fd} is not the host's control socket ({e}).\n"
            "Plugins are started by OpenGTM; test them with `opengtm plugin test <dir>`.",
            file=sys.stderr,
        )
        if exit:
            sys.exit(2)
        return 2
    code = serve(_wire.Connection(sock))
    if exit:
        sys.exit(code)
    return code


__all__ = ["provider", "scraper", "function", "tool", "run", "serve", "Record", "__version__"]
