from __future__ import annotations

import asyncio
import json
import threading
import time

import pytest

from conftest import response
from opengtm_sdk import (
    AuthFailed,
    Context,
    InvalidInput,
    NoResult,
    Permanent,
    RateLimited,
    Record,
    Retryable,
    function,
    provider,
    scraper,
    tool,
)


def run_provider(host, fn, inputs=None, **kw):
    h = host({"provider": fn}, kind="provider", inputs=inputs, **kw)
    return h, h.run()


def test_provider_in_a_few_lines(host):
    @provider
    def enrich(ctx: Context, domain: str):
        r = ctx.fetch(
            "https://api.example.com/v1/companies",
            params={"domain": domain},
            headers={"X-Api-Key": ctx.secret("API_KEY")},
        )
        return {"employees": r.json()["employees"], "domain": domain}

    seen = {}

    def fetch(frame):
        seen.update(frame)
        return response(body={"employees": 250})

    h = host({"provider": enrich}, kind="provider", inputs={"domain": "acme.example"}, secrets={"API_KEY": "k"}, fetch=fetch)
    final = h.run()
    assert final["type"] == "result"
    assert final["records"] == [{"fields": {"employees": 250, "domain": "acme.example"}}]
    assert seen["url"] == "https://api.example.com/v1/companies?domain=acme.example"
    assert seen["method"] == "GET" and seen["headers"] == {"X-Api-Key": "k"}
    assert h.exit_code == 0


def test_hello_declares_protocol_and_kinds(host):
    h, _ = run_provider(host, lambda ctx: {"a": 1})
    hello = h.frames[0]
    assert hello["protocols"] == [1]
    assert hello["sdk"]["name"] == "opengtm-sdk" and hello["sdk"]["language"] == "python"
    assert hello["kinds"] == ["provider"]


def test_provider_no_answer_is_not_a_failure(host):
    _, final = run_provider(host, lambda ctx: None)
    assert final == {"type": "result", "records": [], "provider_error": "no_data"}

    def raises(ctx):
        raise NoResult("not_found")

    _, final = run_provider(host, raises)
    assert final["provider_error"] == "not_found" and final["records"] == []


def test_inputs_are_bound_by_name_and_extras_ignored(host):
    def enrich(ctx, domain, limit=5):
        return {"domain": domain, "limit": limit}

    _, final = run_provider(host, enrich, inputs={"domain": "a.example", "unrelated": 1})
    assert final["records"][0]["fields"] == {"domain": "a.example", "limit": 5}

    def takes_all(ctx, **inputs):
        return inputs

    _, final = run_provider(host, takes_all, inputs={"x": 1, "y": 2})
    assert final["records"][0]["fields"] == {"x": 1, "y": 2}


def test_missing_required_input_is_a_permanent_invalid_input(host):
    _, final = run_provider(host, lambda ctx, domain: {}, inputs={})
    assert final["type"] == "failure" and final["code"] == "invalid_input"
    assert final["retryable"] is False and "domain" in final["message"]


@pytest.mark.parametrize(
    "exc, code, retryable",
    [
        (Retryable("down"), "upstream_error", True),
        (RateLimited("slow down"), "rate_limited", True),
        (Permanent("no"), "plugin_exception", False),
        (InvalidInput("bad"), "invalid_input", False),
        (AuthFailed("nope"), "auth_failed", False),
        (Permanent("custom", code="vendor_quota"), "vendor_quota", False),
        (Retryable("custom", code="vendor_busy"), "vendor_busy", True),
        (Permanent("odd", retryable=True), "plugin_exception", True),
    ],
)
def test_exceptions_become_structured_failures(host, exc, code, retryable):
    def fn(ctx):
        raise exc

    _, final = run_provider(host, fn)
    assert (final["code"], final["retryable"]) == (code, retryable)
    assert final["message"] == str(exc)


def test_unhandled_exception_is_permanent_with_a_traceback(host):
    def fn(ctx):
        return {"x": 1 / 0}

    h, final = run_provider(host, fn)
    assert final["code"] == "plugin_exception" and final["retryable"] is False
    assert "ZeroDivisionError" in final["message"]
    assert "fn" in final["details"]["traceback"]
    assert h.exit_code == 0  # the failure was delivered; the process ends cleanly


def test_non_json_output_is_a_plugin_failure_not_a_crash(host):
    _, final = run_provider(host, lambda ctx: {"x": object()})
    assert final["code"] == "plugin_exception" and "JSON" in final["message"]


def test_fetch_errors_map_to_retry_semantics(host):
    def make(code):
        def fn(ctx):
            ctx.fetch("https://api.example.com/")

        return fn

    for host_code, proto_code, retryable in [
        ("timeout", "timeout", True),
        ("rate_limited", "rate_limited", True),
        ("fetch_failed", "upstream_error", True),
        ("capability_denied", "capability_denied", False),
        ("blocked_url", "blocked_url", False),
        ("robots_disallowed", "robots_disallowed", False),
        ("too_many_fetches", "too_many_fetches", False),
    ]:
        h = host(
            {"provider": make(host_code)}, kind="provider",
            fetch=lambda frame, c=host_code: {"error": {"code": c, "message": "msg"}},
        )
        final = h.run()
        assert (final["code"], final["retryable"]) == (proto_code, retryable), host_code
        assert final["details"]["fetch_code"] == host_code


def test_fetch_response_helpers(host):
    got = {}

    def fn(ctx):
        r = ctx.fetch("https://api.example.com/x", method="post", json={"a": [1, 2]}, headers={"X-T": "1"})
        got["status"], got["ok"], got["text"], got["json"] = r.status, r.ok, r.text, r.json()
        got["headers"] = dict(r.headers)
        b = ctx.fetch("https://api.example.com/bin", body=b"\x00\x01")
        got["bin"] = b.body
        return {"done": True}

    sent = []

    def fetch(frame):
        sent.append(frame)
        if frame["url"].endswith("/bin"):
            return {"status": 200, "body_base64": "AP8="}
        return response(201, {"ok": 1}, evidence={"url": frame["url"], "status": 201, "fetched_at": "2026-01-01T00:00:00Z",
                                                     "content_type": "application/json", "bytes": 8, "sha256": "0" * 64})

    h = host({"provider": fn}, kind="provider", fetch=fetch)
    final = h.run()
    assert final["type"] == "result"
    assert got["status"] == 201 and got["ok"] and got["json"] == {"ok": 1}
    assert got["bin"] == b"\x00\xff"
    assert sent[0]["method"] == "POST" and json.loads(sent[0]["body"]) == {"a": [1, 2]}
    assert sent[0]["headers"]["Content-Type"] == "application/json" and sent[0]["headers"]["X-T"] == "1"
    assert sent[1]["body_base64"] == "AAE="


def test_missing_secret_is_auth_failed(host):
    def fn(ctx):
        return {"k": ctx.secret("NOT_CONFIGURED")}

    _, final = run_provider(host, fn)
    assert final["code"] == "auth_failed" and final["retryable"] is False
    assert "NOT_CONFIGURED" in final["message"]


def test_progress_logs_cost_pages(host):
    def fn(ctx):
        ctx.log("hello", level="debug")
        ctx.progress(pages=2, records=5, fraction=0.5, message="halfway")
        ctx.add_cost(0.25)
        ctx.add_cost(0.25)
        ctx.pages = 3
        return {"a": 1}

    h, final = run_provider(host, fn)
    types = [f["type"] for f in h.frames]
    assert "log" in types and "progress" in types
    assert final["cost_usd"] == 0.5 and final["pages"] == 3


def test_scraper_generator_batches_records(host):
    @scraper
    def scrape(ctx, n):
        for i in range(n):
            yield {"i": i}
        yield Record({"i": n}, evidence=ctx.evidence("https://x.example/", marker=1))
        ctx.stopped = "max_pages"

    h = host({"scraper": scrape}, kind="scraper", inputs={"n": 450})
    final = h.run()
    batches = [f for f in h.frames if f["type"] == "records"]
    assert len(batches) == 2 and all(len(b["records"]) <= 200 for b in batches)
    rows = h.records()
    assert [r["fields"]["i"] for r in rows] == list(range(451))
    assert rows[-1]["evidence"]["source_url"] == "https://x.example/" and rows[-1]["evidence"]["marker"] == 1
    assert final["stopped"] == "max_pages"


def test_async_scraper_with_async_fetch(host):
    async def scrape(ctx, pages):
        for p in range(pages):
            r = await ctx.afetch(f"https://x.example/p{p}")
            yield {"page": p, "len": len(r.text)}

    h = host({"scraper": scrape}, kind="scraper", inputs={"pages": 3}, fetch=lambda f: response(body="x" * 7))
    h.run()
    assert [r["fields"] for r in h.records()] == [{"page": i, "len": 7} for i in range(3)]


def test_async_provider_and_awaitable_scraper(host):
    async def enrich(ctx, v):
        await asyncio.sleep(0)
        return {"v": v}

    h = host({"provider": enrich}, kind="provider", inputs={"v": 5})
    assert h.run()["records"][0]["fields"] == {"v": 5}

    async def scrape(ctx):
        return [{"a": 1}, {"a": 2}]

    h = host({"scraper": scrape}, kind="scraper")
    h.run()
    assert [r["fields"]["a"] for r in h.records()] == [1, 2]


def test_function_and_tool_wrap_return_values(host):
    h = host({"function": lambda ctx, x: x * 2}, kind="function", inputs={"x": 21})
    assert h.run()["records"] == [{"fields": {"result": 42}}]
    h = host({"tool": lambda ctx, q: {"a": q}}, kind="tool", inputs={"q": "z"})
    assert h.run()["records"] == [{"fields": {"output": {"a": "z"}}}]


def test_manifest_kind_without_a_handler_fails_clearly(host):
    h = host({"scraper": lambda ctx: []}, kind="provider")
    final = h.run()
    assert final["code"] == "plugin_exception" and "provider" in final["message"] and "scraper" in final["message"]


def test_record_larger_than_a_frame_is_reported(host):
    def fn(ctx):
        return {"big": "x" * 5000}

    h = host({"provider": fn}, kind="provider", max_frame=4096 + 1024)
    final = h.run()
    assert final["code"] == "output_too_large" and final["retryable"] is False


def test_cancel_interrupts_a_blocked_fetch(host):
    def fn(ctx):
        return ctx.fetch("https://x.example/slow")  # the host never answers

    h = host({"provider": fn}, kind="provider")  # no fetch hook: the request stays unanswered

    def on_frame(frame):
        if frame["type"] == "fetch":
            threading.Timer(0.1, h.cancel).start()

    final = h.run(on_frame=on_frame)
    assert final["type"] == "failure" and final["code"] == "cancelled"


def test_cancel_stops_a_busy_sync_handler(host):
    # serve() runs on a non-main thread here, so interruption is cooperative:
    # the handler polls ctx.cancelled / check_cancelled like documented.
    def fn(ctx):
        ctx.log("ready")
        while True:
            ctx.check_cancelled()
            time.sleep(0.01)

    h = host({"provider": fn}, kind="provider")

    def on_frame(frame):
        if frame["type"] == "log":
            h.cancel("timeout")

    final = h.run(on_frame=on_frame)
    assert final["code"] == "cancelled" and "timeout" in final["message"]


def test_unknown_host_messages_and_fields_are_ignored(host):
    def fn(ctx):
        ctx.log("ready")
        while not ctx.cancelled:
            time.sleep(0.01)
        ctx.check_cancelled()

    h = host({"provider": fn}, kind="provider")
    h._init["future_field"] = {"x": 1}  # not in the schema: validated loosely below
    h._validators = None

    def on_frame(frame):
        if frame["type"] == "log":
            h.send({"type": "from_the_future", "n": 1})
            h.send({"type": "cancel", "reason": "cancelled", "grace_ms": 10, "extra": True})

    assert h.run(on_frame=on_frame)["code"] == "cancelled"


def test_double_registration_is_rejected():
    from opengtm_sdk import runtime

    saved = dict(runtime._REGISTRY)
    runtime._REGISTRY.clear()
    try:
        @function
        def one(ctx):
            return 1

        with pytest.raises(RuntimeError):
            @function
            def two(ctx):
                return 2
    finally:
        runtime._REGISTRY.clear()
        runtime._REGISTRY.update(saved)


def test_decorators_work_with_and_without_parentheses():
    from opengtm_sdk import runtime

    saved = dict(runtime._REGISTRY)
    runtime._REGISTRY.clear()
    try:
        @provider()
        def a(ctx):
            return {}

        assert runtime._REGISTRY["provider"] is a

        @tool
        def b(ctx):
            return 1

        assert runtime._REGISTRY["tool"] is b
    finally:
        runtime._REGISTRY.clear()
        runtime._REGISTRY.update(saved)
