"""
Capability tests for the chat interface (/api/copilotkit).

Offline tests (always run, no network): tool registry, confirmation gating,
read-only tool execution against the local lead DB, provider-chain shape.

Live test (opt-in via RUN_LIVE_LLM=1): end-to-end SSE streaming against the
configured LLM provider chain — verifies the model streams content and invokes
backend tools. Skipped by default so the suite stays fast, offline, and
CI-safe.

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

# conftest.py sets DATABASE_URL=sqlite before collection; belt-and-suspenders here.
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
    # Every declared safe + dangerous tool is actually exposed to the model.
    assert ck.SAFE_TOOLS <= names
    assert set(ck.DANGEROUS_TOOLS) <= names
    # Each tool definition is a well-formed OpenAI function schema.
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
    assert "Launch Lead Collection" in desc   # human label
    assert "fintech mumbai" in desc            # echoes the query detail


# ── Offline: read-only tool execution (local DB, no network) ──────────────

@pytest.mark.parametrize("tool,args,expect_key", [
    ("get_lead_stats", {}, "total"),
    ("search_leads", {"query": "", "limit": 3}, "leads"),
])
def test_readonly_tool_execution(tool, args, expect_key):
    res = json.loads(asyncio.run(ck._execute_tool(tool, args)))
    assert isinstance(res, dict)
    assert expect_key in res, f"{tool} result missing {expect_key!r}: {list(res)[:6]}"


# ── Offline: provider chain shape ─────────────────────────────────────────

def test_provider_chain_shape():
    chain = ck._get_provider_chain()
    assert isinstance(chain, list)
    for p in chain:
        assert {"id", "model", "api_key", "base_url"} <= set(p)


# ── Live: end-to-end SSE streaming (opt-in) ───────────────────────────────

async def _run_chat(client, prompt, timeout=60):
    """POST one user message, consume the SSE stream, classify events."""
    ev = {"content": [], "tool_call": [], "conversation_id": None,
          "error": [], "done": False}
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
            if "error" in d:
                ev["error"].append(d["error"])
    return ev


@pytest.mark.skipif(not RUN_LIVE, reason="set RUN_LIVE_LLM=1 (and have network) to run")
def test_live_streaming_and_tool_use():
    import httpx
    from fastapi import FastAPI

    # Safety net: stub mutating tools so the model can't trigger real work.
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

        # Streaming produced output and a conversation was persisted.
        assert not plain["error"], plain["error"]
        assert plain["conversation_id"]
        assert "".join(plain["content"]).strip(), "no content streamed for plain prompt"

        # The model reached for a backend tool when asked for stats.
        assert not stats["error"], stats["error"]
        assert "get_lead_stats" in stats["tool_call"], stats["tool_call"]

        # No mutating tool was executed for real.
        assert attempted == [], f"mutating tools attempted: {attempted}"
    finally:
        ck._execute_tool = real_execute
