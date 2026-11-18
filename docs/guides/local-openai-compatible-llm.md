# Local OpenAI-compatible LLM server (vLLM) — provider guide

OpenGTM ships a generic **Custom OpenAI Compatible** provider (`custom_openai`)
for any self-hosted server that speaks the OpenAI chat-completions API:
[vLLM](https://docs.vllm.ai/en/latest/serving/openai_compatible_server.html),
llama.cpp's server, Ollama's OpenAI layer, LM Studio, TGI, and similar.

It is a first-class provider in the Go backend (`internal/llm`), the Settings
registry and the Settings UI — chat completions, streaming (the copilot) and
tool/function calling all go through the same client as the built-in providers.

## Required configuration

Three settings (env / `.env`, or the Settings UI — both write the same store):

| Setting | Env key | Notes |
|---|---|---|
| Base URL | `CUSTOM_OPENAI_BASE_URL` | The server root, usually ending in `/v1`. **Required.** |
| API key | `CUSTOM_OPENAI_API_KEY` | **Optional** — leave empty when the server serves without auth. Stored encrypted (`secret_app_keys`), masked in every API response and never logged. |
| Model ID | `CUSTOM_OPENAI_MODEL` | The exact id the server reports — **required**. |
| Timeout | `CUSTOM_OPENAI_HTTP_TIMEOUT` | Optional seconds per request; default is the global `LLM_HTTP_TIMEOUT` (60). |
| Default | `LLM_DEFAULT_PROVIDER=custom_openai` | The provider only serves traffic as the explicit default; it never joins the free-provider fallback chain. |

`.env.example` carries a commented block for all of these. Changing any value
in the Settings UI takes effect immediately — no rebuild, no restart.

Getting the model ID from vLLM — it returns the **served model path**, not a
short name:

```bash
curl http://<host>:<port>/v1/models
# "data": [ { "id": "/root/.cache/huggingface/hub/models--Qwen--..." } ]
```

Copy that `id` string verbatim into `CUSTOM_OPENAI_MODEL`.
(Start vLLM with `--served-model-name qwen` for a friendlier id.)

## Settings UI

1. Open **Settings → AI providers**.
2. Find **Custom OpenAI Compatible** (🖥️).
3. Enter the **Base URL** (e.g. `http://192.168.1.10:8008/v1`) and the **Model
   ID**. The API key field is optional — leave it blank for an unauthenticated
   server; when set, requests carry `Authorization: Bearer <key>`.
4. **Save**, then press **Test** (a short chat completion round-trip).
5. Press **Set default** to route all OpenGTM AI traffic to it.

Workspace vs global: values resolve workspace secret → global setting →
environment, like every other provider. A base URL set at one level is never
combined with a key from another level (the provider refuses to run rather
than send the key somewhere else).

## Docker networking

The `api-go` and `worker-go` containers call the model server from inside the
compose network. Two things matter:

- **Never use `localhost`** in the base URL — inside a container it means the
  container itself. Use the server's LAN address (e.g. `192.168.1.10`) or a
  DNS name.
- The backend's SSRF egress guard blocks private addresses by default. For
  this provider the **host of `CUSTOM_OPENAI_BASE_URL` is allow-listed
  automatically while the provider is configured** (re-read on every request,
  so UI changes work without a restart). The dynamic entry is still vetted:
  cloud-metadata endpoints and link-local addresses are refused. You can also
  pin it statically in `.env`: `EGRESS_ALLOW_HOSTS=<host>[:port]`.

The model server itself is not exposed by this compose file — traffic only
leaves the containers toward your LAN address. Keep vLLM bound to your LAN
interface (`--host 0.0.0.0` on a trusted network, or an SSH tunnel), never to
the public internet.

## Example vLLM configuration

```bash
# on the model host
python -m vllm.entrypoints.openai.api_server \
  --model Qwen/Qwen3.6-35B-A3B-FP8 \
  --host 0.0.0.0 --port 8008 \
  --served-model-name qwen3.6 \
  --max-model-len 262144
# optional auth:  --api-key sk-...   (then set the same value in OpenGTM)
```

In OpenGTM `.env` (or the Settings UI):

```dotenv
CUSTOM_OPENAI_BASE_URL=http://10.10.9.45:8008/v1
CUSTOM_OPENAI_MODEL=/root/.cache/huggingface/hub/models--Qwen--Qwen3.6-35B-A3B-FP8/snapshots/<sha>
# CUSTOM_OPENAI_API_KEY=
LLM_DEFAULT_PROVIDER=custom_openai
```

## Testing commands

```bash
# 1. server reachable from the host
curl http://10.10.9.45:8008/v1/models

# 2. reachable from inside the app containers (no curl in distroless images —
#    use a throwaway container on the same network):
docker run --rm --network opengtm_lead_net curlimages/curl:latest \
  http://10.10.9.45:8008/v1/models

# 3. chat completion round-trip
curl http://10.10.9.45:8008/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","messages":[{"role":"user","content":"Reply with exactly: OPENGTM_LOCAL_LLM_WORKING"}],"max_tokens":16}'

# 4. OpenGTM provider test (Settings UI → Test, or the API):
curl -X POST http://localhost:8000/api/settings/providers/custom_openai/test \
  -H "Authorization: Bearer <session-token>"

# 5. automated tests
cd apps/backend-go && go test ./internal/llm/... ./internal/egress/... ./internal/settings/...
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `No LLM providers configured` | custom_openai needs **both** a base URL and a model ID; and it must be the default provider (`LLM_DEFAULT_PROVIDER=custom_openai` or Settings → Set default). |
| `request timed out` | The server is slow or unreachable from the container. Check `docker run --network opengtm_lead_net curlimages/curl …`; raise `CUSTOM_OPENAI_HTTP_TIMEOUT`. |
| `connection refused` / `no route to host` | Wrong address (never `localhost`), the server binds only to 127.0.0.1, or a host firewall blocks the LAN. |
| dial refused although the server is up | The host is not allow-listed. It is added automatically from the base URL while the provider is configured; set `EGRESS_ALLOW_HOSTS=<host>[:port]` explicitly if the base URL lives only at one settings level. localhost/loopback base URLs must be pinned in `EGRESS_ALLOW_HOSTS`. |
| `HTTP 401` | The server runs with `--api-key`; set `CUSTOM_OPENAI_API_KEY` to match. |
| `HTTP 404 ... model does not exist` | The model ID does not match `/v1/models` exactly (vLLM returns the full cache path). |
| `provider error` on the copilot but Settings → Test works | Check `docker compose logs api-go worker-go` for the provider-tagged line; streaming and tools are exercised by the copilot only. |
| Repeated empty answers / `empty LLM response`, model seems to "think" forever | The served model is a reasoning model and spends the whole `max_tokens` budget on its thinking block, leaving `content` empty. Set `CUSTOM_OPENAI_DISABLE_THINKING=true` — every request then carries `chat_template_kwargs: {"enable_thinking": false}` (honoured by vLLM chat templates; ignored safely elsewhere). |
| Agent/tool columns fail while plain chat works | The served model must support OpenAI tool calling (vLLM: `--enable-auto-tool-choice --tool-call-parser <parser>`). Without it chat still works; agent features degrade. |

## Thinking-mode reliability

`custom_openai` auto-recovers from reasoning models that exhaust the token
budget on thinking: a completion whose `content` is empty (or a streamed round
with no content/tool-call deltas) is retried once with
`chat_template_kwargs.enable_thinking=false`. Set `CUSTOM_OPENAI_DISABLE_THINKING=true`
to make thinking-off the default for every request (skips the retry round-trip
entirely). Hosted providers are never touched.

## Discovery market defaults

The collection pipeline builds some search queries from a primary-market
default when the ICP names no city/country. Set `DEFAULT_REGION=nl` (also
`de`, `uk`, `fr`) for European deployments; unset keeps the legacy India
default. When the ICP explicitly names a market, a `geo_relevance` stage
drops leads with foreign-market evidence (foreign ccTLD, foreign HQ city).

## Automations and watches

Watches/automations are behind `AUTOMATIONS_ENABLED` (default off — the
deployment ships with external side effects closed). Enabling it makes the
CRUD/watch APIs and the scheduler's poller live, but **no outreach is sent**
unless campaign send settings and a connected mailbox/webhook exist; outbound
webhook destinations additionally obey `AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST`.
Set it in `.env`, then `docker compose up -d api-go worker-go scheduler-go`.
