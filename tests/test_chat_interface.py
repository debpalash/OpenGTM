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


def _main_store():
    """Resolve the self-host `main` workspace + its tenant-scoped store.

    Mirrors what _resolve_chat_workspace returns on self-host so direct
    _execute_tool calls in tests use the same (store, workspace_id, slug) shape
    as the live chat path.
    """
    from apps.api.services.workspace import manager as ws
    from apps.api.services.leadgen.store import get_lead_store
    ws_id = ws._get_active_workspace_id() or "main"
    slug = ws.workspace_slug(ws_id) or "main"
    return get_lead_store(ws_id, slug), ws_id, slug


# ── Offline: tool registry ────────────────────────────────────────────────

def test_tool_registry_exposes_expected_tools():
    tools = ck._build_tools()
    names = {t["function"]["name"] for t in tools}
    assert len(tools) == 22, f"expected 22 tools, got {len(tools)}"
    # The dead create_workbook / add_workbook_column tools were removed.
    assert "create_workbook" not in names and "add_workbook_column" not in names
    assert ck.SAFE_TOOLS <= names
    assert set(ck.DANGEROUS_TOOLS) <= names
    assert {"draft_plan", "execute_plan"} <= names
    assert "find_people_at_company" in names
    assert {"verify_people_at_company", "create_people_workbook"} <= names
    people_workbook = next(
        tool for tool in tools
        if tool["function"]["name"] == "create_people_workbook"
    )
    people_workbook_properties = people_workbook["function"]["parameters"]["properties"]
    assert {"conversation_id", "person_ids", "idempotency_key"} <= set(
        people_workbook_properties
    )
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


def test_unknown_tools_fail_closed_to_confirmation():
    assert ck._needs_confirmation("hallucinated_write_tool") is True


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
    store, ws_id, slug = _main_store()
    res = json.loads(asyncio.run(
        ck._execute_tool(tool, args, store=store, workspace_id=ws_id, slug=slug)
    ))
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

    async def fake_exec(name, args, **kwargs):
        calls.append((name, args))
        return json.dumps({"ok": True, "lead_id": args.get("lead_id")})

    monkeypatch.setattr(ck, "_execute_tool", fake_exec)
    msgs = [{"role": "user", "content": "set lead 5 qualified"}]
    proposed = {"id": "tc1", "type": "function",
                "function": {"name": "update_lead_status",
                             "arguments": json.dumps({"lead_id": 5, "status": "qualified"})}}
    approval_id = ck.chat_history.create_tool_approval("main", None, proposed)
    approved = [{
        "tool_call": {**proposed, "id": approval_id},
        "decision": "approve",
    }]
    objs = _events_to_objs(_collect(ck._resolve_approved_calls(
        msgs, approved, store=object(), workspace_id="main", slug="main")))

    # Tool executed exactly once, events emitted, history is OpenAI-valid.
    assert calls == [("update_lead_status", {"lead_id": 5, "status": "qualified"})]
    assert any("tool_call" in o for o in objs)
    assert any("tool_result" in o for o in objs)
    assert msgs[1]["role"] == "assistant" and msgs[1]["tool_calls"]
    assert msgs[2]["role"] == "tool" and msgs[2]["tool_call_id"] == approval_id


def test_resolve_approved_calls_skips_on_deny(monkeypatch):
    calls = []

    async def fake_exec(name, args, **kwargs):
        calls.append(name)
        return json.dumps({"ok": True})

    monkeypatch.setattr(ck, "_execute_tool", fake_exec)
    msgs = [{"role": "user", "content": "delete everything"}]
    proposed = {"id": "tc9", "type": "function",
                "function": {"name": "start_collection",
                             "arguments": json.dumps({"query": "x"})}}
    approval_id = ck.chat_history.create_tool_approval("main", None, proposed)
    approved = [{
        "tool_call": {**proposed, "id": approval_id},
        "decision": "deny",
    }]
    objs = _events_to_objs(_collect(ck._resolve_approved_calls(
        msgs, approved, store=object(), workspace_id="main", slug="main")))

    assert calls == [], "denied tool must NOT execute"
    assert any("tool_denied" in o for o in objs)
    assert json.loads(msgs[-1]["content"]).get("denied") is True


def test_approval_executes_server_stored_arguments_not_client_echo(monkeypatch):
    calls = []

    async def fake_exec(name, args, **kwargs):
        calls.append((name, args))
        return json.dumps({"ok": True})

    monkeypatch.setattr(ck, "_execute_tool", fake_exec)
    proposed = {
        "id": "original",
        "type": "function",
        "function": {
            "name": "update_lead_status",
            "arguments": json.dumps({"lead_id": 5, "status": "qualified"}),
        },
    }
    approval_id = ck.chat_history.create_tool_approval("main", None, proposed)
    tampered = {
        "id": approval_id,
        "type": "function",
        "function": {
            "name": "start_collection",
            "arguments": json.dumps({"query": "attacker-controlled"}),
        },
    }

    _collect(ck._resolve_approved_calls(
        [], [{"tool_call": tampered, "decision": "approve"}],
        store=object(), workspace_id="main", slug="main",
    ))

    assert calls == [
        ("update_lead_status", {"lead_id": 5, "status": "qualified"})
    ]


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

    async def fake_exec(name, args, **kwargs):
        exec_count["n"] += 1
        return json.dumps({"total": 1})

    monkeypatch.setattr(ck, "_execute_tool", fake_exec)
    monkeypatch.setattr(ck.httpx, "AsyncClient", lambda *a, **k: _FakeClient(rec, *a, **k))

    provider = {"id": "fake", "name": "Fake", "api_key": "k",
                "base_url": "http://fake/v1", "model": "m"}
    max_rounds = 3
    lines = _collect(ck._stream_chat(
        [{"role": "user", "content": "keep going forever"}],
        ck._build_tools(), provider, [], round_idx=0, max_rounds=max_rounds, seen_calls={},
        store=object(), workspace_id="main", slug="main"))

    # The model "always wants tools", so the loop must cap executions at
    # max_rounds and then re-issue with NO tools (final body has tools == []).
    assert exec_count["n"] == max_rounds, f"expected {max_rounds} executions, got {exec_count['n']}"
    assert rec["bodies"][-1]["tools"] == [], "final round must be issued with no tools"
    assert any("Final answer." in ln for ln in lines)


# ── Offline: autopilot (goal → plan → execute) ────────────────────────────

def test_autopilot_drafts_plan_from_compound_goal():
    from apps.api.services.agent import autopilot
    plan = asyncio.run(autopilot.draft_plan("build a list of 50 IT staffing firms in Pune and find founders' emails", 50))
    kinds = [s["kind"] for s in plan["steps"]]
    assert kinds[0] == "create_source_workbook"
    assert "add_agent_column" in kinds          # detected the "founders' emails" ask
    assert kinds[-1] == "report"
    assert plan["estimated_rows"] == 50


def test_autopilot_sets_auto_enrich_when_fields_requested():
    from apps.api.services.agent import autopilot
    # Goal with an enrichment field → chain enrichment after sourcing.
    p1 = asyncio.run(autopilot.draft_plan("50 IT staffing firms in Pune, founders' emails", 50))
    create = next(s for s in p1["steps"] if s["kind"] == "create_source_workbook")
    assert create["params"]["auto_enrich"] is True
    # Goal with no enrichment field → just source, no auto-enrich, no agent column.
    p2 = asyncio.run(autopilot.draft_plan("list of IT staffing firms in Pune", 50))
    create2 = next(s for s in p2["steps"] if s["kind"] == "create_source_workbook")
    assert create2["params"]["auto_enrich"] is False
    assert not any(s["kind"] == "add_agent_column" for s in p2["steps"])


def test_autopilot_dedupes_email_field():
    from apps.api.services.agent import autopilot
    # "founders' emails" should yield ONE email column (Founder Email), not two.
    plan = asyncio.run(autopilot.draft_plan("find founder emails and contact emails", 0))
    email_cols = [s for s in plan["steps"]
                  if s["kind"] == "add_agent_column" and s["params"]["target_field"] == "email"]
    assert len(email_cols) == 1


def test_autopilot_execute_orchestrates_existing_tools():
    from apps.api.services.agent import autopilot
    calls = []

    async def fake_tool(name, params):
        calls.append((name, params))
        if name == "create_source_workbook":
            return json.dumps({"workbook_id": "wb_123"})
        return json.dumps({"ok": True})

    plan = asyncio.run(autopilot.draft_plan("50 IT staffing firms in Pune, founders' emails", 50))
    result = asyncio.run(autopilot.execute_plan(plan, fake_tool))

    assert result["workbook_id"] == "wb_123"
    tool_names = [c[0] for c in calls]
    assert tool_names[0] == "create_source_workbook"
    assert "add_agent_column" in tool_names
    # The agent column was attached to the workbook created in step 1.
    agent_call = next(c for c in calls if c[0] == "add_agent_column")
    assert agent_call[1]["workbook_id"] == "wb_123"


def test_execute_plan_is_gated_draft_plan_is_not():
    assert ck._needs_confirmation("execute_plan")
    assert not ck._needs_confirmation("draft_plan")


# ── Offline: cross-conversation memory (dependency-free builtin backend) ──

def test_builtin_memory_recall(tmp_path):
    from apps.api.services import memory as mem
    store = mem._BuiltinMemory(str(tmp_path / "m.db"))
    store.add("User asked about IT staffing firms in Pune; found 12 leads", user_id="u1")
    store.add("User prefers concise answers", user_id="u1")
    hits = store.search("staffing companies in pune", user_id="u1", limit=3)
    assert any("Pune" in h["memory"] for h in hits)
    # No keyword overlap → no recall (avoids injecting irrelevant memories).
    assert store.search("weather forecast tomorrow", user_id="u1") == []
    # Per-user isolation.
    assert store.search("staffing", user_id="someone_else") == []


def test_memory_is_available_without_openmemory():
    from apps.api.services import memory as mem
    assert mem.is_available() is True  # builtin backend always available


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

    async def safe_execute(name, args, **kwargs):
        if name in ck.DANGEROUS_TOOLS:
            attempted.append(name)
            return json.dumps({"stubbed": True, "tool": name})
        return await real_execute(name, args, **kwargs)

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
