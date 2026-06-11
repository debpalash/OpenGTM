"""
LLM Client — Provider-agnostic wrapper for AI-powered pipeline stages.

Reads the user's configured default provider from the settings DB and makes
OpenAI-compatible chat completion calls. Automatically fails over to the next
configured provider on error.

Usage:
    from apps.api.services.leadgen.llm import llm

    result = await llm.complete("Extract company name from this text...")
    data = await llm.extract_json("Return JSON with company info...")
"""

import json
import os
import re
import sqlite3
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional

import aiohttp


# ── Provider Registry (mirrors settings.py) ─────────────────────────
# Ordered by speed for failover priority
PROVIDER_ORDER = [
    "cerebras", "groq", "sambanova", "nvidia",
    "mistral", "openrouter", "github_models", "siliconflow",
]

PROVIDER_CONFIG = {
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

    def add(self, prompt: int, completion: int, provider: str = ""):
        self.prompt_tokens += prompt
        self.completion_tokens += completion
        self.total_tokens += prompt + completion
        self.calls += 1
        if provider:
            self.provider = provider

    def to_dict(self) -> dict:
        return {
            "prompt_tokens": self.prompt_tokens,
            "completion_tokens": self.completion_tokens,
            "total_tokens": self.total_tokens,
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
        }

    def _get_providers(self) -> list[dict]:
        """Get ordered list of configured providers, default first."""
        now = time.time()
        if self._provider_cache and now - self._cache_time < 60:
            return self._provider_cache.get("providers", [])

        default_id = _read_setting("LLM_DEFAULT_PROVIDER", "cerebras")

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
