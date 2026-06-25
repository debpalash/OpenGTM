"""
Anthropic (Claude) provider — registration, native call path, and fallback.

These tests never hit the network: the `anthropic` SDK's AsyncAnthropic client is
monkeypatched with a fake that records the request and returns a canned Message.
They assert that:
  - Anthropic is registered in the provider registry as a native (non
    OpenAI-compatible) provider.
  - The native call path splits system/turns correctly and returns text.
  - Default-provider selection prefers Claude only in cloud + key-present, and
    otherwise falls back to the free OSS chain (Claude never displaces it as a
    fallback).
  - When no Anthropic key is configured, Anthropic is absent from the resolved
    provider list and the free providers are used.
"""
import asyncio
import sys
import types

import pytest

from apps.api.services.leadgen import llm as L


# ── Fakes for the anthropic SDK (no network) ─────────────────────────

class _FakeUsage:
    def __init__(self, i, o):
        self.input_tokens = i
        self.output_tokens = o


class _FakeTextBlock:
    type = "text"

    def __init__(self, text):
        self.text = text


class _FakeThinkingBlock:
    type = "thinking"

    def __init__(self, text):
        self.thinking = text


class _FakeMessage:
    def __init__(self, text):
        # include a thinking block to prove it's skipped
        self.content = [_FakeThinkingBlock("...reasoning..."), _FakeTextBlock(text)]
        self.usage = _FakeUsage(11, 7)
        self.model = "claude-opus-4-8"


class _FakeMessages:
    def __init__(self, recorder):
        self._rec = recorder

    async def create(self, **kwargs):
        self._rec["create_kwargs"] = kwargs
        return _FakeMessage("Hello from Claude")


class _FakeAsyncAnthropic:
    """Drop-in for anthropic.AsyncAnthropic. Records construction + request."""

    last_instance = None

    def __init__(self, **kwargs):
        self.init_kwargs = kwargs
        self._rec = {}
        self.messages = _FakeMessages(self._rec)
        _FakeAsyncAnthropic.last_instance = self

    def with_options(self, **kwargs):
        self._rec["with_options"] = kwargs
        return self

    async def close(self):
        self._rec["closed"] = True


@pytest.fixture
def fake_anthropic(monkeypatch):
    """Install a fake `anthropic` module exposing AsyncAnthropic."""
    mod = types.ModuleType("anthropic")
    mod.AsyncAnthropic = _FakeAsyncAnthropic
    monkeypatch.setitem(sys.modules, "anthropic", mod)
    # Avoid DB writes during usage tracking.
    monkeypatch.setattr(L, "LeadDB", None, raising=False)
    return mod


@pytest.fixture(autouse=True)
def clean_env(monkeypatch):
    for k in ("ANTHROPIC_API_KEY", "ANTHROPIC_MODEL", "ANTHROPIC_BASE_URL",
              "LLM_DEFAULT_PROVIDER", "YUPCHA_CLOUD"):
        monkeypatch.delenv(k, raising=False)
    # Force _read_setting to use env only (no settings DB).
    monkeypatch.setattr(L, "_read_setting",
                        lambda key, default="": __import__("os").environ.get(key, default))
    yield


# ── Registration ─────────────────────────────────────────────────────

def test_anthropic_registered_as_native_provider():
    assert "anthropic" in L.PROVIDER_CONFIG
    cfg = L.PROVIDER_CONFIG["anthropic"]
    assert cfg["env_key"] == "ANTHROPIC_API_KEY"
    assert cfg["default_model"] == "claude-opus-4-8"
    assert cfg["native"] == "anthropic"
    # Present in the fallback order, and last so it never displaces the free tier.
    assert "anthropic" in L.PROVIDER_ORDER
    assert L.PROVIDER_ORDER[-1] == "anthropic"


def test_provider_config_resolves_when_key_present(monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-xyz")
    c = L.LLMClient()
    cfg = c._get_provider_config("anthropic")
    assert cfg is not None
    assert cfg["native"] == "anthropic"
    assert cfg["model"] == "claude-opus-4-8"
    assert cfg["api_key"] == "sk-ant-xyz"


def test_provider_config_none_without_key():
    c = L.LLMClient()
    assert c._get_provider_config("anthropic") is None


# ── Default selection & fallback ─────────────────────────────────────

def test_explicit_default_wins(monkeypatch):
    monkeypatch.setenv("LLM_DEFAULT_PROVIDER", "groq")
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-xyz")
    monkeypatch.setenv("YUPCHA_CLOUD", "1")
    assert L.LLMClient()._resolve_default_provider() == "groq"


def test_cloud_with_key_defaults_to_claude(monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-xyz")
    monkeypatch.setenv("YUPCHA_CLOUD", "1")
    assert L.LLMClient()._resolve_default_provider() == "anthropic"


def test_self_host_with_key_stays_on_free_default(monkeypatch):
    # Key present but no cloud flag -> OSS base keeps the free default.
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-xyz")
    assert L.LLMClient()._resolve_default_provider() == "cerebras"


def test_cloud_without_key_falls_back_to_free(monkeypatch):
    monkeypatch.setenv("YUPCHA_CLOUD", "1")
    assert L.LLMClient()._resolve_default_provider() == "cerebras"


def test_provider_list_excludes_anthropic_without_key(monkeypatch):
    # Configure a free provider so the list is non-empty; Anthropic must be absent.
    monkeypatch.setenv("GROQ_API_KEY", "gsk_test")
    c = L.LLMClient()
    ids = [p["id"] for p in c._get_providers()]
    assert "anthropic" not in ids
    assert "groq" in ids


def test_provider_list_leads_with_claude_in_cloud(monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-xyz")
    monkeypatch.setenv("GROQ_API_KEY", "gsk_test")
    monkeypatch.setenv("YUPCHA_CLOUD", "1")
    c = L.LLMClient()
    ids = [p["id"] for p in c._get_providers()]
    assert ids[0] == "anthropic"          # Claude leads as the default
    assert "groq" in ids                  # free chain still present behind it


# ── Native call path (mocked SDK) ────────────────────────────────────

def test_native_anthropic_call_returns_text(fake_anthropic):
    c = L.LLMClient()
    prov = {
        "id": "anthropic", "api_key": "sk-ant-xyz",
        "base_url": "https://api.anthropic.com", "model": "claude-opus-4-8",
        "token_param": "max_tokens", "native": "anthropic",
    }
    messages = [
        {"role": "system", "content": "You are terse."},
        {"role": "user", "content": "hi"},
    ]
    out = asyncio.run(c._call_anthropic(prov, messages, max_tokens=64))
    assert out == "Hello from Claude"  # thinking block skipped, text returned

    kw = _FakeAsyncAnthropic.last_instance._rec["create_kwargs"]
    assert kw["model"] == "claude-opus-4-8"
    assert kw["max_tokens"] == 64
    assert kw["thinking"] == {"type": "adaptive"}   # adaptive thinking on Opus 4.8
    # system is split out of messages AND carries a prompt-cache breakpoint so the
    # static prefix is reused across the many per-row AI-column calls.
    assert kw["system"] == [{
        "type": "text", "text": "You are terse.",
        "cache_control": {"type": "ephemeral"},
    }]
    assert kw["messages"] == [{"role": "user", "content": "hi"}]
    # token usage tracked
    assert c.usage.total_tokens == 18
    assert c.usage.provider == "anthropic"


def test_complete_routes_to_native_path(fake_anthropic, monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-xyz")
    monkeypatch.setenv("YUPCHA_CLOUD", "1")
    c = L.LLMClient()
    out = asyncio.run(c.complete("hello", system="sys", max_tokens=32))
    assert out == "Hello from Claude"
    # the default-selected provider was anthropic, dispatched natively
    assert _FakeAsyncAnthropic.last_instance is not None


# ── Prompt caching (cost lever A) ────────────────────────────────────

def _prov():
    return {
        "id": "anthropic", "api_key": "sk-ant-xyz",
        "base_url": "https://api.anthropic.com", "model": "claude-opus-4-8",
        "token_param": "max_tokens", "native": "anthropic",
    }


def test_cache_control_on_system_prefix(fake_anthropic):
    c = L.LLMClient()
    asyncio.run(c._call_anthropic(
        _prov(),
        [{"role": "system", "content": "STATIC PREFIX"}, {"role": "user", "content": "row data"}],
        max_tokens=64,
    ))
    sys_blocks = _FakeAsyncAnthropic.last_instance._rec["create_kwargs"]["system"]
    assert sys_blocks == [{
        "type": "text", "text": "STATIC PREFIX",
        "cache_control": {"type": "ephemeral"},
    }]


def test_cache_control_disabled_via_env(fake_anthropic, monkeypatch):
    monkeypatch.setenv("LLM_PROMPT_CACHE", "0")
    c = L.LLMClient()
    asyncio.run(c._call_anthropic(
        _prov(),
        [{"role": "system", "content": "STATIC"}, {"role": "user", "content": "x"}],
        max_tokens=8,
    ))
    sys_blocks = _FakeAsyncAnthropic.last_instance._rec["create_kwargs"]["system"]
    # still a block (so the prefix is well-formed) but no cache_control breakpoint
    assert sys_blocks == [{"type": "text", "text": "STATIC"}]


def test_no_system_means_no_system_kwarg(fake_anthropic):
    c = L.LLMClient()
    asyncio.run(c._call_anthropic(_prov(), [{"role": "user", "content": "x"}], max_tokens=8))
    assert "system" not in _FakeAsyncAnthropic.last_instance._rec["create_kwargs"]


def test_cache_tokens_tracked(fake_anthropic, monkeypatch):
    # usage object reports cache read/write; client must accumulate both.
    class _CacheUsage:
        input_tokens = 5
        output_tokens = 3
        cache_creation_input_tokens = 100
        cache_read_input_tokens = 900

    class _Msg:
        content = [_FakeTextBlock("ok")]
        usage = _CacheUsage()
        model = "claude-opus-4-8"

    class _Messages:
        def __init__(self, rec):
            self._rec = rec

        async def create(self, **kwargs):
            return _Msg()

    class _Client(_FakeAsyncAnthropic):
        def __init__(self, **kwargs):
            super().__init__(**kwargs)
            self.messages = _Messages(self._rec)

    monkeypatch.setattr(fake_anthropic, "AsyncAnthropic", _Client)
    c = L.LLMClient()
    asyncio.run(c._call_anthropic(_prov(), [{"role": "user", "content": "x"}], max_tokens=8))
    assert c.usage.cache_write_tokens == 100
    assert c.usage.cache_read_tokens == 900
    assert c.usage.to_dict()["cache_read_tokens"] == 900


# ── Message Batches (cost lever B) ───────────────────────────────────

class _BatchResult:
    def __init__(self, custom_id, text=None, kind="succeeded"):
        self.custom_id = custom_id
        self.type = kind
        if text is not None:
            msg = types.SimpleNamespace(
                content=[_FakeTextBlock(text)],
                usage=_FakeUsage(2, 1),
            )
            self.result = types.SimpleNamespace(type=kind, message=msg)
        else:
            self.result = types.SimpleNamespace(type=kind)


class _Batches:
    """Records submitted requests; returns canned out-of-order results."""

    submitted = None

    def __init__(self, result_items):
        self._items = result_items

    async def create(self, requests):
        _Batches.submitted = requests
        return types.SimpleNamespace(id="batch_abc", processing_status="ended")

    async def retrieve(self, batch_id):
        return types.SimpleNamespace(id=batch_id, processing_status="ended")

    async def cancel(self, batch_id):
        return types.SimpleNamespace(processing_status="canceling")

    async def results(self, batch_id):
        async def _gen():
            for it in self._items:
                yield it
        return _gen()


@pytest.fixture
def fake_anthropic_batch(monkeypatch):
    """anthropic module with AsyncAnthropic.messages.batches + batch param types."""
    result_items = [
        # deliberately out of order vs. request order, plus one error result
        _BatchResult("2::col", "answer two"),
        _BatchResult("1::col", "answer one"),
        _BatchResult("3::col", kind="errored"),
    ]

    class _BatchClient(_FakeAsyncAnthropic):
        def __init__(self, **kwargs):
            super().__init__(**kwargs)
            self.messages = types.SimpleNamespace(batches=_Batches(result_items))

    mod = types.ModuleType("anthropic")
    mod.AsyncAnthropic = _BatchClient

    # Provide the batch param type modules the SDK call path imports.
    types_mod = types.ModuleType("anthropic.types")
    mcp_mod = types.ModuleType("anthropic.types.message_create_params")
    mcp_mod.MessageCreateParamsNonStreaming = lambda **kw: dict(kw)
    msgs_mod = types.ModuleType("anthropic.types.messages")
    bcp_mod = types.ModuleType("anthropic.types.messages.batch_create_params")
    bcp_mod.Request = lambda custom_id, params: {"custom_id": custom_id, "params": params}

    monkeypatch.setitem(sys.modules, "anthropic", mod)
    monkeypatch.setitem(sys.modules, "anthropic.types", types_mod)
    monkeypatch.setitem(sys.modules, "anthropic.types.message_create_params", mcp_mod)
    monkeypatch.setitem(sys.modules, "anthropic.types.messages", msgs_mod)
    monkeypatch.setitem(sys.modules, "anthropic.types.messages.batch_create_params", bcp_mod)
    monkeypatch.setattr(L, "LeadDB", None, raising=False)
    _Batches.submitted = None
    return mod


def test_batch_maps_results_by_custom_id(fake_anthropic_batch):
    c = L.LLMClient()
    reqs = [
        {"custom_id": "1::col", "prompt": "p1", "system": "SYS", "max_tokens": 100},
        {"custom_id": "2::col", "prompt": "p2", "system": "SYS", "max_tokens": 100},
        {"custom_id": "3::col", "prompt": "p3", "system": "SYS", "max_tokens": 100},
    ]
    out = asyncio.run(c.batch_complete_anthropic(reqs, prov=_prov(), poll_interval=0))
    # keyed strictly by custom_id (results came back out of order); errored absent
    assert out == {"1::col": "answer one", "2::col": "answer two"}
    # the shared system prefix is cache-breakpointed inside each batched request
    sub = _Batches.submitted
    assert len(sub) == 3
    assert sub[0]["params"]["system"] == [{
        "type": "text", "text": "SYS", "cache_control": {"type": "ephemeral"},
    }]
    assert sub[0]["params"]["thinking"] == {"type": "adaptive"}
    # batch usage (incl. cache) is tracked on the client
    assert c.usage.calls == 2


def test_batch_empty_requests_short_circuits(fake_anthropic_batch):
    c = L.LLMClient()
    assert asyncio.run(c.batch_complete_anthropic([], prov=_prov())) == {}
    assert _Batches.submitted is None


def test_batch_cancels_on_stop(fake_anthropic_batch):
    # When should_stop() is true up front, no results are mapped back.
    c = L.LLMClient()
    reqs = [{"custom_id": "1::col", "prompt": "p", "system": "S", "max_tokens": 10}]
    out = asyncio.run(c.batch_complete_anthropic(
        reqs, prov=_prov(), poll_interval=0, should_stop=lambda: True,
    ))
    assert out == {}
