"""
Native tool-use research column ("Claygent") — HTTP-layer tests.

Unlike tests/test_native_research_tooluse.py (which stubs the high-level
`llm.anthropic_tool_call`), these tests mock the LLM at the HTTP/SDK boundary:
`LLMClient._make_anthropic_client` is patched to return a fake AsyncAnthropic
client whose `messages.create(**kwargs)` records every request body and returns
scripted responses. The REAL `anthropic_tool_call` therefore runs — including
the `tools`/`tool_choice`/`output_config` request assembly and the
usage/`record_llm_usage` accounting — with zero network access.

Covers:
  * full tool_call round-trip (search → fetch → final structured answer)
  * provider-without-tool-support → automatic fallback to the legacy loop
    (both gate-level: non-Anthropic provider; and error-level: first native
    turn rejected)
  * max_steps hard stop (and the RESEARCH_MAX_STEPS_CAP hard ceiling)
  * SSRF-blocked fetch handled inside the loop (real _is_safe_public_url
    against the cloud metadata IP literal — no DNS)
  * flag off → legacy path (native client never constructed)
"""

import asyncio
import json

import pytest

import apps.api.services.leadgen.db as dbmod
import apps.api.services.scraper as scraper_mod
from apps.api.services.leadgen import llm as L
from apps.api.services.workbook import prompt_guard as G
from apps.api.services.workbook import research_column as RC


# ── Fake Anthropic response objects (shape of the SDK's Message) ──────────

class _Usage:
    def __init__(self, i=10, o=10, cw=0, cr=0):
        self.input_tokens = i
        self.output_tokens = o
        self.cache_creation_input_tokens = cw
        self.cache_read_input_tokens = cr


class _TextBlock:
    type = "text"

    def __init__(self, text):
        self.text = text


class _ToolUseBlock:
    type = "tool_use"

    def __init__(self, name, inp, id="tu_1"):
        self.name = name
        self.input = inp
        self.id = id


class _Resp:
    def __init__(self, content, stop_reason, usage=None):
        self.content = content
        self.stop_reason = stop_reason
        self.usage = usage or _Usage()


class _FakeHTTPClient:
    """Stands in for AsyncAnthropic at the HTTP boundary.

    Records every `messages.create(**kwargs)` request body (so tests can
    assert on the native `tools`/`tool_choice`/`output_config` wire shape)
    and returns/raises the next scripted item.
    """

    def __init__(self, script):
        self.script = list(script)
        self.requests = []
        self.closed = False

    def with_options(self, **_kw):
        return self

    @property
    def messages(self):
        return self

    async def create(self, **kwargs):
        self.requests.append(kwargs)
        item = self.script.pop(0) if self.script else _Resp([_TextBlock("")], "end_turn")
        if isinstance(item, BaseException):
            raise item
        return item

    async def close(self):
        self.closed = True


PROV = {
    "id": "anthropic",
    "model": "claude-opus-4-8",
    "api_key": "test-key",
    "base_url": "https://api.anthropic.com",
    "token_param": "max_tokens",
    "native": "anthropic",
}


def _synth_resp(answer, citations, usage=None):
    return _Resp(
        [_TextBlock(json.dumps({"answer": answer, "citations": citations}))],
        "end_turn",
        usage=usage,
    )


def _enable_native_http(monkeypatch, script):
    """Turn the native flag ON and wire a scripted fake client at the HTTP layer.

    Returns (fake_client, llm_usage_records) where llm_usage_records collects
    every record_llm_usage(provider, model, prompt_tok, completion_tok) call.
    """
    client = _FakeHTTPClient(script)

    # Flag ON via the settings layer; every other setting keeps its default.
    monkeypatch.setattr(
        RC, "_read_setting",
        lambda key, default="": "1" if key == "RESEARCH_NATIVE_TOOLS" else default,
    )
    monkeypatch.setattr(L.llm, "anthropic_provider", lambda: dict(PROV))
    monkeypatch.setattr(L.llm, "_make_anthropic_client", lambda prov: client)
    monkeypatch.setattr(G, "guard_enabled", lambda: True)
    monkeypatch.setattr(RC, "_get_lead_values", lambda lead, cols: {"company": "Acme"})
    monkeypatch.setattr(RC, "_resolve_prompt", lambda tpl, vals: tpl)

    async def _fake_search(query, max_results=5):
        return [{"title": "Acme SOC2", "href": "https://acme.example.com/soc2",
                 "body": "Acme is SOC 2 Type II certified."}]
    monkeypatch.setattr(RC, "_ddg_search", _fake_search)

    # llm_usage accounting recorder (LeadDB is imported lazily at call time).
    records = []

    class _RecorderDB:
        def record_llm_usage(self, provider, model, prompt_tokens, completion_tokens, **kw):
            records.append((provider, model, prompt_tokens, completion_tokens))

        def close(self):
            pass

    monkeypatch.setattr(dbmod, "LeadDB", _RecorderDB)
    return client, records


def _stub_scraper(monkeypatch, calls, text="Acme is SOC 2 Type II certified."):
    class _Scraper:
        async def scrape(self, url):
            calls.append(url)
            return {"preview_text": text, "status": 200}

    monkeypatch.setattr(scraper_mod, "UniversalScraper", lambda: _Scraper())


def _allow_urls(monkeypatch):
    """Skip real DNS for the happy-path fetch (SSRF tests use IP literals)."""
    monkeypatch.setattr(RC, "_is_safe_public_url", lambda u: (True, ""))
    monkeypatch.setattr(RC, "_revalidate_public_url", lambda u: (True, ""))


def _tool_turn_requests(client):
    """The requests that were native tool-use turns (carried the tools param)."""
    return [r for r in client.requests if "tools" in r]


# ── 1. Full round-trip: search → fetch → final structured answer ──────────

def test_tool_call_roundtrip_search_fetch_answer(monkeypatch):
    fetched_url = "https://acme.example.com/soc2"
    script = [
        _Resp([_ToolUseBlock("search", {"query": "Acme SOC 2"}, id="s1")], "tool_use"),
        _Resp([_ToolUseBlock("fetch", {"url": fetched_url}, id="f1")], "tool_use"),
        _Resp([_TextBlock("Acme is SOC 2 certified.")], "end_turn"),
        _synth_resp(
            "Yes — Acme is SOC 2 Type II certified.",
            [{"url": fetched_url, "title": "Acme SOC2", "quoted_text": "SOC 2 Type II"}],
        ),
    ]
    client, usage_records = _enable_native_http(monkeypatch, script)
    _allow_urls(monkeypatch)
    scraped = []
    _stub_scraper(monkeypatch, scraped)
    L.llm.reset_usage()

    out = asyncio.run(RC.execute_research_column(
        "Does Acme have SOC 2? Cite the source.", {}, [], max_steps=4))

    # Final answer + validated citation (URL is in the fetched set).
    assert out["success"] is True
    assert "SOC 2" in out["value"]
    md = out["metadata"]["research"]
    assert md["stopped_reason"] == "answered"
    assert md["citations"] == [{"url": fetched_url, "title": "Acme SOC2",
                                "quoted_text": "SOC 2 Type II"}]
    assert md["cost_usd"] > 0
    assert scraped == [fetched_url]

    # Native wire shape: real `tools` param on every tool turn, first turn
    # forced with tool_choice=any, subsequent turns auto.
    tool_turns = _tool_turn_requests(client)
    assert len(tool_turns) == 3
    for req in tool_turns:
        assert {t["name"] for t in req["tools"]} == {"search", "fetch"}
    assert tool_turns[0]["tool_choice"] == {"type": "any"}
    assert tool_turns[1]["tool_choice"] == {"type": "auto"}
    assert tool_turns[2]["tool_choice"] == {"type": "auto"}

    # Synthesis turn used structured output, not tools.
    synth_reqs = [r for r in client.requests if "output_config" in r]
    assert len(synth_reqs) == 1
    assert synth_reqs[0]["output_config"]["format"]["type"] == "json_schema"

    # tool_result round-trip: results were sent back as tool_result blocks,
    # wrapped in the untrusted fence, matched to the emitting tool_use ids.
    last_msgs = client.requests[-1]["messages"]
    tool_result_ids = [
        b["tool_use_id"]
        for m in last_msgs if m.get("role") == "user" and isinstance(m.get("content"), list)
        for b in m["content"] if isinstance(b, dict) and b.get("type") == "tool_result"
    ]
    assert tool_result_ids == ["s1", "f1"]
    fenced = [
        str(b.get("content", ""))
        for m in last_msgs if m.get("role") == "user" and isinstance(m.get("content"), list)
        for b in m["content"] if isinstance(b, dict) and b.get("type") == "tool_result"
    ]
    assert all("BEGIN_UNTRUSTED_" in c for c in fenced)

    # Usage recorded in llm_usage exactly as today: one row per HTTP call.
    assert len(usage_records) == 4
    assert all(r[0] == "anthropic" and r[1] == "claude-opus-4-8" for r in usage_records)
    assert L.llm.usage.calls >= 4


# ── 2. Provider without tool support → automatic legacy fallback ──────────

def test_provider_without_tools_falls_back_to_legacy(monkeypatch):
    # First (and only) native turn is rejected at the HTTP layer, the way an
    # OpenAI-compatible / non-tool-capable backend would refuse the request.
    script = [RuntimeError("HTTP 400: 'tools' is not supported for this model")]
    client, _ = _enable_native_http(monkeypatch, script)

    called = {"legacy": False}

    async def _fake_legacy(question, max_steps=4):
        called["legacy"] = True
        return {"success": True, "value": "legacy answer", "error": None}
    monkeypatch.setattr(RC, "_run_legacy", _fake_legacy)

    out = asyncio.run(RC.execute_research_column("q", {}, [], max_steps=4))
    assert called["legacy"] is True
    assert out == {"success": True, "value": "legacy answer", "error": None}
    # The failed native attempt did not limp on to synthesis calls.
    assert len(client.requests) == 1


def test_non_anthropic_provider_gates_to_legacy(monkeypatch):
    # Flag ON but the default provider has no native tool-use → gate-level
    # fallback: the native client is never even constructed.
    monkeypatch.setattr(
        RC, "_read_setting",
        lambda key, default="": "1" if key == "RESEARCH_NATIVE_TOOLS" else default,
    )
    monkeypatch.setattr(L.llm, "anthropic_provider", lambda: None)
    monkeypatch.setattr(
        L.llm, "_make_anthropic_client",
        lambda prov: pytest.fail("native client must not be constructed"),
    )
    monkeypatch.setattr(RC, "_get_lead_values", lambda lead, cols: {})
    monkeypatch.setattr(RC, "_resolve_prompt", lambda tpl, vals: tpl)

    called = {"legacy": False}

    async def _fake_legacy(question, max_steps=4):
        called["legacy"] = True
        return {"success": True, "value": "legacy answer", "error": None}
    monkeypatch.setattr(RC, "_run_legacy", _fake_legacy)

    out = asyncio.run(RC.execute_research_column("q", {}, [], max_steps=4))
    assert called["legacy"] is True
    assert out["value"] == "legacy answer"
    assert "metadata" not in out


# ── 3. max_steps hard stop ─────────────────────────────────────────────────

def test_max_steps_hard_stop(monkeypatch):
    # Model never answers — every turn asks to search again. The script holds
    # exactly max_steps tool turns, then the synthesis response; if the loop
    # over-ran the cap it would consume the synth item as a tool turn and the
    # tool-turn count below would catch it.
    script = [_Resp([_ToolUseBlock("search", {"query": "x"}, id=f"s{i}")], "tool_use")
              for i in range(3)]
    script.append(_synth_resp("best guess from notes", []))
    client, _ = _enable_native_http(monkeypatch, script)

    out = asyncio.run(RC.execute_research_column(
        "q", {}, [], max_steps=3, cell_budget_usd=100.0))

    assert out["metadata"]["research"]["stopped_reason"] == "max_steps"
    # Exactly max_steps tool turns hit the wire, then synthesis.
    assert len(_tool_turn_requests(client)) == 3


def test_max_steps_clamped_to_hard_cap(monkeypatch):
    # A column configured with an absurd max_steps is clamped to the
    # RESEARCH_MAX_STEPS_CAP hard ceiling (default 6).
    script = [_Resp([_ToolUseBlock("search", {"query": "x"}, id=f"s{i}")], "tool_use")
              for i in range(RC.MAX_STEPS_CAP)]
    script.append(_synth_resp("best guess", []))
    client, _ = _enable_native_http(monkeypatch, script)

    out = asyncio.run(RC.execute_research_column(
        "q", {}, [], max_steps=50, cell_budget_usd=100.0))

    assert out["metadata"]["research"]["stopped_reason"] == "max_steps"
    assert len(_tool_turn_requests(client)) == RC.MAX_STEPS_CAP


# ── 4. SSRF-blocked fetch handled inside the loop ──────────────────────────

def test_ssrf_blocked_fetch_handled(monkeypatch):
    meta_url = "http://169.254.169.254/latest/meta-data"
    script = [
        _Resp([_ToolUseBlock("fetch", {"url": meta_url}, id="f1")], "tool_use"),
        _Resp([_TextBlock("could not read it")], "end_turn"),
        # Model tries to cite the blocked URL anyway → must be dropped.
        _synth_resp("no data available",
                    [{"url": meta_url, "title": "meta", "quoted_text": "x"}]),
    ]
    client, _ = _enable_native_http(monkeypatch, script)
    # REAL _is_safe_public_url runs: the metadata IP literal is link-local and
    # is rejected without any DNS lookup.
    scraped = []
    _stub_scraper(monkeypatch, scraped, text="SECRET-INTERNAL")

    out = asyncio.run(RC.execute_research_column("q", {}, [], max_steps=4))

    # The scraper never touched the metadata endpoint.
    assert scraped == []
    # The model got a proper is_error tool_result and the loop continued.
    second_req_msgs = _tool_turn_requests(client)[1]["messages"]
    blocked = [
        b for m in second_req_msgs
        if m.get("role") == "user" and isinstance(m.get("content"), list)
        for b in m["content"] if isinstance(b, dict) and b.get("type") == "tool_result"
    ]
    assert len(blocked) == 1
    assert blocked[0].get("is_error") is True
    assert "blocked" in str(blocked[0]["content"])
    # A never-fetched URL cannot survive citation validation.
    assert out["metadata"]["research"]["citations"] == []


# ── 5. Flag off → legacy path ──────────────────────────────────────────────

def test_flag_off_uses_legacy(monkeypatch):
    monkeypatch.setattr(RC, "_read_setting", lambda key, default="": "0"
                        if key == "RESEARCH_NATIVE_TOOLS" else default)
    monkeypatch.setattr(
        L.llm, "_make_anthropic_client",
        lambda prov: pytest.fail("native client must not be constructed when flag is off"),
    )
    monkeypatch.setattr(RC, "_get_lead_values", lambda lead, cols: {})
    monkeypatch.setattr(RC, "_resolve_prompt", lambda tpl, vals: "Does Acme use K8s?")

    called = {"legacy": False, "question": None}

    async def _fake_legacy(question, max_steps=4):
        called["legacy"] = True
        called["question"] = question
        return {"success": True, "value": "legacy answer", "error": None}
    monkeypatch.setattr(RC, "_run_legacy", _fake_legacy)

    out = asyncio.run(RC.execute_research_column("q", {}, [], max_steps=4))
    assert called["legacy"] is True
    assert called["question"] == "Does Acme use K8s?"
    assert out["value"] == "legacy answer"
    assert "metadata" not in out  # legacy path output shape unchanged
