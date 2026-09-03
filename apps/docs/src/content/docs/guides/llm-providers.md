---
title: LLM providers
description: The provider chain behind AI, research, agent and chat features, and how the default is chosen.
sidebar:
  order: 3
---

AI columns, research columns, agent columns and chat all go through one LLM
client with automatic failover. You need at least one key; several providers
have free tiers.

## Supported providers

| id | Key | Default model | Base URL override |
|---|---|---|---|
| `anthropic` | `ANTHROPIC_API_KEY` | `claude-opus-4-8` | `ANTHROPIC_BASE_URL` |
| `cerebras` | `CEREBRAS_API_KEY` | `llama-3.3-70b` | `CEREBRAS_BASE_URL` |
| `groq` | `GROQ_API_KEY` | `llama-3.3-70b-versatile` | `GROQ_BASE_URL` |
| `sambanova` | `SAMBANOVA_API_KEY` | `Meta-Llama-3.3-70B-Instruct` | `SAMBANOVA_BASE_URL` |
| `nvidia` | `NVIDIA_API_KEY` | `meta/llama-3.3-70b-instruct` | `NVIDIA_BASE_URL` |
| `mistral` | `MISTRAL_API_KEY` | `mistral-small-latest` | `MISTRAL_BASE_URL` |
| `openrouter` | `OPENROUTER_API_KEY` | `meta-llama/llama-3.3-70b-instruct:free` | `OPENROUTER_BASE_URL` |
| `github_models` | `GITHUB_MODELS_API_KEY` | `gpt-4o` | `GITHUB_MODELS_BASE_URL` |
| `siliconflow` | `SILICONFLOW_API_KEY` | `Qwen/Qwen3-8B` | `SILICONFLOW_BASE_URL` |

Each has a `<PROVIDER>_MODEL` variable to change the model. The Settings UI
additionally lists Google AI (`GOOGLE_AI_API_KEY`, `gemini-2.5-flash`),
Cohere, Cloudflare Workers AI and Hugging Face with their free-tier notes.
Anthropic uses the native Messages API; the others are OpenAI-compatible.

## Which provider runs

1. `LLM_DEFAULT_PROVIDER`, when set, always wins.
2. Otherwise, if an Anthropic key is present **and** `YUPCHA_CLOUD` is truthy,
   Claude becomes the default. Self-hosted installs stay opt-in.
3. Otherwise `cerebras`.

Failover order after the default is `cerebras, groq, sambanova, nvidia,
mistral, openrouter, github_models, siliconflow, anthropic`. Anthropic sits
last so the free tier is never displaced as a fallback. A call tries up to
three providers, with exponential backoff and jitter on rate limits.

Keys saved in the Settings UI override environment variables.

## Tuning

| Setting | Default | Meaning |
|---|---|---|
| `LLM_PROMPT_CACHE` | on | Cache identical prompts |
| `LLM_HTTP_TIMEOUT` | 60 | Seconds per request |
| `CHAT_MAX_TOOL_ROUNDS` | 8 | ReAct tool rounds per chat turn |
| `RESEARCH_NATIVE_TOOLS` | off | Use Claude native tool-use for research columns when Anthropic is the default |
| `RESEARCH_CELL_BUDGET_USD` | 0.05 | Per-cell budget for research columns |

## Prompt injection

Fetched web content is fenced as untrusted data before it reaches a model, and
research columns use in-house `search`/`fetch` tools rather than vendor
server-side browsing so every fetch passes the SSRF guard and the fence. See
[Security model](/self-hosting/security/).
