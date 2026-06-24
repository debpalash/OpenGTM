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
    assert kw["system"] == "You are terse."          # system split out of messages
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
