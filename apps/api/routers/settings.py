"""
Settings API — Manage LLM providers and system configuration.
Persists settings to SQLite database. Seeds from .env on first run.
"""

import os
import sqlite3
from typing import Optional, Dict
from pathlib import Path
from fastapi import APIRouter, HTTPException, Depends
from pydantic import BaseModel
from apps.api.core.security import get_current_active_user

# Load .env into os.environ so we can seed DB from it
_env_path = Path(__file__).resolve().parents[3] / ".env"
if _env_path.exists():
    for line in _env_path.read_text().splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            key, _, val = line.partition("=")
            key, val = key.strip(), val.strip()
            if key and val and key not in os.environ:
                os.environ[key] = val

router = APIRouter(prefix="/api/settings", tags=["Settings"])



# ── Database ───────────────────────────────────────────

def _get_db() -> sqlite3.Connection:
    """Get a connection to the main data DB with settings table."""
    # Use project root (3 levels up from this file) for consistent path
    project_root = Path(__file__).resolve().parents[3]
    db_path = project_root / "data" / "data.db"
    db_path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(db_path))
    conn.row_factory = sqlite3.Row
    conn.execute("""
        CREATE TABLE IF NOT EXISTS settings (
            key   TEXT PRIMARY KEY,
            value TEXT DEFAULT ''
        )
    """)
    conn.commit()
    return conn


def _db_get(key: str, default: str = "") -> str:
    """Read a setting from DB, fallback to os.environ, then default."""
    try:
        conn = _get_db()
        row = conn.execute("SELECT value FROM settings WHERE key = ?", (key,)).fetchone()
        conn.close()
        if row and row["value"]:
            return row["value"]
    except Exception:
        pass
    return os.environ.get(key, default)


def _db_set(key: str, value: str):
    """Write a setting to DB and os.environ."""
    try:
        conn = _get_db()
        conn.execute(
            "INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?",
            (key, value, value)
        )
        conn.commit()
        conn.close()
    except Exception as e:
        print(f"  ⚠ Settings DB write error: {e}")
    os.environ[key] = value


def _seed_from_env():
    """Sync .env keys into the settings DB (insert missing, don't overwrite existing)."""
    try:
        conn = _get_db()
        # Get existing DB keys
        existing = {r["key"] for r in conn.execute("SELECT key FROM settings").fetchall()}

        seeded = 0
        for pid, prov in PROVIDERS.items():
            for env_key in [prov["env_key"], prov.get("env_url", ""), prov.get("env_model", "")]:
                if env_key and env_key not in existing:
                    val = os.environ.get(env_key, "")
                    if val:
                        conn.execute(
                            "INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)",
                            (env_key, val)
                        )
                        seeded += 1

        # Seed default provider
        if "LLM_DEFAULT_PROVIDER" not in existing:
            default = os.environ.get("LLM_DEFAULT_PROVIDER", "openrouter")
            conn.execute("INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)",
                         ("LLM_DEFAULT_PROVIDER", default))

        conn.commit()
        conn.close()
        if seeded:
            print(f"  🌱 Seeded {seeded} settings from .env")
    except Exception as e:
        print(f"  ⚠ Seed error: {e}")


# ── Provider Registry ──────────────────────────────────

PROVIDERS = {
    "openrouter": {
        "name": "OpenRouter",
        "env_key": "OPENROUTER_API_KEY",
        "env_url": "OPENROUTER_BASE_URL",
        "env_model": "OPENROUTER_MODEL",
        "default_url": "https://openrouter.ai/api/v1",
        "default_model": "meta-llama/llama-3.3-70b-instruct:free",
        "docs": "https://openrouter.ai/keys",
        "free_tier": "20 RPM, 50 RPD (1K with $10 topup)",
        "icon": "🌐",
        "openai_compatible": True,
    },
    "google_ai": {
        "name": "Google AI Studio",
        "env_key": "GOOGLE_AI_API_KEY",
        "env_url": "GOOGLE_AI_BASE_URL",
        "env_model": "GOOGLE_AI_MODEL",
        "default_url": "https://generativelanguage.googleapis.com/v1beta/openai",
        "default_model": "gemini-2.5-flash",
        "docs": "https://aistudio.google.com/app/apikey",
        "free_tier": "10 RPM, 1500 RPD",
        "icon": "🔷",
        "openai_compatible": True,
    },
    "nvidia": {
        "name": "NVIDIA NIM",
        "env_key": "NVIDIA_API_KEY",
        "env_url": "NVIDIA_BASE_URL",
        "env_model": "NVIDIA_MODEL",
        "default_url": "https://integrate.api.nvidia.com/v1",
        "default_model": "meta/llama-3.3-70b-instruct",
        "docs": "https://build.nvidia.com/explore/discover",
        "free_tier": "40 RPM (phone verification)",
        "icon": "💚",
        "openai_compatible": True,
    },
    "groq": {
        "name": "Groq",
        "env_key": "GROQ_API_KEY",
        "env_url": "GROQ_BASE_URL",
        "env_model": "GROQ_MODEL",
        "default_url": "https://api.groq.com/openai/v1",
        "default_model": "llama-3.3-70b-versatile",
        "docs": "https://console.groq.com/keys",
        "free_tier": "30 RPM, 1K RPD",
        "icon": "⚡",
        "openai_compatible": True,
    },
    "cerebras": {
        "name": "Cerebras",
        "env_key": "CEREBRAS_API_KEY",
        "env_url": "CEREBRAS_BASE_URL",
        "env_model": "CEREBRAS_MODEL",
        "default_url": "https://api.cerebras.ai/v1",
        "default_model": "llama-3.3-70b",
        "docs": "https://cloud.cerebras.ai/",
        "free_tier": "30 RPM, 14,400 RPD",
        "icon": "🧠",
        "openai_compatible": True,
    },
    "mistral": {
        "name": "Mistral AI",
        "env_key": "MISTRAL_API_KEY",
        "env_url": "MISTRAL_BASE_URL",
        "env_model": "MISTRAL_MODEL",
        "default_url": "https://api.mistral.ai/v1",
        "default_model": "mistral-small-latest",
        "docs": "https://console.mistral.ai/api-keys",
        "free_tier": "1 req/s, 1B tok/month",
        "icon": "🔮",
        "openai_compatible": True,
    },
    "cohere": {
        "name": "Cohere",
        "env_key": "COHERE_API_KEY",
        "env_url": "COHERE_BASE_URL",
        "env_model": "COHERE_MODEL",
        "default_url": "https://api.cohere.ai/v2",
        "default_model": "command-a-03-2025",
        "docs": "https://dashboard.cohere.com/api-keys",
        "free_tier": "20 RPM, 1K/month",
        "icon": "🐚",
        "openai_compatible": False,
    },
    "github_models": {
        "name": "GitHub Models",
        "env_key": "GITHUB_MODELS_API_KEY",
        "env_url": "GITHUB_MODELS_BASE_URL",
        "env_model": "GITHUB_MODELS_MODEL",
        "default_url": "https://models.inference.ai.azure.com",
        "default_model": "gpt-4o",
        "docs": "https://github.com/marketplace/models",
        "free_tier": "10-15 RPM, 50-150 RPD",
        "icon": "🐙",
        "openai_compatible": True,
    },
    "cloudflare": {
        "name": "Cloudflare Workers AI",
        "env_key": "CLOUDFLARE_API_KEY",
        "env_url": "",
        "env_model": "CLOUDFLARE_MODEL",
        "default_url": "",
        "default_model": "@cf/meta/llama-3.3-70b-instruct-fp8-fast",
        "docs": "https://dash.cloudflare.com/profile/api-tokens",
        "free_tier": "10K neurons/day",
        "icon": "☁️",
        "openai_compatible": True,
    },
    "huggingface": {
        "name": "HuggingFace",
        "env_key": "HUGGINGFACE_API_KEY",
        "env_url": "",
        "env_model": "HUGGINGFACE_MODEL",
        "default_url": "https://api-inference.huggingface.co",
        "default_model": "meta-llama/Llama-3.3-70B-Instruct",
        "docs": "https://huggingface.co/settings/tokens",
        "free_tier": "$0.10/month credits",
        "icon": "🤗",
        "openai_compatible": False,
    },
    "sambanova": {
        "name": "SambaNova Cloud",
        "env_key": "SAMBANOVA_API_KEY",
        "env_url": "SAMBANOVA_BASE_URL",
        "env_model": "SAMBANOVA_MODEL",
        "default_url": "https://api.sambanova.ai/v1",
        "default_model": "Meta-Llama-3.3-70B-Instruct",
        "docs": "https://sambanova.ai/",
        "free_tier": "$5 trial credits",
        "icon": "🔥",
        "openai_compatible": True,
    },
    "siliconflow": {
        "name": "SiliconFlow",
        "env_key": "SILICONFLOW_API_KEY",
        "env_url": "SILICONFLOW_BASE_URL",
        "env_model": "SILICONFLOW_MODEL",
        "default_url": "https://api.siliconflow.cn/v1",
        "default_model": "Qwen/Qwen3-8B",
        "docs": "https://cloud.siliconflow.cn/account/ak",
        "free_tier": "1K RPM, 50K TPM",
        "icon": "🌊",
        "openai_compatible": True,
    },
}

# ── Enrichment Provider Registry ───────────────────────

ENRICHMENT_PROVIDERS = {
    "hunter_io": {
        "name": "Hunter.io",
        "env_key": "HUNTER_API_KEY",
        "capability": "Email Finder + Verifier",
        "free_tier": "25 lookups/mo",
        "docs": "https://hunter.io/api-documentation/v2",
        "icon": "🎯",
    },
    "apollo_io": {
        "name": "Apollo.io",
        "env_key": "APOLLO_API_KEY",
        "capability": "People + Company Enrichment",
        "free_tier": "50 credits/mo",
        "docs": "https://apolloio.github.io/apollo-api-docs/",
        "icon": "🚀",
    },
    "abstract_api": {
        "name": "AbstractAPI",
        "env_key": "ABSTRACT_API_KEY",
        "capability": "Email Validation + Deliverability",
        "free_tier": "100/mo",
        "docs": "https://www.abstractapi.com/api/email-verification-validation-api",
        "icon": "📧",
    },
    "numverify": {
        "name": "NumVerify",
        "env_key": "NUMVERIFY_API_KEY",
        "capability": "Phone Number Validation",
        "free_tier": "100/mo",
        "docs": "https://numverify.com/documentation",
        "icon": "📱",
    },
    "ipinfo": {
        "name": "IPInfo",
        "env_key": "IPINFO_TOKEN",
        "capability": "Company Data from Domain",
        "free_tier": "50K/mo",
        "docs": "https://ipinfo.io/developers",
        "icon": "🌍",
    },
}

# Seed on module load
_seed_from_env()


def _mask_key(key: str) -> str:
    """Mask an API key for display, showing only first 8 and last 4 chars."""
    if not key or len(key) < 16:
        return "••••••••" if key else ""
    return key[:8] + "••••" + key[-4:]


@router.get("/providers")
def list_providers():
    """List all configured LLM providers and their status."""
    default = _db_get("LLM_DEFAULT_PROVIDER", "openrouter")
    result = []
    for pid, prov in PROVIDERS.items():
        api_key = _db_get(prov["env_key"], "")
        base_url = _db_get(prov["env_url"], prov["default_url"]) if prov.get("env_url") else prov["default_url"]
        model = _db_get(prov["env_model"], prov["default_model"]) if prov.get("env_model") else prov["default_model"]
        result.append({
            "id": pid,
            "name": prov["name"],
            "icon": prov["icon"],
            "configured": bool(api_key),
            "api_key_masked": _mask_key(api_key),
            "base_url": base_url,
            "model": model,
            "default_model": prov["default_model"],
            "docs_url": prov["docs"],
            "free_tier": prov["free_tier"],
            "is_default": pid == default,
            "openai_compatible": prov["openai_compatible"],
        })
    return {"providers": result, "default_provider": default}


class ProviderUpdate(BaseModel):
    api_key: Optional[str] = None
    base_url: Optional[str] = None
    model: Optional[str] = None
    set_default: Optional[bool] = None


@router.put("/providers/{provider_id}")
def update_provider(provider_id: str, body: ProviderUpdate):
    """Update a provider's configuration. Persists to database."""
    if provider_id not in PROVIDERS:
        raise HTTPException(status_code=404, detail=f"Provider '{provider_id}' not found")

    prov = PROVIDERS[provider_id]

    if body.api_key is not None:
        _db_set(prov["env_key"], body.api_key)
    if body.base_url is not None and prov.get("env_url"):
        _db_set(prov["env_url"], body.base_url)
    if body.model is not None and prov.get("env_model"):
        _db_set(prov["env_model"], body.model)
    if body.set_default:
        _db_set("LLM_DEFAULT_PROVIDER", provider_id)

    return {"status": "ok", "provider": provider_id}


@router.post("/providers/{provider_id}/test")
async def test_provider(provider_id: str):
    """Send a test prompt to the provider and return the response."""
    if provider_id not in PROVIDERS:
        raise HTTPException(status_code=404, detail=f"Provider '{provider_id}' not found")

    prov = PROVIDERS[provider_id]
    api_key = _db_get(prov["env_key"], "")
    if not api_key:
        return {"status": "error", "error": "API key not configured"}

    base_url = _db_get(prov["env_url"], prov["default_url"]) if prov.get("env_url") else prov["default_url"]
    model = _db_get(prov["env_model"], prov["default_model"]) if prov.get("env_model") else prov["default_model"]

    try:
        import httpx
        async with httpx.AsyncClient(timeout=20) as client:
            if prov.get("openai_compatible", True):
                # Build request body — Cerebras uses max_completion_tokens
                req_body: dict = {
                    "model": model,
                    "messages": [{"role": "user", "content": "Say 'hello' in exactly one word."}],
                }
                if provider_id == "cerebras":
                    req_body["max_completion_tokens"] = 50
                else:
                    req_body["max_tokens"] = 50

                resp = await client.post(
                    f"{base_url}/chat/completions",
                    headers={
                        "Authorization": f"Bearer {api_key}",
                        "Content-Type": "application/json",
                    },
                    json=req_body,
                )
                data = resp.json()
                if resp.status_code == 200:
                    msg = data.get("choices", [{}])[0].get("message", {})
                    # Some models (reasoning) return content=null with text in reasoning
                    text = msg.get("content") or msg.get("reasoning") or ""
                    actual_model = data.get("model", model)
                    return {"status": "ok", "response": (text or "").strip()[:200], "model": actual_model}
                else:
                    # Extract error message from various API error formats
                    err = data.get("error", {})
                    if isinstance(err, dict):
                        err_msg = err.get("message", "") or err.get("code", "")
                    elif isinstance(err, str):
                        err_msg = err
                    else:
                        err_msg = resp.text[:200]
                    return {"status": "error", "error": err_msg or "Provider returned error"}
            else:
                if provider_id == "google_ai":
                    resp = await client.post(
                        f"{base_url}/models/{model}:generateContent?key={api_key}",
                        json={
                            "contents": [{"parts": [{"text": "Say 'hello' in exactly one word."}]}],
                        },
                    )
                    data = resp.json()
                    if resp.status_code == 200:
                        text = data.get("candidates", [{}])[0].get("content", {}).get("parts", [{}])[0].get("text", "")
                        return {"status": "ok", "response": text.strip(), "model": model}
                    else:
                        return {"status": "error", "error": data.get("error", {}).get("message", resp.text[:200])}
                else:
                    return {"status": "error", "error": "Provider not supported for testing yet"}

    except Exception as e:
        return {"status": "error", "error": str(e)}


@router.get("/system")
def get_system_settings():
    """Get system configuration."""
    return {
        "debug": os.environ.get("DEBUG", "false") == "true",
        "api_host": os.environ.get("API_HOST", "0.0.0.0"),
        "api_port": int(os.environ.get("API_PORT", 8000)),
        "cors_origins": os.environ.get("CORS_ORIGINS", "*"),
        "default_provider": _db_get("LLM_DEFAULT_PROVIDER", "openrouter"),
        "scribd_configured": bool(os.environ.get("SCRIBD_COOKIES", "")),
        "google_search_configured": bool(os.environ.get("GOOGLE_API_KEY", "")),
        "linkedin_configured": bool(os.environ.get("LINKEDIN_LI_AT_COOKIE", "")),
    }


@router.get("/models/{provider_id}")
async def list_models(provider_id: str, free_only: bool = False):
    """Fetch available models for a provider."""
    if provider_id not in PROVIDERS:
        raise HTTPException(status_code=404, detail=f"Provider '{provider_id}' not found")

    if provider_id == "openrouter":
        return await _fetch_openrouter_models(free_only)
    elif provider_id == "google_ai":
        return {"models": [
            {"id": "gemini-2.5-flash", "name": "Gemini 2.5 Flash", "context": 1048576, "free": True},
            {"id": "gemini-2.5-pro", "name": "Gemini 2.5 Pro", "context": 1048576, "free": True},
            {"id": "gemini-2.0-flash", "name": "Gemini 2.0 Flash", "context": 1048576, "free": True},
            {"id": "gemini-2.0-flash-lite", "name": "Gemini 2.0 Flash Lite", "context": 1048576, "free": True},
            {"id": "gemini-1.5-flash", "name": "Gemini 1.5 Flash", "context": 1048576, "free": True},
            {"id": "gemini-1.5-pro", "name": "Gemini 1.5 Pro", "context": 2097152, "free": True},
        ]}
    elif provider_id == "groq":
        return {"models": [
            {"id": "llama-3.3-70b-versatile", "name": "Llama 3.3 70B", "context": 131072, "free": True},
            {"id": "llama-3.1-8b-instant", "name": "Llama 3.1 8B", "context": 131072, "free": True},
            {"id": "llama-4-scout-17b-16e-instruct", "name": "Llama 4 Scout", "context": 131072, "free": True},
            {"id": "gemma2-9b-it", "name": "Gemma 2 9B", "context": 8192, "free": True},
            {"id": "qwen-qwq-32b", "name": "Qwen QwQ 32B", "context": 131072, "free": True},
            {"id": "deepseek-r1-distill-llama-70b", "name": "DeepSeek R1 70B", "context": 131072, "free": True},
        ]}
    elif provider_id == "cerebras":
        return {"models": [
            {"id": "llama-3.3-70b", "name": "Llama 3.3 70B", "context": 8192, "free": True},
            {"id": "llama3.1-8b", "name": "Llama 3.1 8B", "context": 8192, "free": True},
            {"id": "qwen-3-32b", "name": "Qwen 3 32B", "context": 8192, "free": True},
            {"id": "deepseek-r1-distill-llama-70b", "name": "DeepSeek R1 70B", "context": 8192, "free": True},
        ]}
    elif provider_id == "mistral":
        return {"models": [
            {"id": "mistral-small-latest", "name": "Mistral Small 3.1", "context": 32768, "free": True},
            {"id": "mistral-large-latest", "name": "Mistral Large", "context": 131072, "free": True},
            {"id": "ministral-8b-latest", "name": "Ministral 8B", "context": 131072, "free": True},
            {"id": "codestral-latest", "name": "Codestral", "context": 32768, "free": True},
            {"id": "pixtral-12b-2409", "name": "Pixtral 12B", "context": 131072, "free": True},
        ]}
    elif provider_id == "cohere":
        return {"models": [
            {"id": "command-a-03-2025", "name": "Command A", "context": 256000, "free": True},
            {"id": "command-r-plus-08-2024", "name": "Command R+", "context": 128000, "free": True},
            {"id": "command-r-08-2024", "name": "Command R", "context": 128000, "free": True},
            {"id": "c4ai-aya-expanse-32b", "name": "Aya Expanse 32B", "context": 128000, "free": True},
        ]}
    elif provider_id == "github_models":
        return {"models": [
            {"id": "gpt-4o", "name": "GPT-4o", "context": 128000, "free": True},
            {"id": "gpt-4o-mini", "name": "GPT-4o Mini", "context": 128000, "free": True},
            {"id": "gpt-4.1", "name": "GPT-4.1", "context": 1047576, "free": True},
            {"id": "gpt-4.1-mini", "name": "GPT-4.1 Mini", "context": 1047576, "free": True},
            {"id": "gpt-4.1-nano", "name": "GPT-4.1 Nano", "context": 1047576, "free": True},
            {"id": "o4-mini", "name": "o4-mini", "context": 200000, "free": True},
            {"id": "o3-mini", "name": "o3-mini", "context": 200000, "free": True},
            {"id": "DeepSeek-R1", "name": "DeepSeek R1", "context": 131072, "free": True},
            {"id": "Llama-3.3-70B-Instruct", "name": "Llama 3.3 70B", "context": 131072, "free": True},
            {"id": "Mistral-Small-3.1", "name": "Mistral Small 3.1", "context": 131072, "free": True},
        ]}
    elif provider_id == "nvidia":
        return {"models": [
            {"id": "meta/llama-3.3-70b-instruct", "name": "Llama 3.3 70B", "context": 131072, "free": True},
            {"id": "meta/llama-3.1-8b-instruct", "name": "Llama 3.1 8B", "context": 131072, "free": True},
            {"id": "mistralai/mistral-large-2-instruct", "name": "Mistral Large 2", "context": 131072, "free": True},
            {"id": "qwen/qwen3-235b-a22b", "name": "Qwen3 235B", "context": 131072, "free": True},
            {"id": "google/gemma-3-27b-it", "name": "Gemma 3 27B", "context": 131072, "free": True},
        ]}
    else:
        prov = PROVIDERS[provider_id]
        return {"models": [
            {"id": prov["default_model"], "name": prov["default_model"], "context": 0, "free": True},
        ]}


async def _fetch_openrouter_models(free_only: bool = False):
    """Fetch models from OpenRouter's live API."""
    import httpx
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            resp = await client.get("https://openrouter.ai/api/v1/models")
            data = resp.json()

        models = []
        for m in data.get("data", []):
            pricing = m.get("pricing", {})
            is_free = pricing.get("prompt") == "0" and pricing.get("completion") == "0"

            if free_only and not is_free:
                continue

            models.append({
                "id": m["id"],
                "name": m.get("name", m["id"]),
                "context": m.get("context_length", 0),
                "free": is_free,
                "description": (m.get("description") or "")[:120],
                "pricing": {
                    "prompt": pricing.get("prompt", "0"),
                    "completion": pricing.get("completion", "0"),
                },
            })

        models.sort(key=lambda x: (0 if x["free"] else 1, x["name"]))
        return {"models": models, "total": len(models)}

    except Exception as e:
        return {"models": [], "error": str(e)}


# ── LLM Usage Stats ───────────────────────────────────────────

@router.get("/llm-usage")
def get_llm_usage(date: Optional[str] = None):
    """Get LLM usage stats per provider for today (or a specific date)."""
    from apps.api.services.leadgen.db import LeadDB
    db = LeadDB()
    usage = db.get_llm_usage(date)
    totals = db.get_llm_usage_total()
    db.close()

    # Map provider names to their daily limits (approximate)
    DAILY_LIMITS = {
        "cerebras": {"calls": 50, "label": "50 RPD"},
        "groq": {"calls": 1000, "label": "1K RPD"},
        "sambanova": {"calls": 1000, "label": "~1K RPD"},
        "nvidia": {"calls": 1000, "label": "1K RPD"},
        "mistral": {"calls": 14400, "label": "14.4K RPD"},
        "openrouter": {"calls": 50, "label": "50 RPD"},
        "github_models": {"calls": 1000, "label": "1K RPD"},
        "siliconflow": {"calls": 150, "label": "150 RPD"},
    }

    providers = []
    for u in usage:
        pid = u["provider"]
        fallback = DAILY_LIMITS.get(pid, {"calls": 0, "label": "Unknown"})

        # Prefer actual rate limit from provider API headers
        api_limit = u.get("rate_limit", 0) or 0
        api_remaining = u.get("rate_remaining", 0) or 0
        api_reset = u.get("rate_reset", "") or ""

        if api_limit > 0:
            # Use real data from provider headers
            daily_limit = api_limit
            remaining = api_remaining
            limit_label = f"{api_limit} RPD (live)"
        else:
            # Fall back to hardcoded estimates
            daily_limit = fallback["calls"]
            remaining = max(0, daily_limit - u["calls"])
            limit_label = fallback["label"]

        pct = min(100, round((u["calls"] / daily_limit) * 100)) if daily_limit > 0 else 0

        providers.append({
            "provider": pid,
            "model": u.get("model", ""),
            "calls": u["calls"],
            "tokens": u["total_tokens"],
            "daily_limit": daily_limit,
            "limit_label": limit_label,
            "remaining": remaining,
            "pct": pct,
            "rate_reset": api_reset,
            "live": api_limit > 0,
        })

    return {
        "providers": providers,
        "today_calls": sum(u["calls"] for u in usage),
        "today_tokens": sum(u["total_tokens"] for u in usage),
        "all_time_calls": totals.get("total_calls", 0) or 0,
        "all_time_tokens": totals.get("total_tokens", 0) or 0,
    }


# ── Enrichment Provider API ──────────────────────────────────────

@router.get("/enrichment-providers")
def list_enrichment_providers():
    """List all enrichment providers and their configuration status."""
    result = []
    for pid, prov in ENRICHMENT_PROVIDERS.items():
        api_key = _db_get(prov["env_key"], "")
        result.append({
            "id": pid,
            "name": prov["name"],
            "icon": prov["icon"],
            "capability": prov["capability"],
            "configured": bool(api_key),
            "api_key_masked": _mask_key(api_key),
            "free_tier": prov["free_tier"],
            "docs_url": prov["docs"],
        })
    return {"providers": result}


@router.put("/enrichment-providers/{provider_id}")
def update_enrichment_provider(provider_id: str, body: ProviderUpdate):
    """Update an enrichment provider's API key."""
    if provider_id not in ENRICHMENT_PROVIDERS:
        raise HTTPException(status_code=404, detail=f"Enrichment provider '{provider_id}' not found")
    prov = ENRICHMENT_PROVIDERS[provider_id]
    if body.api_key is not None:
        _db_set(prov["env_key"], body.api_key)
    return {"status": "ok", "provider": provider_id}


# ── Data Sources ─────────────────────────────────────────────────

DATA_SOURCES = [
    # ── Core Engines ──
    {"id": "duckduckgo", "name": "DuckDuckGo", "icon": "search", "description": "Web search for company websites", "default_enabled": True, "strategy": "web", "category": "core"},
    {"id": "google_maps", "name": "Google Maps", "icon": "map-pin", "description": "Local business listings with reviews & ratings", "default_enabled": True, "strategy": "maps", "category": "core"},
    {"id": "directories", "name": "Business Directories", "icon": "book-open", "description": "Clutch, GoodFirms aggregated directory scraping", "default_enabled": True, "strategy": "directories", "category": "core"},
    {"id": "linkedin", "name": "LinkedIn", "icon": "briefcase", "description": "Professional network company profiles", "default_enabled": True, "strategy": "linkedin", "category": "core"},
    {"id": "job_boards", "name": "Job Boards", "icon": "users", "description": "Companies actively hiring (Indeed, Naukri)", "default_enabled": True, "strategy": "job_boards", "category": "core"},

    # ── India — B2B Marketplaces ──
    {"id": "indiamart", "name": "IndiaMart", "icon": "shopping-bag", "description": "India's largest B2B marketplace — manufacturers, suppliers, exporters", "default_enabled": True, "strategy": "registry", "category": "india_b2b"},
    {"id": "justdial", "name": "JustDial", "icon": "phone", "description": "India's #1 local business directory with contacts", "default_enabled": True, "strategy": "registry", "category": "india_b2b"},
    {"id": "tradeindia", "name": "TradeIndia", "icon": "package", "description": "B2B marketplace for Indian manufacturers & exporters", "default_enabled": True, "strategy": "registry", "category": "india_b2b"},
    {"id": "sulekha", "name": "Sulekha", "icon": "store", "description": "Local services & business directory", "default_enabled": True, "strategy": "registry", "category": "india_b2b"},
    {"id": "exportersindia", "name": "ExportersIndia", "icon": "globe", "description": "Indian exporters & manufacturers directory", "default_enabled": True, "strategy": "registry", "category": "india_b2b"},

    # ── India — Review & Rating Sites ──
    {"id": "ambitionbox", "name": "AmbitionBox", "icon": "bar-chart-3", "description": "Company reviews, salaries, culture ratings", "default_enabled": True, "strategy": "review_sites", "category": "india_review"},
    {"id": "glassdoor_in", "name": "Glassdoor India", "icon": "star", "description": "Employee reviews & company ratings India", "default_enabled": True, "strategy": "registry", "category": "india_review"},

    # ── India — Job Boards ──
    {"id": "naukri", "name": "Naukri", "icon": "user-check", "description": "Hiring companies on India's top job portal", "default_enabled": True, "strategy": "registry", "category": "india_jobs"},

    # ── India — Government & Registration ──
    {"id": "zaubacorp", "name": "Zauba Corp (MCA)", "icon": "landmark", "description": "Ministry of Corporate Affairs — registered companies", "default_enabled": True, "strategy": "registry", "category": "india_gov"},
    {"id": "tofler", "name": "Tofler", "icon": "file-text", "description": "Company financials, directors, registration data", "default_enabled": True, "strategy": "registry", "category": "india_gov"},

    # ── India — Startup Ecosystem ──
    {"id": "yourstory", "name": "YourStory", "icon": "newspaper", "description": "Indian startup news & company profiles", "default_enabled": True, "strategy": "registry", "category": "india_startup"},
    {"id": "inc42", "name": "Inc42", "icon": "trending-up", "description": "Indian startup funding & news tracker", "default_enabled": True, "strategy": "registry", "category": "india_startup"},

    # ── Global — IT/Tech Directories ──
    {"id": "clutch", "name": "Clutch", "icon": "award", "description": "B2B reviews & ratings for IT/digital agencies", "default_enabled": True, "strategy": "registry", "category": "global_tech"},
    {"id": "goodfirms", "name": "GoodFirms", "icon": "check-circle", "description": "IT company reviews & research platform", "default_enabled": True, "strategy": "registry", "category": "global_tech"},
    {"id": "g2", "name": "G2", "icon": "grid", "description": "Software & service company reviews — buyer intent", "default_enabled": True, "strategy": "registry", "category": "global_tech"},
    {"id": "softwaresuggest", "name": "SoftwareSuggest", "icon": "monitor", "description": "Software companies & product comparison", "default_enabled": True, "strategy": "registry", "category": "global_tech"},
    {"id": "techbehemoths", "name": "TechBehemoths", "icon": "cpu", "description": "IT agencies & digital companies worldwide", "default_enabled": True, "strategy": "registry", "category": "global_tech"},

    # ── Global — Business Directories ──
    {"id": "yellowpages", "name": "Yellow Pages", "icon": "book", "description": "US/global local business directory", "default_enabled": True, "strategy": "registry", "category": "global_directory"},
    {"id": "yelp", "name": "Yelp", "icon": "message-circle", "description": "Local businesses with customer reviews", "default_enabled": True, "strategy": "registry", "category": "global_directory"},
    {"id": "bbb", "name": "Better Business Bureau", "icon": "shield", "description": "Accredited US businesses with trust ratings", "default_enabled": False, "strategy": "registry", "category": "global_directory"},
    {"id": "thomasnet", "name": "ThomasNet", "icon": "factory", "description": "US industrial suppliers & manufacturers", "default_enabled": False, "strategy": "registry", "category": "global_directory"},

    # ── Global — Startup & Funding ──
    {"id": "crunchbase", "name": "Crunchbase", "icon": "rocket", "description": "Startup funding rounds & company data", "default_enabled": True, "strategy": "registry", "category": "global_startup"},
    {"id": "tracxn", "name": "Tracxn", "icon": "activity", "description": "Startup tracking & funding intelligence", "default_enabled": True, "strategy": "registry", "category": "global_startup"},
    {"id": "angellist", "name": "AngelList / Wellfound", "icon": "zap", "description": "Startup jobs & company profiles", "default_enabled": True, "strategy": "registry", "category": "global_startup"},

    # ── Global — Social & Professional ──
    {"id": "linkedin_companies", "name": "LinkedIn Companies", "icon": "linkedin", "description": "Company pages on LinkedIn via DDG", "default_enabled": True, "strategy": "registry", "category": "social"},
    {"id": "facebook_pages", "name": "Facebook Business", "icon": "facebook", "description": "Business pages on Facebook", "default_enabled": True, "strategy": "registry", "category": "social"},

    # ── Global — Job Boards ──
    {"id": "indeed", "name": "Indeed", "icon": "briefcase", "description": "Companies hiring globally — intent signal", "default_enabled": True, "strategy": "registry", "category": "global_jobs"},
    {"id": "glassdoor", "name": "Glassdoor", "icon": "star", "description": "Company reviews & employer ratings", "default_enabled": True, "strategy": "registry", "category": "global_jobs"},

    # ── Europe ──
    {"id": "europages", "name": "Europages", "icon": "globe-2", "description": "European B2B supplier directory", "default_enabled": False, "strategy": "registry", "category": "europe"},
    {"id": "kompass", "name": "Kompass", "icon": "compass", "description": "Global B2B company directory", "default_enabled": False, "strategy": "registry", "category": "europe"},

    # ── Generic Search Patterns ──
    {"id": "generic_companies_list", "name": "Company Lists", "icon": "list", "description": "Generic web search for company lists & directories", "default_enabled": True, "strategy": "registry", "category": "generic"},
    {"id": "generic_association", "name": "Industry Associations", "icon": "users-2", "description": "Chamber of commerce & association member directories", "default_enabled": True, "strategy": "registry", "category": "generic"},
    {"id": "generic_awards", "name": "Award Winners", "icon": "trophy", "description": "Best/fastest-growing company award lists", "default_enabled": True, "strategy": "registry", "category": "generic"},

    # ── Product/SaaS Directories ──
    {"id": "capterra", "name": "Capterra", "icon": "monitor", "description": "Software reviews & comparison platform", "default_enabled": True, "strategy": "registry", "category": "saas_directory"},
    {"id": "getapp", "name": "GetApp", "icon": "grid", "description": "SaaS product discovery & reviews", "default_enabled": True, "strategy": "registry", "category": "saas_directory"},
    {"id": "producthunt", "name": "Product Hunt", "icon": "rocket", "description": "New product launches & startup discovery", "default_enabled": True, "strategy": "registry", "category": "saas_directory"},
    {"id": "sourceforge", "name": "SourceForge", "icon": "code", "description": "Open source & commercial software directory", "default_enabled": True, "strategy": "registry", "category": "saas_directory"},
    {"id": "alternativeto", "name": "AlternativeTo", "icon": "layers", "description": "Software alternative recommendations", "default_enabled": True, "strategy": "registry", "category": "saas_directory"},
    {"id": "saashub", "name": "SaaSHub", "icon": "layout", "description": "SaaS product directory & alternatives", "default_enabled": True, "strategy": "registry", "category": "saas_directory"},
    {"id": "stackshare", "name": "StackShare", "icon": "cpu", "description": "Tech stack discovery — who uses what", "default_enabled": True, "strategy": "registry", "category": "saas_directory"},
    {"id": "appsumo", "name": "AppSumo", "icon": "tag", "description": "SaaS deals & product marketplace", "default_enabled": True, "strategy": "registry", "category": "saas_directory"},

    # ── Trust / Review ──
    {"id": "trustpilot", "name": "Trustpilot", "icon": "shield", "description": "Business trust reviews & ratings", "default_enabled": True, "strategy": "registry", "category": "trust_review"},
    {"id": "tripadvisor", "name": "TripAdvisor", "icon": "map", "description": "Hospitality & tourism business reviews", "default_enabled": False, "strategy": "registry", "category": "trust_review"},
    {"id": "google_reviews", "name": "Google Reviews", "icon": "star", "description": "Google Business Profile reviews", "default_enabled": True, "strategy": "registry", "category": "trust_review"},
    {"id": "mouthshut", "name": "MouthShut", "icon": "message-circle", "description": "Indian consumer & business reviews", "default_enabled": True, "strategy": "registry", "category": "trust_review"},

    # ── Freelance Marketplaces ──
    {"id": "upwork", "name": "Upwork", "icon": "briefcase", "description": "Freelance agencies & service providers", "default_enabled": True, "strategy": "registry", "category": "freelance"},
    {"id": "fiverr", "name": "Fiverr Business", "icon": "zap", "description": "Service provider marketplace", "default_enabled": False, "strategy": "registry", "category": "freelance"},
    {"id": "toptal", "name": "Toptal", "icon": "award", "description": "Top 3% freelancers & agencies", "default_enabled": False, "strategy": "registry", "category": "freelance"},
    {"id": "freelancer", "name": "Freelancer", "icon": "user", "description": "Freelance & agency marketplace", "default_enabled": False, "strategy": "registry", "category": "freelance"},
    {"id": "bark", "name": "Bark", "icon": "phone", "description": "Local service professionals directory", "default_enabled": True, "strategy": "registry", "category": "freelance"},

    # ── Developer Communities ──
    {"id": "github_orgs", "name": "GitHub Organizations", "icon": "code", "description": "Tech companies on GitHub", "default_enabled": True, "strategy": "registry", "category": "developer"},
    {"id": "stackoverflow_jobs", "name": "StackOverflow Companies", "icon": "layers", "description": "Companies hiring developers", "default_enabled": True, "strategy": "registry", "category": "developer"},
    {"id": "hackernews", "name": "Hacker News", "icon": "terminal", "description": "Show HN launches & tech company mentions", "default_enabled": True, "strategy": "registry", "category": "developer"},
    {"id": "devto", "name": "Dev.to", "icon": "hash", "description": "Developer community company mentions", "default_enabled": False, "strategy": "registry", "category": "developer"},

    # ── News & Media ──
    {"id": "google_news", "name": "Google News", "icon": "newspaper", "description": "Latest company news & press coverage", "default_enabled": True, "strategy": "registry", "category": "news"},
    {"id": "economic_times", "name": "Economic Times", "icon": "trending-up", "description": "Indian business news & company mentions", "default_enabled": True, "strategy": "registry", "category": "news"},
    {"id": "business_standard", "name": "Business Standard", "icon": "file-text", "description": "Indian financial & business news", "default_enabled": True, "strategy": "registry", "category": "news"},
    {"id": "livemint", "name": "Livemint", "icon": "newspaper", "description": "Indian business & startup news", "default_enabled": True, "strategy": "registry", "category": "news"},
    {"id": "techcrunch", "name": "TechCrunch", "icon": "zap", "description": "Tech startup funding & news", "default_enabled": True, "strategy": "registry", "category": "news"},
    {"id": "forbes", "name": "Forbes", "icon": "award", "description": "Forbes company lists & rankings", "default_enabled": True, "strategy": "registry", "category": "news"},

    # ── India — More Directories ──
    {"id": "yellowpages_in", "name": "India Yellow Pages", "icon": "book", "description": "Indian local business directory", "default_enabled": True, "strategy": "registry", "category": "india_directory"},
    {"id": "grotal", "name": "Grotal", "icon": "search", "description": "Indian business listing & directory", "default_enabled": True, "strategy": "registry", "category": "india_directory"},
    {"id": "urbanpro", "name": "UrbanPro", "icon": "user-check", "description": "Indian professionals & training providers", "default_enabled": True, "strategy": "registry", "category": "india_directory"},
    {"id": "dial4trade", "name": "Dial4Trade", "icon": "phone", "description": "Indian B2B supplier directory", "default_enabled": True, "strategy": "registry", "category": "india_directory"},
    {"id": "fundoodata", "name": "FundooData", "icon": "database", "description": "Indian company database & contacts", "default_enabled": True, "strategy": "registry", "category": "india_directory"},
    {"id": "startup_india", "name": "Startup India", "icon": "rocket", "description": "Government startup registry portal", "default_enabled": True, "strategy": "registry", "category": "india_startup"},
    {"id": "nasscom", "name": "NASSCOM", "icon": "landmark", "description": "IT industry body member directory", "default_enabled": True, "strategy": "registry", "category": "india_directory"},
    {"id": "shine", "name": "Shine Jobs", "icon": "briefcase", "description": "Indian job portal — hiring companies", "default_enabled": True, "strategy": "registry", "category": "india_jobs"},
    {"id": "monsterindia", "name": "Monster India", "icon": "briefcase", "description": "Indian recruitment & hiring portal", "default_enabled": True, "strategy": "registry", "category": "india_jobs"},

    # ── Global — More Directories ──
    {"id": "manta", "name": "Manta", "icon": "book", "description": "US small business directory", "default_enabled": True, "strategy": "registry", "category": "global_directory"},
    {"id": "dnb", "name": "D&B", "icon": "database", "description": "Dun & Bradstreet company profiles", "default_enabled": False, "strategy": "registry", "category": "global_directory"},
    {"id": "opencorporates", "name": "OpenCorporates", "icon": "globe", "description": "Open corporate registry data worldwide", "default_enabled": True, "strategy": "registry", "category": "europe"},
    {"id": "companieshouse", "name": "Companies House UK", "icon": "landmark", "description": "UK company registration records", "default_enabled": False, "strategy": "registry", "category": "europe"},
    {"id": "dealroom", "name": "Dealroom", "icon": "trending-up", "description": "European startup & VC data platform", "default_enabled": True, "strategy": "registry", "category": "global_startup"},
    {"id": "cbinsights", "name": "CB Insights", "icon": "bar-chart-3", "description": "Market intelligence & company research", "default_enabled": True, "strategy": "registry", "category": "global_startup"},
    {"id": "foursquare", "name": "Foursquare", "icon": "map-pin", "description": "Location-based business discovery", "default_enabled": False, "strategy": "registry", "category": "global_directory"},
    {"id": "hotfrog", "name": "Hotfrog", "icon": "globe", "description": "Global business listing directory", "default_enabled": True, "strategy": "registry", "category": "global_directory"},
    {"id": "brownbook", "name": "BrownBook", "icon": "book-open", "description": "Worldwide business directory", "default_enabled": True, "strategy": "registry", "category": "global_directory"},
    {"id": "cylex", "name": "Cylex", "icon": "search", "description": "Business directory — US & Europe", "default_enabled": True, "strategy": "registry", "category": "global_directory"},

    # ── Social & Community Signals ──
    {"id": "twitter_companies", "name": "Twitter/X Companies", "icon": "hash", "description": "Company accounts on Twitter/X", "default_enabled": True, "strategy": "registry", "category": "social"},
    {"id": "instagram_business", "name": "Instagram Business", "icon": "camera", "description": "Business accounts on Instagram", "default_enabled": True, "strategy": "registry", "category": "social"},
    {"id": "reddit_companies", "name": "Reddit Mentions", "icon": "message-circle", "description": "Company recommendations on Reddit", "default_enabled": True, "strategy": "registry", "category": "social"},
    {"id": "quora_companies", "name": "Quora Mentions", "icon": "help-circle", "description": "Company mentions on Quora", "default_enabled": True, "strategy": "registry", "category": "social"},

    # ── E-Commerce Sellers ──
    {"id": "amazon_sellers", "name": "Amazon Sellers", "icon": "shopping-bag", "description": "Amazon marketplace sellers & brands", "default_enabled": True, "strategy": "registry", "category": "ecommerce"},
    {"id": "flipkart_sellers", "name": "Flipkart Sellers", "icon": "shopping-bag", "description": "Flipkart marketplace sellers", "default_enabled": True, "strategy": "registry", "category": "ecommerce"},
    {"id": "shopify_stores", "name": "Shopify Stores", "icon": "store", "description": "Shopify-powered online stores", "default_enabled": True, "strategy": "registry", "category": "ecommerce"},

    # ── Generic Patterns (cont.) ──
    {"id": "generic_expo", "name": "Trade Shows & Expos", "icon": "calendar", "description": "Exhibition & trade show exhibitor lists", "default_enabled": True, "strategy": "registry", "category": "generic"},
    {"id": "generic_incubator", "name": "Incubator/Accelerator", "icon": "rocket", "description": "Startup accelerator portfolio companies", "default_enabled": True, "strategy": "registry", "category": "generic"},
    {"id": "generic_govt_tender", "name": "Government Tenders", "icon": "landmark", "description": "Govt tender vendor & supplier lists", "default_enabled": True, "strategy": "registry", "category": "generic"},
    {"id": "generic_iso_certified", "name": "ISO Certified", "icon": "check-circle", "description": "ISO certified companies by industry", "default_enabled": True, "strategy": "registry", "category": "generic"},
    {"id": "generic_hiring_surge", "name": "Hiring Surge", "icon": "trending-up", "description": "Companies actively posting \"we are hiring\"", "default_enabled": True, "strategy": "registry", "category": "generic"},
]


def get_source_enabled(source_id: str) -> bool:
    """Check if a data source is enabled."""
    src = next((s for s in DATA_SOURCES if s["id"] == source_id), None)
    default = src["default_enabled"] if src else False
    val = _db_get(f"SOURCE_{source_id.upper()}_ENABLED", "")
    if val == "":
        return default
    return val == "1"


@router.get("/sources")
def list_sources():
    """Get all data sources with their enabled/disabled status."""
    result = []
    for src in DATA_SOURCES:
        enabled = get_source_enabled(src["id"])
        result.append({
            "id": src["id"],
            "name": src["name"],
            "icon": src["icon"],
            "description": src["description"],
            "enabled": enabled,
            "strategy": src["strategy"],
            "category": src.get("category", "other"),
        })
    return result


@router.put("/sources/{source_id}")
def toggle_source(source_id: str, enabled: bool = True):
    """Enable or disable a data source."""
    src = next((s for s in DATA_SOURCES if s["id"] == source_id), None)
    if not src:
        from fastapi import HTTPException
        raise HTTPException(status_code=404, detail=f"Source '{source_id}' not found")
    _db_set(f"SOURCE_{source_id.upper()}_ENABLED", "1" if enabled else "0")
    return {"id": source_id, "enabled": enabled}


@router.get("/sources/registry")
def list_registry_sources():
    """Get the full source registry with all 30+ directories/marketplaces."""
    try:
        from apps.api.services.leadgen.source_registry import get_source_summary
        return get_source_summary()
    except Exception as e:
        return {"total": 0, "error": str(e)}


# ── Integration Destinations (CRM / export connectors) ────────────────────
# BYOK credentials for the workbook output-column destinations. Data-driven so
# the Settings UI renders one card per integration with the right fields.

INTEGRATIONS = [
    {
        "id": "hubspot", "name": "HubSpot CRM", "icon": "hubspot",
        "description": "Push leads as HubSpot contacts (output column → CRM).",
        "fields": [
            {"key": "HUBSPOT_TOKEN", "label": "Private App Token", "secret": True, "placeholder": "pat-na1-..."},
        ],
    },
    {
        "id": "salesforce", "name": "Salesforce", "icon": "salesforce",
        "description": "Push leads as Salesforce Lead records (upsert by email).",
        "fields": [
            {"key": "SALESFORCE_INSTANCE_URL", "label": "Instance URL", "secret": False, "placeholder": "https://your.my.salesforce.com"},
            {"key": "SALESFORCE_ACCESS_TOKEN", "label": "Access Token", "secret": True, "placeholder": "00D..."},
        ],
    },
    {
        "id": "airtable", "name": "Airtable", "icon": "airtable",
        "description": "Append rows to an Airtable base (output column → Airtable).",
        "fields": [
            {"key": "AIRTABLE_TOKEN", "label": "Personal Access Token", "secret": True, "placeholder": "pat..."},
        ],
    },
    {
        "id": "sheets", "name": "Google Sheets", "icon": "sheets",
        "description": "Append rows to a Google Sheet via an OAuth2 access token.",
        "fields": [
            {"key": "GOOGLE_SHEETS_TOKEN", "label": "OAuth2 Access Token", "secret": True, "placeholder": "ya29..."},
        ],
    },
]


class IntegrationUpdate(BaseModel):
    values: Dict[str, str]


def _integration_view(it: dict) -> dict:
    """Serialize an integration with connection status, never echoing secrets."""
    fields = []
    connected = True
    for f in it["fields"]:
        val = _db_get(f["key"], "")
        is_set = bool(val)
        connected = connected and is_set
        fields.append({
            "key": f["key"], "label": f["label"], "secret": f["secret"],
            "placeholder": f.get("placeholder", ""),
            "set": is_set,
            # Non-secret values (instance URL) are echoed; secrets never are.
            "value": "" if f["secret"] else val,
            "masked": (f"…{val[-4:]}" if (f["secret"] and is_set and len(val) >= 4) else ("set" if is_set else "")),
        })
    return {
        "id": it["id"], "name": it["name"], "icon": it["icon"],
        "description": it["description"], "connected": connected, "fields": fields,
    }


@router.get("/integrations")
def list_integrations(user=Depends(get_current_active_user)):
    """List destination integrations with connection status (secrets masked)."""
    return {"integrations": [_integration_view(it) for it in INTEGRATIONS]}


@router.put("/integrations/{integration_id}")
def update_integration(integration_id: str, body: IntegrationUpdate,
                       user=Depends(get_current_active_user)):
    """Save credentials for one integration. Blank values are ignored so a
    saved secret is never clobbered by an empty field."""
    it = next((x for x in INTEGRATIONS if x["id"] == integration_id), None)
    if not it:
        raise HTTPException(status_code=404, detail="Integration not found")
    allowed = {f["key"] for f in it["fields"]}
    saved = []
    for key, value in (body.values or {}).items():
        if key in allowed and value and value.strip():
            _db_set(key, value.strip())
            saved.append(key)
    return {"status": "ok", "saved": saved, "integration": _integration_view(it)}
