"""
LLM Client — Provider-agnostic wrapper for AI-powered pipeline stages.

Reads the user's configured default provider from the settings DB and makes
chat completion calls. Most providers speak the OpenAI-compatible HTTP contract;
Anthropic (Claude) is a first-class provider that uses the native Messages API
via the official `anthropic` SDK. Automatically fails over to the next
configured provider on error, so Claude and the free Llama/Qwen/Gemini tier
back each other up.

Usage:
    from apps.api.services.leadgen.llm import llm

    result = await llm.complete("Extract company name from this text...")
    data = await llm.extract_json("Return JSON with company info...")
"""

import json
import logging
import os
import re
import sqlite3
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional

import aiohttp

logger = logging.getLogger("leadgen.llm")


# ── Provider Registry (mirrors settings.py) ─────────────────────────
# Ordered by speed for failover priority. Anthropic (Claude) is first-class but
# kept LAST in this priority list so the free OSS tier is never displaced as a
# *fallback*: Claude only leads when it's the configured/auto-selected default
# (see _get_providers). The free Llama/Qwen/Gemini chain remains intact behind it.
PROVIDER_ORDER = [
    "cerebras", "groq", "sambanova", "nvidia",
    "mistral", "openrouter", "github_models", "siliconflow",
    "anthropic",
]

# Anthropic is NOT OpenAI-compatible — it uses the native Messages API via the
# official `anthropic` SDK. _call_provider branches on this id. The model id
# defaults to Claude Opus 4.8 (latest), overridable via ANTHROPIC_MODEL.
ANTHROPIC_PROVIDER_ID = "anthropic"
ANTHROPIC_DEFAULT_MODEL = "claude-opus-4-8"

PROVIDER_CONFIG = {
    "anthropic":     {"env_key": "ANTHROPIC_API_KEY",     "env_url": "ANTHROPIC_BASE_URL",     "env_model": "ANTHROPIC_MODEL",     "default_url": "https://api.anthropic.com",            "default_model": ANTHROPIC_DEFAULT_MODEL,      "token_param": "max_tokens", "native": "anthropic"},
    "cerebras":      {"env_key": "CEREBRAS_API_KEY",      "env_url": "CEREBRAS_BASE_URL",      "env_model": "CEREBRAS_MODEL",      "default_url": "https://api.cerebras.ai/v1",           "default_model": "llama-3.3-70b",              "token_param": "max_completion_tokens"},
    "groq":          {"env_key": "GROQ_API_KEY",          "env_url": "GROQ_BASE_URL",          "env_model": "GROQ_MODEL",          "default_url": "https://api.groq.com/openai/v1",       "default_model": "llama-3.3-70b-versatile",    "token_param": "max_tokens"},
    "sambanova":     {"env_key": "SAMBANOVA_API_KEY",     "env_url": "SAMBANOVA_BASE_URL",     "env_model": "SAMBANOVA_MODEL",     "default_url": "https://api.sambanova.ai/v1",          "default_model": "Meta-Llama-3.3-70B-Instruct","token_param": "max_tokens"},
    "nvidia":        {"env_key": "NVIDIA_API_KEY",        "env_url": "NVIDIA_BASE_URL",        "env_model": "NVIDIA_MODEL",        "default_url": "https://integrate.api.nvidia.com/v1",  "default_model": "meta/llama-3.3-70b-instruct","token_param": "max_tokens"},
    "mistral":       {"env_key": "MISTRAL_API_KEY",       "env_url": "MISTRAL_BASE_URL",       "env_model": "MISTRAL_MODEL",       "default_url": "https://api.mistral.ai/v1",            "default_model": "mistral-small-latest",       "token_param": "max_tokens"},
    "openrouter":    {"env_key": "OPENROUTER_API_KEY",    "env_url": "OPENROUTER_BASE_URL",    "env_model": "OPENROUTER_MODEL",    "default_url": "https://openrouter.ai/api/v1",         "default_model": "meta-llama/llama-3.3-70b-instruct:free", "token_param": "max_tokens"},
    "github_models": {"env_key": "GITHUB_MODELS_API_KEY", "env_url": "GITHUB_MODELS_BASE_URL", "env_model": "GITHUB_MODELS_MODEL", "default_url": "https://models.inference.ai.azure.com", "default_model": "gpt-4o",                    "token_param": "max_tokens"},
    "siliconflow":   {"env_key": "SILICONFLOW_API_KEY",   "env_url": "SILICONFLOW_BASE_URL",   "env_model": "SILICONFLOW_MODEL",   "default_url": "https://api.siliconflow.cn/v1",        "default_model": "Qwen/Qwen3-8B",             "token_param": "max_tokens"},
}

# ── Settings DB Access ───────────────────────────────────────────────

_DB_PATH = Path(__file__).parent.parent.parent.parent.parent / "data" / "data.db"


def _read_setting(key: str, default: str = "") -> str:
    """Read a setting from the DB, fall back to env."""
    try:
        conn = sqlite3.connect(str(_DB_PATH))
        conn.row_factory = sqlite3.Row
        row = conn.execute("SELECT value FROM settings WHERE key = ?", (key,)).fetchone()
        conn.close()
        if row and row["value"]:
            return row["value"]
    except Exception:
        pass
    return os.environ.get(key, default)


# ── Token Tracking ───────────────────────────────────────────────────

@dataclass
class TokenUsage:
    """Tracks token usage across a pipeline run."""
    prompt_tokens: int = 0
    completion_tokens: int = 0
    total_tokens: int = 0
    calls: int = 0
    provider: str = ""
    errors: list = field(default_factory=list)
    # Anthropic prompt-caching accounting. cache_write tokens bill at ~1.25x of
    # input; cache_read tokens bill at ~0.1x — so a high read count is the signal
    # that the static AI-column prefix is being reused across rows for cheap.
    cache_write_tokens: int = 0
    cache_read_tokens: int = 0

    def add(self, prompt: int, completion: int, provider: str = "",
            cache_write: int = 0, cache_read: int = 0):
        self.prompt_tokens += prompt
        self.completion_tokens += completion
        self.total_tokens += prompt + completion
        self.cache_write_tokens += cache_write
        self.cache_read_tokens += cache_read
        self.calls += 1
        if provider:
            self.provider = provider

    def to_dict(self) -> dict:
        return {
            "prompt_tokens": self.prompt_tokens,
            "completion_tokens": self.completion_tokens,
            "total_tokens": self.total_tokens,
            "cache_write_tokens": self.cache_write_tokens,
            "cache_read_tokens": self.cache_read_tokens,
            "calls": self.calls,
            "provider": self.provider,
            "errors": self.errors[-5:],  # Keep last 5 errors
        }


# ── LLM Client ──────────────────────────────────────────────────────

class LLMClient:
    """Provider-agnostic LLM client with automatic failover."""

    def __init__(self):
        self._provider_cache: dict = {}
        self._cache_time: float = 0
        self.usage = TokenUsage()

    def _get_provider_config(self, provider_id: str) -> Optional[dict]:
        """Get resolved config (API key, URL, model) for a provider."""
        cfg = PROVIDER_CONFIG.get(provider_id)
        if not cfg:
            return None

        api_key = _read_setting(cfg["env_key"], "")
        if not api_key:
            return None

        return {
            "id": provider_id,
            "api_key": api_key,
            "base_url": _read_setting(cfg.get("env_url", ""), cfg["default_url"]),
            "model": _read_setting(cfg.get("env_model", ""), cfg["default_model"]),
            "token_param": cfg.get("token_param", "max_tokens"),
            "native": cfg.get("native", ""),  # "" = OpenAI-compatible; "anthropic" = native SDK
        }

    def _resolve_default_provider(self) -> str:
        """Pick the default provider id.

        Precedence:
          1. An explicit LLM_DEFAULT_PROVIDER setting always wins (user choice).
          2. Otherwise, if an Anthropic key is present AND this is a cloud
             deployment (YUPCHA_CLOUD truthy), Claude becomes the default.
             Self-hosted/OSS installs stay opt-in: without the cloud flag the
             free chain leads, so the OSS base is never silently switched to a
             paid model.
          3. Fallback to "cerebras" (the historical free default).

        Either way the *fallback chain* is unaffected — Claude failing over to
        the free providers (and vice-versa) is handled in _get_providers.
        """
        explicit = _read_setting("LLM_DEFAULT_PROVIDER", "")
        if explicit:
            return explicit

        cloud = str(_read_setting("YUPCHA_CLOUD", "")).strip().lower() in ("1", "true", "yes", "on")
        if cloud and self._get_provider_config(ANTHROPIC_PROVIDER_ID):
            return ANTHROPIC_PROVIDER_ID

        return "cerebras"

    def _get_providers(self) -> list[dict]:
        """Get ordered list of configured providers, default first."""
        now = time.time()
        if self._provider_cache and now - self._cache_time < 60:
            return self._provider_cache.get("providers", [])

        default_id = self._resolve_default_provider()

        # Build ordered list: default first, then by speed priority
        providers = []
        seen = set()

        # Default provider first
        cfg = self._get_provider_config(default_id)
        if cfg:
            providers.append(cfg)
            seen.add(default_id)

        # Then remaining by speed priority
        for pid in PROVIDER_ORDER:
            if pid not in seen:
                cfg = self._get_provider_config(pid)
                if cfg:
                    providers.append(cfg)
                    seen.add(pid)

        self._provider_cache = {"providers": providers}
        self._cache_time = now
        return providers

    async def complete(
        self,
        prompt: str,
        system: str = "",
        max_tokens: int = 1024,
        temperature: float = 0.2,
    ) -> str:
        """Send a chat completion request, with automatic failover.

        Returns the response text, or empty string on total failure.
        """
        providers = self._get_providers()
        if not providers:
            self.usage.errors.append("No LLM providers configured")
            return ""

        messages = []
        if system:
            messages.append({"role": "system", "content": system})
        messages.append({"role": "user", "content": prompt})

        import asyncio as _asyncio
        for prov in providers[:3]:  # Try up to 3 providers
            # Retry each provider with exponential backoff + jitter on a
            # rate-limit (429). Under high row-concurrency many AI cells call the
            # LLM at once and a free tier throttles — without this the call just
            # returns empty ("llm_returned_empty") and the cell fails. Backoff
            # staggers the retries so they succeed instead of all failing.
            for attempt in range(4):
                try:
                    result = await self._call_provider(prov, messages, max_tokens, temperature)
                    if result:
                        return result
                    break  # empty but no error → try the next provider
                except Exception as e:
                    msg = str(e)
                    self.usage.errors.append(f"{prov['id']}: {msg[:100]}")
                    rate_limited = any(s in msg.lower() for s in ("429", "rate", "quota", "too many"))
                    if rate_limited and attempt < 3:
                        # 0.5·2^n seconds + jitter derived from the attempt (no RNG).
                        delay = 0.5 * (2 ** attempt) + (attempt * 0.37)
                        await _asyncio.sleep(delay)
                        continue
                    break  # non-rate-limit error or out of attempts → next provider

        return ""

    async def extract_json(
        self,
        prompt: str,
        system: str = "",
        max_tokens: int = 1024,
    ) -> dict:
        """Send a prompt expecting JSON response, with retry on parse failure.

        Returns parsed dict, or empty dict on failure.
        """
        json_system = (system + "\n\n" if system else "") + "IMPORTANT: Respond with valid JSON only. No markdown, no code fences, no explanation."

        for attempt in range(2):
            text = await self.complete(prompt, system=json_system, max_tokens=max_tokens)
            if not text:
                continue

            # Try to extract JSON from response
            parsed = self._parse_json(text)
            if parsed is not None:
                return parsed

            # Retry with stronger instruction
            if attempt == 0:
                prompt = prompt + "\n\nYour previous response was not valid JSON. Return ONLY a JSON object, nothing else."

        return {}

    def _parse_json(self, text: str) -> Optional[dict]:
        """Robustly parse JSON from LLM response."""
        text = text.strip()

        # Remove markdown code fences
        if text.startswith("```"):
            text = re.sub(r"^```(?:json)?\s*", "", text)
            text = re.sub(r"\s*```$", "", text)

        # Try direct parse
        try:
            return json.loads(text)
        except json.JSONDecodeError:
            pass

        # Try to find JSON object in the text
        match = re.search(r'\{[^{}]*(?:\{[^{}]*\}[^{}]*)*\}', text, re.DOTALL)
        if match:
            try:
                return json.loads(match.group())
            except json.JSONDecodeError:
                pass

        return None

    async def _call_provider(
        self,
        prov: dict,
        messages: list,
        max_tokens: int,
        temperature: float,
    ) -> str:
        """Dispatch one completion to a provider.

        Anthropic (Claude) uses the native Messages API via the official SDK;
        every other provider speaks the OpenAI-compatible chat-completions HTTP
        contract handled by _call_openai_compatible.
        """
        if prov.get("native") == "anthropic":
            return await self._call_anthropic(prov, messages, max_tokens)
        return await self._call_openai_compatible(prov, messages, max_tokens, temperature)

    @staticmethod
    def _split_anthropic_messages(messages: list) -> tuple[list[str], list[dict]]:
        """Split OpenAI-style messages into Anthropic's (system_parts, turns).

        The shared message list uses OpenAI's {"role","content"} shape with an
        optional leading "system" message. Anthropic takes the system prompt as a
        separate top-level argument and only user/assistant turns in `messages`.
        """
        system_parts: list[str] = []
        turns: list[dict] = []
        for m in messages:
            role = m.get("role")
            content = m.get("content", "")
            if role == "system":
                if content:
                    system_parts.append(content)
            else:
                # Anthropic accepts only "user"/"assistant" roles.
                turns.append({"role": "user" if role == "user" else "assistant", "content": content})
        if not turns:
            turns = [{"role": "user", "content": ""}]
        return system_parts, turns

    @staticmethod
    def _anthropic_system_blocks(system_parts: list[str]) -> Optional[list]:
        """Build a cache-breakpointed `system` value from joined system parts.

        Prompt caching is a prefix match: the AI-column system prompt is
        byte-identical across every row of a column, so a single
        `cache_control: {type: "ephemeral"}` breakpoint on it lets the first row
        write the prefix and every subsequent row read it at ~10% of input cost.
        Returns None when there's no system prompt (nothing to cache).

        Disable via LLM_PROMPT_CACHE=0 (then we fall back to a plain string).
        """
        if not system_parts:
            return None
        text = "\n\n".join(system_parts)
        cache_on = str(os.getenv("LLM_PROMPT_CACHE", "1")).strip().lower() not in ("0", "false", "no", "off")
        if not cache_on:
            return [{"type": "text", "text": text}]
        return [{"type": "text", "text": text, "cache_control": {"type": "ephemeral"}}]

    @staticmethod
    def _anthropic_cache_tokens(usage) -> tuple[int, int]:
        """Pull (cache_write, cache_read) token counts off an Anthropic usage obj."""
        cache_write = getattr(usage, "cache_creation_input_tokens", 0) or 0
        cache_read = getattr(usage, "cache_read_input_tokens", 0) or 0
        return cache_write, cache_read

    def _make_anthropic_client(self, prov: dict):
        """Construct an AsyncAnthropic client for a resolved provider config."""
        from anthropic import AsyncAnthropic  # caller handles ImportError

        client_kwargs: dict = {"api_key": prov["api_key"]}
        base_url = (prov.get("base_url") or "").strip()
        # Only pass base_url if it's a real override (not the default api host),
        # so the SDK's own default/versioning is used in the common case.
        if base_url and base_url not in ("https://api.anthropic.com", "https://api.anthropic.com/v1"):
            client_kwargs["base_url"] = base_url
        return AsyncAnthropic(**client_kwargs)

    async def _call_anthropic(
        self,
        prov: dict,
        messages: list,
        max_tokens: int,
    ) -> str:
        """Make a single Claude call via the official `anthropic` SDK.

        Uses adaptive thinking (recommended for Opus 4.8) and the async client so
        we don't block the event loop. A `cache_control` breakpoint is placed on
        the system prompt so the static AI-column prefix is cached across the many
        per-row calls that share it (see _anthropic_system_blocks).
        """
        try:
            from anthropic import AsyncAnthropic  # noqa: F401  (import-presence check)
        except ImportError as e:
            raise RuntimeError(f"anthropic SDK not installed: {e}")

        system_parts, turns = self._split_anthropic_messages(messages)

        timeout = float(os.getenv("LLM_HTTP_TIMEOUT", "60"))
        client = self._make_anthropic_client(prov)
        try:
            create_kwargs: dict = {
                "model": prov["model"],
                "max_tokens": max_tokens,
                "messages": turns,
                # Adaptive thinking is the recommended mode on Opus 4.8 / 4.7.
                "thinking": {"type": "adaptive"},
            }
            system_blocks = self._anthropic_system_blocks(system_parts)
            if system_blocks is not None:
                create_kwargs["system"] = system_blocks

            resp = await client.with_options(timeout=timeout).messages.create(**create_kwargs)

            # Track token usage in the same shape as OpenAI usage, plus cache hits.
            usage = getattr(resp, "usage", None)
            prompt_tok = getattr(usage, "input_tokens", 0) or 0
            completion_tok = getattr(usage, "output_tokens", 0) or 0
            cache_write, cache_read = self._anthropic_cache_tokens(usage)
            self.usage.add(prompt=prompt_tok, completion=completion_tok, provider=prov["id"],
                           cache_write=cache_write, cache_read=cache_read)

            try:
                from apps.api.services.leadgen.db import LeadDB
                db = LeadDB()
                db.record_llm_usage(prov["id"], prov["model"], prompt_tok, completion_tok)
                db.close()
            except Exception:
                pass  # never fail the call over tracking

            # Concatenate text blocks (skip thinking blocks).
            parts = [
                getattr(b, "text", "")
                for b in (getattr(resp, "content", None) or [])
                if getattr(b, "type", "") == "text"
            ]
            return "".join(parts).strip()
        finally:
            try:
                await client.close()
            except Exception:
                pass

    # ── Native tool-use (Anthropic-only manual agentic loop) ───────────

    async def anthropic_tool_call(
        self,
        messages: list,
        *,
        system=None,
        tools: Optional[list] = None,
        tool_choice: Optional[dict] = None,
        output_format: Optional[dict] = None,
        max_tokens: int = 1024,
        prov: Optional[dict] = None,
    ):
        """One native Messages-API turn with tools / tool_choice / structured output.

        Returns the **raw** Anthropic response object (content blocks +
        stop_reason + usage) so the CALLER can append the full `response.content`
        back to history unchanged — preserving `thinking` and `tool_use` blocks
        (the API rejects *modified* thinking blocks on the same model). This is a
        new low-level method: `complete`/`extract_json`/`_call_anthropic` are left
        untouched.

        `messages` is the Anthropic-native turn list ([{"role","content"},...])
        where `content` may already be a list of blocks (assistant turns) or a
        string/blocks (user turns). `system` may be a string or a list of system
        blocks; we cache-breakpoint a string for the static research/plan prefix.
        Raises RuntimeError when the SDK is missing (callers catch → fallback).
        """
        try:
            from anthropic import AsyncAnthropic  # noqa: F401  (presence check)
        except ImportError as e:
            raise RuntimeError(f"anthropic SDK not installed: {e}")

        prov = prov or self.anthropic_provider()
        if not prov:
            raise RuntimeError("anthropic provider not configured")

        timeout = float(os.getenv("LLM_HTTP_TIMEOUT", "60"))
        client = self._make_anthropic_client(prov)
        try:
            create_kwargs: dict = {
                "model": prov["model"],
                "max_tokens": max_tokens,
                "messages": messages,
                "thinking": {"type": "adaptive"},
            }
            if system is not None:
                if isinstance(system, str):
                    blocks = self._anthropic_system_blocks([system])
                    if blocks is not None:
                        create_kwargs["system"] = blocks
                else:
                    create_kwargs["system"] = system
            if tools:
                create_kwargs["tools"] = tools
            if tool_choice is not None:
                create_kwargs["tool_choice"] = tool_choice
            if output_format is not None:
                # output_config.format is incompatible with forced tool_choice /
                # prefilling; synthesis callers pass no tool_choice.
                create_kwargs["output_config"] = {"format": output_format}

            resp = await client.with_options(timeout=timeout).messages.create(**create_kwargs)

            # Usage accounting — read the FINAL response usage ONCE (SDK retries
            # return one final usage; never per-attempt → no double counting).
            usage = getattr(resp, "usage", None)
            prompt_tok = getattr(usage, "input_tokens", 0) or 0
            completion_tok = getattr(usage, "output_tokens", 0) or 0
            cache_write, cache_read = self._anthropic_cache_tokens(usage)
            self.usage.add(prompt=prompt_tok, completion=completion_tok, provider=prov["id"],
                           cache_write=cache_write, cache_read=cache_read)
            try:
                from apps.api.services.leadgen.db import LeadDB
                db = LeadDB()
                db.record_llm_usage(prov["id"], prov["model"], prompt_tok, completion_tok)
                db.close()
            except Exception:
                pass  # never fail the call over tracking
            return resp
        finally:
            try:
                await client.close()
            except Exception:
                pass

    # Per-MTok pricing resolved from the model registry / known tiers. Failing
    # CLOSED (unknown model → most-expensive tier) means an overridden/unknown
    # ANTHROPIC_MODEL trips the per-cell budget EARLY (conservative), never runs
    # away. (USD per 1M tokens.)
    _ANTHROPIC_PRICES: dict = {
        "claude-opus-4-8": (5.0, 25.0),
        "claude-opus-4-7": (5.0, 25.0),
        "claude-opus-4-6": (5.0, 25.0),
        "claude-opus-4-5": (5.0, 25.0),
        "claude-sonnet-4-6": (3.0, 15.0),
        "claude-haiku-4-5": (1.0, 5.0),
        "claude-fable-5": (10.0, 50.0),
        "claude-mythos-5": (10.0, 50.0),
    }
    # Most-expensive known tier — the fail-closed default.
    _ANTHROPIC_MAX_PRICE = (10.0, 50.0)

    def _anthropic_price_for_model(self, model: str) -> tuple[float, float]:
        """(input_$/MTok, output_$/MTok) for a model id; fail closed on unknown.

        Match on a known prefix so date-suffixed ids resolve; otherwise price at
        the most-expensive known tier so the budget never under-counts.
        """
        m = (model or "").strip()
        if m in self._ANTHROPIC_PRICES:
            return self._ANTHROPIC_PRICES[m]
        for key, price in self._ANTHROPIC_PRICES.items():
            if m.startswith(key):
                return price
        return self._ANTHROPIC_MAX_PRICE

    def anthropic_cost_usd(self, usage, prov: Optional[dict] = None) -> float:
        """USD cost of one Anthropic turn from its `usage`, fail-closed pricing.

        Prices input/output (+ cache read at ~0.1x, cache write at ~1.25x) using
        the active model's per-MTok rate. Reads usage ONCE from the final
        response (callers pass `resp.usage`), so a retried turn neither inflates
        nor under-counts spend. Unknown/overridden model → most-expensive tier.
        """
        if usage is None:
            return 0.0
        model = (prov or self.anthropic_provider() or {}).get("model") or ANTHROPIC_DEFAULT_MODEL
        in_rate, out_rate = self._anthropic_price_for_model(model)
        input_tok = getattr(usage, "input_tokens", 0) or 0
        output_tok = getattr(usage, "output_tokens", 0) or 0
        cache_write, cache_read = self._anthropic_cache_tokens(usage)
        cost = (
            input_tok * in_rate
            + output_tok * out_rate
            + cache_write * in_rate * 1.25
            + cache_read * in_rate * 0.1
        ) / 1_000_000.0
        return cost

    # ── Message Batches (Anthropic-only, bulk per-row column runs) ──────

    def anthropic_provider(self) -> Optional[dict]:
        """Return the resolved Anthropic provider config iff it's the default.

        Batch submission only makes sense when Anthropic is the provider actually
        serving the run (it's the default-selected provider), so the bulk path
        keys off this. Returns None for non-Anthropic defaults / no key, in which
        case callers fall back to the synchronous per-row path.
        """
        providers = self._get_providers()
        if providers and providers[0].get("native") == "anthropic":
            return providers[0]
        return None

    async def batch_complete_anthropic(
        self,
        requests: list[dict],
        prov: Optional[dict] = None,
        poll_interval: float = 5.0,
        max_wait: float = 24 * 3600.0,
        should_stop=None,
    ) -> dict:
        """Run many completions as ONE Anthropic Message Batch (~50% cost, async).

        Each entry in `requests` is {"custom_id", "prompt", "system", "max_tokens"}.
        Returns {custom_id: text} for every request that succeeded; failed/errored
        custom_ids are simply absent so the caller can fall back per-row.

        Prompt caching applies inside the batch too — the shared system prefix is
        cache-breakpointed once per request, so identical AI-column system prompts
        are deduplicated across the batch. Results arrive in any order, so we key
        strictly by custom_id (never by position).

        `should_stop` (optional, sync callable) is polled while waiting; if it
        returns True we cancel the batch and return whatever has completed.
        """
        if not requests:
            return {}
        try:
            from anthropic.types.message_create_params import MessageCreateParamsNonStreaming
            from anthropic.types.messages.batch_create_params import Request
        except ImportError as e:
            raise RuntimeError(f"anthropic SDK missing batch types: {e}")

        prov = prov or self.anthropic_provider()
        if not prov:
            raise RuntimeError("anthropic provider not configured for batch run")

        model = prov["model"]
        batch_reqs = []
        for r in requests:
            system_blocks = self._anthropic_system_blocks(
                [r["system"]] if r.get("system") else []
            )
            params: dict = {
                "model": model,
                "max_tokens": int(r.get("max_tokens", 1024)),
                "messages": [{"role": "user", "content": r.get("prompt", "")}],
                "thinking": {"type": "adaptive"},
            }
            if system_blocks is not None:
                params["system"] = system_blocks
            batch_reqs.append(Request(
                custom_id=str(r["custom_id"]),
                params=MessageCreateParamsNonStreaming(**params),
            ))

        import asyncio as _asyncio
        client = self._make_anthropic_client(prov)
        results: dict = {}
        try:
            batch = await client.messages.batches.create(requests=batch_reqs)
            batch_id = batch.id

            cancelled = False
            waited = 0.0
            while True:
                if should_stop is not None and should_stop():
                    try:
                        await client.messages.batches.cancel(batch_id)
                    except Exception:
                        pass
                    cancelled = True
                    break
                batch = await client.messages.batches.retrieve(batch_id)
                if getattr(batch, "processing_status", "") == "ended":
                    break
                if waited >= max_wait:
                    logger.warning(f"anthropic batch {batch_id} exceeded max_wait; abandoning")
                    break
                await _asyncio.sleep(poll_interval)
                waited += poll_interval

            if cancelled:
                return results  # stopped mid-run → don't map partial results back

            # Stream results; key by custom_id. Track usage incl. cache reads.
            try:
                async for item in await client.messages.batches.results(batch_id):
                    res = getattr(item, "result", None)
                    if getattr(res, "type", "") != "succeeded":
                        continue
                    msg = getattr(res, "message", None)
                    usage = getattr(msg, "usage", None)
                    if usage is not None:
                        cw, cr = self._anthropic_cache_tokens(usage)
                        self.usage.add(
                            prompt=getattr(usage, "input_tokens", 0) or 0,
                            completion=getattr(usage, "output_tokens", 0) or 0,
                            provider=prov["id"], cache_write=cw, cache_read=cr,
                        )
                    parts = [
                        getattr(b, "text", "")
                        for b in (getattr(msg, "content", None) or [])
                        if getattr(b, "type", "") == "text"
                    ]
                    text = "".join(parts).strip()
                    if text:
                        results[str(getattr(item, "custom_id", ""))] = text
            except Exception as e:
                logger.warning(f"anthropic batch {batch_id} results read failed: {e}")
        finally:
            try:
                await client.close()
            except Exception:
                pass
        return results

    async def _call_openai_compatible(
        self,
        prov: dict,
        messages: list,
        max_tokens: int,
        temperature: float,
    ) -> str:
        """Make a single OpenAI-compatible chat completion call."""
        url = f"{prov['base_url']}/chat/completions"
        headers = {
            "Authorization": f"Bearer {prov['api_key']}",
            "Content-Type": "application/json",
        }

        body: dict = {
            "model": prov["model"],
            "messages": messages,
            prov["token_param"]: max_tokens,
            "temperature": temperature,
        }

        async with aiohttp.ClientSession() as session:
            async with session.post(
                url, headers=headers, json=body,
                timeout=aiohttp.ClientTimeout(total=int(os.getenv("LLM_HTTP_TIMEOUT", "60"))),
            ) as resp:
                data = await resp.json()

                if resp.status != 200:
                    err = data.get("error", {})
                    msg = err.get("message", "") if isinstance(err, dict) else str(err)
                    raise RuntimeError(f"HTTP {resp.status}: {msg[:100]}")

                # Track token usage
                usage = data.get("usage", {})
                prompt_tok = usage.get("prompt_tokens", 0)
                completion_tok = usage.get("completion_tokens", 0)
                self.usage.add(
                    prompt=prompt_tok,
                    completion=completion_tok,
                    provider=prov["id"],
                )

                # Persist to DB + capture rate limit headers
                try:
                    from apps.api.services.leadgen.db import LeadDB
                    db = LeadDB()
                    # Capture rate limit headers (most providers send these)
                    rate_limit = int(resp.headers.get("X-RateLimit-Limit", 0) or
                                    resp.headers.get("x-ratelimit-limit-requests", 0) or 0)
                    rate_remaining = int(resp.headers.get("X-RateLimit-Remaining", 0) or
                                        resp.headers.get("x-ratelimit-remaining-requests", 0) or 0)
                    rate_reset = resp.headers.get("X-RateLimit-Reset", "") or resp.headers.get("x-ratelimit-reset-requests", "")
                    db.record_llm_usage(
                        prov["id"], prov["model"], prompt_tok, completion_tok,
                        rate_limit=rate_limit, rate_remaining=rate_remaining, rate_reset=str(rate_reset),
                    )
                    db.close()
                except Exception:
                    pass  # Don't fail the call over tracking

                # Extract response text
                choices = data.get("choices", [])
                if not choices:
                    return ""

                msg = choices[0].get("message", {})
                return (msg.get("content") or msg.get("reasoning") or "").strip()

    def reset_usage(self):
        """Reset token usage tracking for a new job."""
        self.usage = TokenUsage()


# ── Module-level singleton ───────────────────────────────────────────
llm = LLMClient()
