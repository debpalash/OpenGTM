"""
Settings API — Manage LLM providers and system configuration.
Persists settings to SQLite database. Seeds from .env on first run.
"""

import os
import sqlite3
from typing import Optional
from pathlib import Path
from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

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
        "default_url": "https://generativelanguage.googleapis.com/v1beta",
        "default_model": "gemini-2.5-flash",
        "docs": "https://aistudio.google.com/app/apikey",
        "free_tier": "5-15 RPM, 100-1K RPD",
        "icon": "🔷",
        "openai_compatible": False,
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
