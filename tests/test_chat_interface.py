"""
Capability + safety tests for the chat interface (/api/copilotkit).

Offline tests (always run, no network): tool registry, confirmation gating,
read-only tool execution against the local lead DB, provider-chain shape, the
bounded ReAct loop (via a fake provider), and the human-in-the-loop approval
resolver.

Live test (opt-in via RUN_LIVE_LLM=1): end-to-end SSE streaming against the
configured LLM provider chain. Skipped by default so the suite stays fast,
offline, and CI-safe.

Safety: mutating tools (start_collection, enrich_lead, ...) are monkeypatched
to a stub, so even the live test can never spawn real scraping/enrichment jobs.

Run offline:  uv run pytest tests/test_chat_interface.py
Run live too: RUN_LIVE_LLM=1 uv run pytest tests/test_chat_interface.py -s
"""
import os
import sys
import json
import asyncio

import pytest

os.environ.setdefault("DATABASE_URL", "sqlite:///./data/_pytest.db")

PROJECT_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if PROJECT_ROOT not in sys.path:
    sys.path.insert(0, PROJECT_ROOT)

from apps.api.routers import copilotkit as ck  # noqa: E402

RUN_LIVE = os.environ.get("RUN_LIVE_LLM") == "1"


# ── Offline: tool registry ────────────────────────────────────────────────

def test_tool_registry_exposes_expected_tools():
    tools = ck._build_tools()
    names = {t["function"]["name"] for t in tools}
    assert len(tools) == 18, f"expected 18 tools, got {len(tools)}"
    assert ck.SAFE_TOOLS <= names
    assert set(ck.DANGEROUS_TOOLS) <= names
    for t in tools:
        assert t["type"] == "function"
        fn = t["function"]
        assert fn["name"] and fn.get("description")
        assert fn.get("parameters", {}).get("type") == "object"


# ── Offline: confirmation gating ──────────────────────────────────────────

def test_dangerous_tools_require_confirmation():
    assert all(ck._needs_confirmation(n) for n in ck.DANGEROUS_TOOLS)


def test_safe_tools_bypass_confirmation():
    assert all(not ck._needs_confirmation(n) for n in ck.SAFE_TOOLS)


def test_safe_and_dangerous_sets_are_disjoint():
    assert ck.SAFE_TOOLS.isdisjoint(set(ck.DANGEROUS_TOOLS))


def test_describe_action_renders_label_and_detail():
    desc = ck._describe_action("start_collection", {"query": "fintech mumbai"})
    assert "Launch Lead Collection" in desc
    assert "fintech mumbai" in desc


# ── Offline: read-only tool execution (local DB, no network) ──────────────

@pytest.mark.parametrize("tool,args,expect_key", [
    ("get_lead_stats", {}, "total"),
    ("search_leads", {"query": "", "limit": 3}, "leads"),
])
def test_readonly_tool_execution(tool, args, expect_key):
    res = json.loads(asyncio.run(ck._execute_tool(tool, args)))
    assert isinstance(res, dict)
    assert expect_key in res, f"{tool} result missing {expect_key!r}: {list(res)[:6]}"


def test_provider_chain_shape():
    chain = ck._get_provider_chain()
    assert isinstance(chain, list)
    for p in chain:
        assert {"id", "model", "api_key", "base_url"} <= set(p)


# ── Offline: human-in-the-loop approval resolver ──────────────────────────

def _collect(agen):
    """Drain an async generator into a list (sync helper for tests)."""
    async def _run():
        out = []
        async for x in agen:
            out.append(x)
        return out
    return asyncio.run(_run())


def _events_to_objs(lines):
    objs = []
    for ln in lines:
        if ln.startswith("data: ") and ln.strip() != "data: [DONE]":
            try:
                objs.append(json.loads(ln[6:]))
            except json.JSONDecodeError:
                pass
    return objs


def test_resolve_approved_calls_executes_on_approve(monkeypatch):
    calls = []

    async def fake_exec(name, args):
        calls.append((name, args))
        return json.dumps({"ok": True, "lead_id": args.get("lead_id")})

    monkeypatch.setattr(ck, "_execute_tool", fake_exec)
    msgs = [{"role": "user", "content": "set lead 5 qualified"}]
    approved = [{
        "tool_call": {"id": "tc1", "type": "function",
                      "function": {"name": "update_lead_status",
                                   "arguments": json.dumps({"lead_id": 5, "status": "qualified"})}},
        "decision": "approve",
    }]
    objs = _events_to_objs(_collect(ck._resolve_approved_calls(msgs, approved)))

    # Tool executed exactly once, events emitted, history is OpenAI-valid.
    assert calls == [("update_lead_status", {"lead_id": 5, "status": "qualified"})]
    assert any("tool_call" in o for o in objs)
    assert any("tool_result" in o for o in objs)
    assert msgs[1]["role"] == "assistant" and msgs[1]["tool_calls"]
    assert msgs[2]["role"] == "tool" and msgs[2]["tool_call_id"] == "tc1"


def test_resolve_approved_calls_skips_on_deny(monkeypatch):
    calls = []

    async def fake_exec(name, args):
        calls.append(name)
        return json.dumps({"ok": True})

    monkeypatch.setattr(ck, "_execute_tool", fake_exec)
    msgs = [{"role": "user", "content": "delete everything"}]
    approved = [{
        "tool_call": {"id": "tc9", "type": "function",
                      "function": {"name": "start_collection",
                                   "arguments": json.dumps({"query": "x"})}},
        "decision": "deny",
    }]
    objs = _events_to_objs(_collect(ck._resolve_approved_calls(msgs, approved)))

    assert calls == [], "denied tool must NOT execute"
    assert any("tool_denied" in o for o in objs)
    assert json.loads(msgs[-1]["content"]).get("denied") is True


# ── Offline: bounded ReAct loop (fake provider, no network) ───────────────

class _FakeResp:
    def __init__(self, lines):
        self.status_code = 200
        self._lines = lines

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False

    async def aiter_lines(self):
        for ln in self._lines:
            yield ln

    async def aread(self):
        return b""


class _FakeClient:
    """Stand-in for httpx.AsyncClient that always proposes a tool call while
    tools are offered, and returns content once tools are empty. Records each
    request body so the test can assert the final round was tools-less."""
    def __init__(self, recorder, *a, **k):
        self._rec = recorder

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False

    def stream(self, method, url, json=None, headers=None):
        self._rec["bodies"].append(json)
        if json.get("tools"):
            # Propose one tool call with DISTINCT args each round (so the
            # per-turn dedup doesn't suppress it — this isolates the round cap).
            n = len(self._rec["bodies"])
            lines = [
                'data: ' + _dumps({"choices": [{"delta": {"tool_calls": [
                    {"index": 0, "id": f"c{n}", "function": {"name": "search_leads",
                     "arguments": _dumps({"query": f"q{n}"})}}]}}]}),
                'data: ' + _dumps({"choices": [{"delta": {}, "finish_reason": "tool_calls"}]}),
                "data: [DONE]",
            ]
        else:
            lines = [
                'data: ' + _dumps({"choices": [{"delta": {"content": "Final answer."}}]}),
                "data: [DONE]",
            ]
        return _FakeResp(lines)


def _dumps(o):
    return json.dumps(o)


def test_bounded_tool_loop_caps_rounds(monkeypatch):
    rec = {"bodies": []}
    exec_count = {"n": 0}

    async def fake_exec(name, args):
        exec_count["n"] += 1
        return json.dumps({"total": 1})

    monkeypatch.setattr(ck, "_execute_tool", fake_exec)
    monkeypatch.setattr(ck.httpx, "AsyncClient", lambda *a, **k: _FakeClient(rec, *a, **k))

    provider = {"id": "fake", "name": "Fake", "api_key": "k",
                "base_url": "http://fake/v1", "model": "m"}
    max_rounds = 3
    lines = _collect(ck._stream_chat(
        [{"role": "user", "content": "keep going forever"}],
        ck._build_tools(), provider, [], round_idx=0, max_rounds=max_rounds, seen_calls={}))

    # The model "always wants tools", so the loop must cap executions at
    # max_rounds and then re-issue with NO tools (final body has tools == []).
    assert exec_count["n"] == max_rounds, f"expected {max_rounds} executions, got {exec_count['n']}"
    assert rec["bodies"][-1]["tools"] == [], "final round must be issued with no tools"
    assert any("Final answer." in ln for ln in lines)


# ── Live: end-to-end SSE streaming (opt-in) ───────────────────────────────

async def _run_chat(client, prompt, timeout=60):
    ev = {"content": [], "tool_call": [], "conversation_id": None,
          "confirmation": [], "error": [], "done": False}
    body = {"messages": [{"role": "user", "content": prompt}]}
    async with client.stream("POST", "/api/copilotkit", json=body, timeout=timeout) as r:
        assert r.status_code == 200
        async for line in r.aiter_lines():
            if not line.startswith("data: "):
                continue
            data = line[6:]
            if data == "[DONE]":
                ev["done"] = True
                break
            try:
                d = json.loads(data)
            except json.JSONDecodeError:
                continue
            if "conversation_id" in d:
                ev["conversation_id"] = d["conversation_id"]
            if "content" in d:
                ev["content"].append(d["content"])
            if "tool_call" in d:
                ev["tool_call"].append(d["tool_call"]["name"])
            if "confirmation_required" in d:
                ev["confirmation"].append(d["confirmation_required"]["name"])
            if "error" in d:
                ev["error"].append(d["error"])
    return ev


@pytest.mark.skipif(not RUN_LIVE, reason="set RUN_LIVE_LLM=1 (and have network) to run")
def test_live_streaming_and_tool_use():
    import httpx
    from fastapi import FastAPI

    real_execute = ck._execute_tool
    attempted = []

    async def safe_execute(name, args):
        if name in ck.DANGEROUS_TOOLS:
            attempted.append(name)
            return json.dumps({"stubbed": True, "tool": name})
        return await real_execute(name, args)

    ck._execute_tool = safe_execute
    try:
        app = FastAPI()
        app.include_router(ck.router)

        async def _go():
            transport = httpx.ASGITransport(app=app)
            async with httpx.AsyncClient(transport=transport, base_url="http://test") as c:
                plain = await _run_chat(c, "In one sentence, what can you help me do?")
                stats = await _run_chat(c, "How many leads are in the pipeline? Use the stats tool.")
                return plain, stats

        plain, stats = asyncio.run(_go())

        assert not plain["error"], plain["error"]
        assert plain["conversation_id"]
        assert "".join(plain["content"]).strip()

        assert not stats["error"], stats["error"]
        assert "get_lead_stats" in stats["tool_call"], stats["tool_call"]
        assert attempted == [], f"mutating tools attempted: {attempted}"
    finally:
        ck._execute_tool = real_execute
