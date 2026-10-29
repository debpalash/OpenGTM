"""Records leadgen/llm.py + copilot/llm_stream.py behaviour against a fake HTTP server.

Run from legacy/python:  .venv/bin/python <this file> <outdir>
Never touches the network beyond 127.0.0.1 and never a real provider.
"""
import asyncio, json, os, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.getcwd())
import apps.api.services.leadgen.llm as L
import apps.api.routers.copilotkit as CK
from apps.api.services.copilot import llm_stream as LS
import httpx

OUT = sys.argv[1]
KEEP_HDRS = ["authorization", "content-type", "x-api-key", "anthropic-version", "accept"]

# ── fake server ──────────────────────────────────────────────────────
STATE = {"script": {}, "log": [], "counts": {}}


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _do(self):
        n = int(self.headers.get("content-length") or 0)
        raw = self.rfile.read(n) if n else b""
        try:
            body = json.loads(raw) if raw else None
        except Exception:
            body = {"_raw": raw.decode("utf8", "replace")}
        path = self.path
        hdrs = {k: v for k, v in ((k.lower(), v) for k, v in self.headers.items()) if k in KEEP_HDRS}
        STATE["log"].append({"method": self.command, "path": path, "headers": hdrs, "body": body})
        i = STATE["counts"].get(path, 0)
        STATE["counts"][path] = i + 1
        seq = STATE["script"].get(path) or [{"status": 404, "json": {"error": "no script"}}]
        r = seq[min(i, len(seq) - 1)]
        if r.get("delay"):
            time.sleep(r["delay"])
        status = r.get("status", 200)
        if "sse" in r:
            data = "".join(l + "\n" for l in r["sse"]).encode()
            ct = "text/event-stream"
        elif "text" in r:
            data = r["text"].encode(); ct = r.get("ct", "text/html")
        else:
            data = json.dumps(r.get("json", {})).encode(); ct = "application/json"
        self.send_response(status)
        self.send_header("content-type", ct)
        for k, v in (r.get("headers") or {}).items():
            self.send_header(k, v)
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    do_POST = do_GET = _do


srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
PORT = srv.server_address[1]
threading.Thread(target=srv.serve_forever, daemon=True).start()
BASE = f"http://127.0.0.1:{PORT}"
DEAD = "http://127.0.0.1:1"


def sub(o):
    """Replace the live base URL with the {{BASE}} placeholder."""
    return json.loads(json.dumps(o).replace(BASE, "{{BASE}}"))


def unsub(o):
    return json.loads(json.dumps(o).replace("{{BASE}}", BASE))


def reset(script):
    STATE["script"] = unsub(script)
    STATE["log"] = []
    STATE["counts"] = {}


# ── settings / usage fakes ───────────────────────────────────────────
class Store:
    def __init__(self):
        self.rows = []

    def record_llm_usage(self, provider, model, p, c, rate_limit=0, rate_remaining=0, rate_reset=""):
        self.rows.append({"provider": provider, "model": model, "prompt_tokens": p, "completion_tokens": c,
                          "rate_limit": rate_limit, "rate_remaining": rate_remaining, "rate_reset": rate_reset})


def patch_settings(settings):
    s = unsub(settings)

    def get(key, default=""):
        v = s.get(key)
        return v if v else default
    L._read_setting = get
    CK._db_get = get


SLEEPS = []
_real_sleep = asyncio.sleep


async def fake_sleep(d, *a, **k):
    SLEEPS.append(round(d, 4))
    await _real_sleep(0.001)

asyncio.sleep = fake_sleep

VECTORS = {"complete": [], "parse_json": [], "extract_json": [], "default_provider": [], "stream": [],
           "chat_chain": [], "cost": [], "anthropic_call": [], "batch": []}


def ok_openai(text="hello", p=11, c=7, headers=None, **extra):
    r = {"status": 200, "json": {"choices": [{"message": {"content": text}}], "usage": {"prompt_tokens": p, "completion_tokens": c}}}
    if headers:
        r["headers"] = headers
    r["json"].update(extra)
    return r


def ant_ok(text="hi", inp=12, out=5, cw=0, cr=0, content=None):
    return {"status": 200, "json": {
        "id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-4-8",
        "content": content or [{"type": "text", "text": text}],
        "stop_reason": "end_turn", "stop_sequence": None,
        "usage": {"input_tokens": inp, "output_tokens": out, "cache_creation_input_tokens": cw, "cache_read_input_tokens": cr}}}


def run_complete(name, settings, script, calls, env=None):
    """calls: list of dict(kind=complete|extract_json, prompt, system, max_tokens, temperature)."""
    reset(script)
    patch_settings(settings)
    old_env = {}
    for k, v in (env or {}).items():
        old_env[k] = os.environ.get(k)
        os.environ[k] = v
    SLEEPS.clear()
    store = Store()
    cl = L.LLMClient(usage_store=store)
    results = []
    for c in calls:
        kw = {k: v for k, v in c.items() if k in ("system", "max_tokens", "temperature")}
        if c.get("kind", "complete") == "complete":
            r = asyncio.run(cl.complete(c["prompt"], **kw))
        else:
            kw.pop("temperature", None)
            r = asyncio.run(cl.extract_json(c["prompt"], **kw))
        results.append(r)
    for k, v in old_env.items():
        if v is None:
            os.environ.pop(k, None)
        else:
            os.environ[k] = v
    return {
        "name": name, "settings": sub(settings), "script": sub(script), "env": env or {}, "calls": calls,
        "results": results, "requests": sub(STATE["log"]), "sleeps": list(SLEEPS),
        "usage": cl.usage.to_dict(), "usage_errors_all": list(cl.usage.errors), "usage_rows": store.rows,
    }


OPENAI_IDS = ["cerebras", "groq", "sambanova", "nvidia", "mistral", "openrouter", "github_models", "siliconflow"]


def prov_settings(ids, extra=None):
    s = {}
    for pid in ids:
        cfg = L.PROVIDER_CONFIG[pid]
        s[cfg["env_key"]] = f"key-{pid}"
        s[cfg["env_url"]] = "{{BASE}}/" + pid + "/v1"
    s.update(extra or {})
    return s


V = VECTORS["complete"]

# 1. each OpenAI-compatible provider: request body per provider id, token param, defaults
for pid in OPENAI_IDS:
    V.append(run_complete(f"openai_compat_{pid}", prov_settings([pid], {"LLM_DEFAULT_PROVIDER": pid}),
                          {f"/{pid}/v1/chat/completions": [ok_openai("  pong \n", 9, 3, headers={"X-RateLimit-Limit": "1000", "X-RateLimit-Remaining": "999", "X-RateLimit-Reset": "60s"})]},
                          [{"prompt": "ping", "system": "be terse"}]))
# model override
V.append(run_complete("model_override", prov_settings(["groq"], {"LLM_DEFAULT_PROVIDER": "groq", "GROQ_MODEL": "custom-model"}),
                      {"/groq/v1/chat/completions": [ok_openai("x")]}, [{"prompt": "p", "max_tokens": 77, "temperature": 0.9}]))
# no system; lowercase rate headers
V.append(run_complete("no_system_lower_headers", prov_settings(["groq"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [ok_openai("x", headers={"x-ratelimit-limit-requests": "50", "x-ratelimit-remaining-requests": "49", "x-ratelimit-reset-requests": "2s"})]},
                      [{"prompt": "p"}]))
# content null falls back to reasoning; empty choices; empty content
V.append(run_complete("reasoning_fallback", prov_settings(["groq"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [{"status": 200, "json": {"choices": [{"message": {"content": None, "reasoning": " thought "}}], "usage": {"prompt_tokens": 1, "completion_tokens": 2}}}]},
                      [{"prompt": "p"}]))
V.append(run_complete("empty_choices_next_provider", prov_settings(["groq", "cerebras"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [{"status": 200, "json": {"choices": [], "usage": {"prompt_tokens": 4, "completion_tokens": 0}}}],
                       "/cerebras/v1/chat/completions": [ok_openai("from cerebras")]},
                      [{"prompt": "p"}]))
# no providers configured
V.append(run_complete("no_providers", {}, {}, [{"prompt": "p"}]))
# failover: 500 -> next immediately (no sleep)
V.append(run_complete("failover_500", prov_settings(["groq", "cerebras"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [{"status": 500, "json": {"error": {"message": "boom upstream"}}}],
                       "/cerebras/v1/chat/completions": [ok_openai("ok2")]}, [{"prompt": "p"}]))
# 429 retry with backoff then success on same provider
V.append(run_complete("retry_429_then_ok", prov_settings(["groq", "cerebras"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [{"status": 429, "json": {"error": {"message": "Rate limit reached"}}}, {"status": 429, "json": {"error": {"message": "Rate limit reached"}}}, ok_openai("third time")],
                       "/cerebras/v1/chat/completions": [ok_openai("never")]}, [{"prompt": "p"}]))
# 429 forever: 4 attempts, 3 sleeps, then next provider
V.append(run_complete("retry_429_exhausted_failover", prov_settings(["groq", "cerebras"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [{"status": 429, "json": {"error": {"message": "slow down"}}}],
                       "/cerebras/v1/chat/completions": [ok_openai("fallback")]}, [{"prompt": "p"}]))
# only first 3 of 5 providers are tried
V.append(run_complete("provider_cap_3", prov_settings(["cerebras", "groq", "sambanova", "nvidia", "mistral"], {"LLM_DEFAULT_PROVIDER": "cerebras"}),
                      {f"/{p}/v1/chat/completions": [{"status": 500, "json": {"error": {"message": "down " + p}}}] for p in ["cerebras", "groq", "sambanova", "nvidia", "mistral"]},
                      [{"prompt": "p"}]))
# error as string, error text truncation to 100, message containing 'rate' substring
V.append(run_complete("error_string_and_truncation", prov_settings(["groq"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [{"status": 400, "json": {"error": "x" * 150}}]}, [{"prompt": "p"}]))
V.append(run_complete("quota_message_retries", prov_settings(["groq"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [{"status": 403, "json": {"error": {"message": "You exceeded your current quota"}}}, ok_openai("after quota")]},
                      [{"prompt": "p"}]))
# explicit default not configured -> order by PROVIDER_ORDER
V.append(run_complete("default_order_nvidia_first", prov_settings(["groq", "nvidia", "cerebras"], {"LLM_DEFAULT_PROVIDER": "nvidia"}),
                      {"/nvidia/v1/chat/completions": [ok_openai("nv")]}, [{"prompt": "p"}]))
V.append(run_complete("timeout_message", prov_settings(["groq"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [{"status": 200, "delay": 2.0, "json": {}}]}, [{"prompt": "p"}], env={"LLM_HTTP_TIMEOUT": "1"}))

# ── Anthropic native (official SDK) ──────────────────────────────────
A = VECTORS["anthropic_call"]


def ant_settings(extra=None):
    s = {"ANTHROPIC_API_KEY": "sk-ant-test", "ANTHROPIC_BASE_URL": "{{BASE}}", "LLM_DEFAULT_PROVIDER": "anthropic"}
    s.update(extra or {})
    return s


A.append(run_complete("anthropic_basic", ant_settings({"ANTHROPIC_MODEL": "claude-sonnet-4-6"}),
                      {"/v1/messages": [ant_ok(" hello ", 20, 6, cw=100, cr=50)]}, [{"prompt": "ping", "system": "sys A", "max_tokens": 321}]))
A.append(run_complete("anthropic_default_model_no_system", ant_settings(),
                      {"/v1/messages": [ant_ok("x")]}, [{"prompt": "ping"}]))
A.append(run_complete("anthropic_cache_off", ant_settings(), {"/v1/messages": [ant_ok("x")]}, [{"prompt": "ping", "system": "S"}], env={"LLM_PROMPT_CACHE": "0"}))
A.append(run_complete("anthropic_thinking_blocks", ant_settings(),
                      {"/v1/messages": [ant_ok(content=[{"type": "thinking", "thinking": "hmm", "signature": "sig"}, {"type": "text", "text": "A"}, {"type": "text", "text": "B"}])]},
                      [{"prompt": "p"}]))
A.append(run_complete("anthropic_base_v1_suffix_is_override", ant_settings({"ANTHROPIC_BASE_URL": "{{BASE}}/prefix"}),
                      {"/prefix/v1/messages": [ant_ok("pfx")]}, [{"prompt": "p"}]))
A.append(run_complete("anthropic_429_sdk_retries_then_ok", ant_settings(),
                      {"/v1/messages": [{"status": 429, "headers": {"retry-after-ms": "1"}, "json": {"type": "error", "error": {"type": "rate_limit_error", "message": "slow"}}}, ant_ok("after retry")]},
                      [{"prompt": "p"}]))
A.append(run_complete("anthropic_401_no_retry_failover", ant_settings({"GROQ_API_KEY": "k", "GROQ_BASE_URL": "{{BASE}}/groq/v1"}),
                      {"/v1/messages": [{"status": 401, "json": {"type": "error", "error": {"type": "authentication_error", "message": "invalid x-api-key"}}}],
                       "/groq/v1/chat/completions": [ok_openai("groq saved")]}, [{"prompt": "p"}]))
A.append(run_complete("anthropic_400", ant_settings(),
                      {"/v1/messages": [{"status": 400, "json": {"type": "error", "error": {"type": "invalid_request_error", "message": "bad"}}}]}, [{"prompt": "p"}]))
A.append(run_complete("anthropic_500_exhausted", ant_settings(),
                      {"/v1/messages": [{"status": 529, "headers": {"retry-after-ms": "1"}, "json": {"type": "error", "error": {"type": "overloaded_error", "message": "Overloaded"}}}]}, [{"prompt": "p"}]))

# ── extract_json ─────────────────────────────────────────────────────
E = VECTORS["extract_json"]
cl = L.LLMClient()
PARSE_INPUTS = [
    '{"a": 1}', '```json\n{"a": 1}\n```', '```\n{"a": 1}\n```', '  {"a":1}  ', 'Sure! Here you go: {"a": 1} hope that helps',
    'text {"a": {"b": 2}} trailing', 'text {"a": {"b": {"c": 3}}} trailing', '```json\n{"a": 1}', '{"a": 1}\n```', '[1,2,3]', '"just a string"', '42', 'null',
    'no json here', '', '{"a": 1', '{broken}', "{'a': 1}", '```json {"a": 1} ```', 'pre ```json\n{"a":1}\n``` post', '{"a": 1} {"b": 2}',
    'x {"a": [1, {"b": 2}]} y', '{"s": "brace } inside"}', 'lead {"s": "brace } inside"} tail', '```JSON\n{"a": 1}\n```', '```json\n\n{"a": 1}\n\n```',
    '{"a": 1}\n\nNote: done', 'Answer:\n```json\n{"a": 1}\n```', '{"nested": {"x": 1}, "y": 2}', 'abc {"k": "v"} def {"k2": "v2"}',
    'true', '{"a": NaN}', '{"a": "é ü 😀"}', '﻿{"a": 1}', '```json\r\n{"a": 1}\r\n```',
]
for t in PARSE_INPUTS:
    r = cl._parse_json(t)
    nonfinite = r is not None and "NaN" in json.dumps(r)  # NaN is not valid JSON: record the fact, not the value
    VECTORS["parse_json"].append({"input": t, "ok": r is not None, "output": None if nonfinite else r, "nonfinite": nonfinite})

# End-to-end extract_json flow (retry on invalid JSON, system suffix)
E.append(run_complete("extract_json_first_try", prov_settings(["groq"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [ok_openai('```json\n{"company": "Acme"}\n```')]},
                      [{"kind": "extract_json", "prompt": "get it", "system": "sys", "max_tokens": 500}]))
E.append(run_complete("extract_json_retry_then_ok", prov_settings(["groq"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [ok_openai("not json at all"), ok_openai('{"a": 2}')]},
                      [{"kind": "extract_json", "prompt": "get it"}]))
E.append(run_complete("extract_json_both_invalid", prov_settings(["groq"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [ok_openai("nope"), ok_openai("still nope")]},
                      [{"kind": "extract_json", "prompt": "get it"}]))
E.append(run_complete("extract_json_empty_responses", prov_settings(["groq"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [ok_openai("")]},
                      [{"kind": "extract_json", "prompt": "get it"}]))
E.append(run_complete("extract_json_list_is_returned_as_is", prov_settings(["groq"], {"LLM_DEFAULT_PROVIDER": "groq"}),
                      {"/groq/v1/chat/completions": [ok_openai("[1, 2]")]},
                      [{"kind": "extract_json", "prompt": "get it"}]))

# ── default provider resolution ──────────────────────────────────────
D = VECTORS["default_provider"]
for name, s in [
    ("explicit", {"LLM_DEFAULT_PROVIDER": "mistral", "MISTRAL_API_KEY": "k"}),
    ("explicit_wins_over_cloud", {"LLM_DEFAULT_PROVIDER": "groq", "GROQ_API_KEY": "k", "YUPCHA_CLOUD": "1", "ANTHROPIC_API_KEY": "a"}),
    ("cloud_with_anthropic_key", {"YUPCHA_CLOUD": "true", "ANTHROPIC_API_KEY": "a", "CEREBRAS_API_KEY": "c"}),
    ("cloud_flag_values_YES", {"YUPCHA_CLOUD": " YES ", "ANTHROPIC_API_KEY": "a"}),
    ("cloud_flag_off", {"YUPCHA_CLOUD": "0", "ANTHROPIC_API_KEY": "a", "CEREBRAS_API_KEY": "c"}),
    ("cloud_without_key", {"YUPCHA_CLOUD": "1", "CEREBRAS_API_KEY": "c"}),
    ("fallback_cerebras_unset", {"GROQ_API_KEY": "g"}),
    ("explicit_unconfigured_default_then_order", {"LLM_DEFAULT_PROVIDER": "nvidia", "GROQ_API_KEY": "g", "ANTHROPIC_API_KEY": "a", "SILICONFLOW_API_KEY": "s", "CEREBRAS_API_KEY": "c"}),
    ("unknown_default", {"LLM_DEFAULT_PROVIDER": "bogus", "SAMBANOVA_API_KEY": "s"}),
    ("all_configured_default_anthropic", {"LLM_DEFAULT_PROVIDER": "anthropic", **{c["env_key"]: "k" for c in L.PROVIDER_CONFIG.values()}}),
    ("none", {}),
]:
    patch_settings(s)
    c = L.LLMClient()
    provs = c._get_providers()
    D.append({"name": name, "settings": s, "default": c._resolve_default_provider(),
              "providers": [{k: p[k] for k in ("id", "base_url", "model", "token_param", "native")} for p in provs],
              "anthropic_provider": (c.anthropic_provider() or {}).get("id")})

# ── cost ─────────────────────────────────────────────────────────────
class U:
    def __init__(self, i, o, cw=0, cr=0):
        self.input_tokens, self.output_tokens, self.cache_creation_input_tokens, self.cache_read_input_tokens = i, o, cw, cr

patch_settings({"ANTHROPIC_API_KEY": "k"})
c = L.LLMClient()
for model in ["claude-opus-4-8", "claude-sonnet-4-6", "claude-sonnet-4-6-20260101", "claude-haiku-4-5", "claude-fable-5", "unknown-model", "", "claude-opus-4-8-x"]:
    for u in [(1000, 500, 0, 0), (123456, 7890, 3000, 40000)]:
        VECTORS["cost"].append({"model": model, "usage": list(u), "usd": c.anthropic_cost_usd(U(*u), {"model": model}),
                                "price": list(c._anthropic_price_for_model(model))})
VECTORS["cost"].append({"model": "", "usage": None, "usd": c.anthropic_cost_usd(None, {"model": "x"}), "price": None})

# ── native tool call (raw Messages turn) ─────────────────────────────
reset({"/v1/messages": [ant_ok("tool turn", 30, 9, cw=5, cr=7)]})
patch_settings(ant_settings({"ANTHROPIC_MODEL": "claude-opus-4-8"}))
store = Store()
c = L.LLMClient(usage_store=store)
tools = [{"name": "get_weather", "description": "d", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}]
resp = asyncio.run(c.anthropic_tool_call([{"role": "user", "content": "weather?"}], system="static plan prefix", tools=tools,
                                        tool_choice={"type": "auto"}, output_format={"type": "json_schema", "schema": {"type": "object"}}, max_tokens=700))
VECTORS["tool_call"] = [{"name": "full", "settings": sub(ant_settings({"ANTHROPIC_MODEL": "claude-opus-4-8"})),
                         "script": sub({"/v1/messages": [ant_ok("tool turn", 30, 9, cw=5, cr=7)]}),
                         "args": {"messages": [{"role": "user", "content": "weather?"}], "system": "static plan prefix", "tools": tools,
                                  "tool_choice": {"type": "auto"}, "output_format": {"type": "json_schema", "schema": {"type": "object"}}, "max_tokens": 700},
                         "requests": sub(STATE["log"]), "usage_rows": store.rows, "usage": c.usage.to_dict()}]
reset({"/v1/messages": [ant_ok("t2")]})
store = Store(); c = L.LLMClient(usage_store=store)
sysblocks = [{"type": "text", "text": "custom block"}]
asyncio.run(c.anthropic_tool_call([{"role": "user", "content": [{"type": "text", "text": "hi"}]}], system=sysblocks))
VECTORS["tool_call"].append({"name": "system_blocks_passthrough", "settings": sub(ant_settings()), "script": sub({"/v1/messages": [ant_ok("t2")]}),
                             "args": {"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}], "system": sysblocks, "max_tokens": 1024},
                             "requests": sub(STATE["log"]), "usage_rows": store.rows, "usage": c.usage.to_dict()})

# ── message batches ──────────────────────────────────────────────────
def batch_run(name, script, reqs, **kw):
    reset(script)
    patch_settings(ant_settings())
    SLEEPS.clear()
    store = Store(); c = L.LLMClient(usage_store=store)
    submitted = []
    res = asyncio.run(c.batch_complete_anthropic(reqs, poll_interval=0.001, on_submitted=submitted.append, **kw))
    VECTORS["batch"].append({"name": name, "settings": sub(ant_settings()), "script": sub(script), "requests_in": reqs, "kw": {k: v for k, v in kw.items() if not callable(v)},
                             "result": res, "submitted": submitted, "requests": sub(STATE["log"]), "usage_rows": store.rows, "usage": c.usage.to_dict()})


bresults = "\n".join(json.dumps(x) for x in [
    {"custom_id": "b", "result": {"type": "succeeded", "message": ant_ok("B text", 10, 4, cw=2, cr=1)["json"]}},
    {"custom_id": "a", "result": {"type": "succeeded", "message": ant_ok("  A text  ", 8, 3)["json"]}},
    {"custom_id": "c", "result": {"type": "errored", "error": {"type": "error", "error": {"type": "api_error", "message": "x"}}}},
    {"custom_id": "d", "result": {"type": "succeeded", "message": ant_ok("", 1, 1)["json"]}},
]) + "\n"
bdone = {"id": "msgbatch_1", "type": "message_batch", "processing_status": "ended", "request_counts": {"processing": 0, "succeeded": 3, "errored": 1, "canceled": 0, "expired": 0},
         "created_at": "2026-01-01T00:00:00Z", "expires_at": "2026-01-02T00:00:00Z", "ended_at": "2026-01-01T00:10:00Z", "cancel_initiated_at": None,
         "archived_at": None, "results_url": "{{BASE}}/v1/messages/batches/msgbatch_1/results"}
bproc = dict(bdone, processing_status="in_progress", ended_at=None, results_url=None)
batch_run("batch_basic", {"/v1/messages/batches": [{"status": 200, "json": bproc}],
                           "/v1/messages/batches/msgbatch_1": [{"status": 200, "json": bproc}, {"status": 200, "json": bdone}],
                           "/v1/messages/batches/msgbatch_1/results": [{"status": 200, "text": bresults, "ct": "application/x-jsonl"}]},
          [{"custom_id": "a", "prompt": "PA", "system": "SYS", "max_tokens": 200}, {"custom_id": "b", "prompt": "PB"}, {"custom_id": "c", "prompt": "PC", "system": "SYS"}, {"custom_id": "d", "prompt": "PD"}])
batch_run("batch_resume_existing_with_known_ids", {"/v1/messages/batches/msgbatch_1": [{"status": 200, "json": bdone}],
                                                    "/v1/messages/batches/msgbatch_1/results": [{"status": 200, "text": bresults, "ct": "application/x-jsonl"}]},
          [{"custom_id": "a", "prompt": "PA"}], batch_id="msgbatch_1", known_result_ids=["a"])
VECTORS["batch"][-1]["kw"]["known_result_ids"] = ["a"]

# ── streaming (copilot/llm_stream.one_round) ─────────────────────────
S = VECTORS["stream"]


def sse(*events):
    out = []
    for e in events:
        out.append("data: " + (e if isinstance(e, str) else json.dumps(e)))
        out.append("")
    return out


def chunk(content=None, tool_calls=None, finish=None):
    d = {}
    if content is not None:
        d["content"] = content
    if tool_calls is not None:
        d["tool_calls"] = tool_calls
    return {"choices": [{"delta": d, "finish_reason": finish}]}


def prov(pid, base=None, key="k-" + "x"):
    return {"id": pid, "name": pid.upper(), "api_key": key, "base_url": base if base is not None else "{{BASE}}/" + pid + "/v1", "model": "m-" + pid, "openai_compatible": True}


def frame_dump(frames):
    out = []
    for f in frames:
        if f.kind == "cursor":
            out.append({"kind": "cursor", "provider": f.data.provider["id"], "fallbacks": [p["id"] for p in (f.data.fallbacks or [])]})
        else:
            out.append({"kind": f.kind, "data": f.data})
    return out


def run_stream(name, script, provider, fallbacks, messages=None, tools=None, patch_timeout=None):
    reset(script)
    provider = unsub(provider); fallbacks = unsub(fallbacks)
    messages = messages or [{"role": "user", "content": "hi"}]
    tools = tools if tools is not None else [{"type": "function", "function": {"name": "t", "parameters": {"type": "object"}}}]
    orig = httpx.AsyncClient
    if patch_timeout:
        class AC(orig):
            def __init__(self, *a, **k):
                k["timeout"] = patch_timeout
                super().__init__(*a, **k)
        LS.httpx.AsyncClient = AC

    async def go():
        fr = []
        async for f in LS.one_round(messages, tools, LS.ProviderCursor(provider, fallbacks or None), "ws1"):
            fr.append(f)
        return fr
    frames = asyncio.run(go())
    LS.httpx.AsyncClient = orig
    S.append({"name": name, "script": sub(script), "provider": sub(provider), "fallbacks": sub(fallbacks), "messages": messages, "tools": tools,
              "frames": sub(frame_dump(frames)), "requests": sub(STATE["log"]), "timeout": patch_timeout})


P = "/p1/v1/chat/completions"; Q = "/p2/v1/chat/completions"
run_stream("content_then_done", {P: [{"sse": sse(chunk("Hel"), chunk("lo"), chunk(None, None, "stop"), "[DONE]")}]}, prov("p1"), [])
run_stream("tool_calls_accumulate", {P: [{"sse": sse(
    chunk(None, [{"index": 0, "id": "call_1", "function": {"name": "search_", "arguments": ""}}]),
    chunk(None, [{"index": 0, "function": {"name": "leads", "arguments": "{\"q\":"}}]),
    chunk(None, [{"index": 1, "id": "call_2", "function": {"name": "stats", "arguments": "{}"}}]),
    chunk(None, [{"index": 0, "function": {"arguments": "\"x\"}"}}]),
    chunk(None, None, "tool_calls"), "[DONE]")}]}, prov("p1"), [prov("p2")])
run_stream("tool_calls_finish_without_calls_then_eof", {P: [{"sse": sse(chunk("a"), chunk(None, None, "tool_calls"))}]}, prov("p1"), [])
run_stream("bad_json_chunk_skipped_and_non_data_lines", {P: [{"sse": [": comment", "event: x", "data: {not json", "", "data: " + json.dumps(chunk("ok")), "", "data: [DONE]", ""]}]}, prov("p1"), [])
run_stream("empty_choices_chunk", {P: [{"sse": sse({"choices": []}, chunk("z"), "[DONE]")}]}, prov("p1"), [])
run_stream("no_api_key", {}, prov("p1", key=""), [prov("p2")])
run_stream("rate_limit_failover", {P: [{"status": 429, "json": {"error": "slow"}}], Q: [{"sse": sse(chunk("second"), "[DONE]")}]}, prov("p1"), [prov("p2"), prov("p3")])
run_stream("rate_limit_no_fallback", {P: [{"status": 429, "text": "y" * 300, "ct": "text/plain"}]}, prov("p1"), [])
run_stream("server_error_failover", {P: [{"status": 503, "text": "unavailable"}], Q: [{"sse": sse(chunk("two"), "[DONE]")}]}, prov("p1"), [prov("p2")])
run_stream("server_error_no_fallback", {P: [{"status": 502, "text": "bad gw"}]}, prov("p1"), [])
run_stream("auth_rejected_failover", {P: [{"status": 401, "text": "nope"}], Q: [{"sse": sse(chunk("two"), "[DONE]")}]}, prov("p1"), [prov("p2")])
run_stream("forbidden_no_fallback", {P: [{"status": 403, "text": "forbidden"}]}, prov("p1"), [])
run_stream("client_error_400_with_fallbacks_no_failover", {P: [{"status": 400, "text": "bad request body"}], Q: [{"sse": sse(chunk("two"), "[DONE]")}]}, prov("p1"), [prov("p2")])
run_stream("chain_p1_429_p2_500_p3_ok", {P: [{"status": 429, "text": "r"}], Q: [{"status": 500, "text": "e"}], "/p3/v1/chat/completions": [{"sse": sse(chunk("third"), "[DONE]")}]}, prov("p1"), [prov("p2"), prov("p3")])
run_stream("connect_error_failover", {Q: [{"sse": sse(chunk("saved"), "[DONE]")}]}, prov("p1", base=DEAD), [prov("p2")])
run_stream("connect_error_no_fallback", {}, prov("p1", base=DEAD), [])
run_stream("timeout_failover", {P: [{"delay": 1.5, "sse": sse("[DONE]")}], Q: [{"sse": sse(chunk("saved"), "[DONE]")}]}, prov("p1"), [prov("p2")], patch_timeout=0.4)
run_stream("timeout_no_fallback", {P: [{"delay": 1.5, "sse": sse("[DONE]")}]}, prov("p1"), [], patch_timeout=0.4)
run_stream("base_url_trailing_slash", {P: [{"sse": sse(chunk("ok"), "[DONE]")}]}, dict(prov("p1"), base_url="{{BASE}}/p1/v1/"), [])
run_stream("eof_without_done", {P: [{"sse": sse(chunk("partial"))}]}, prov("p1"), [prov("p2")])

# ── chat chain (copilotkit _get_provider_chain/_get_active_provider) ─
C = VECTORS["chat_chain"]
for name, s in [
    ("default_openrouter_unset_all_none", {}),
    ("default_groq_plus_fallbacks", {"LLM_DEFAULT_PROVIDER": "groq", "GROQ_API_KEY": "g", "OPENROUTER_API_KEY": "o", "CEREBRAS_API_KEY": "c", "MISTRAL_API_KEY": "m"}),
    ("anthropic_default_included_first", {"LLM_DEFAULT_PROVIDER": "anthropic", "ANTHROPIC_API_KEY": "a", "GROQ_API_KEY": "g"}),
    ("anthropic_not_default_skipped", {"LLM_DEFAULT_PROVIDER": "groq", "ANTHROPIC_API_KEY": "a", "GROQ_API_KEY": "g", "COHERE_API_KEY": "c", "HUGGINGFACE_API_KEY": "h"}),
    ("url_override_and_model_override", {"LLM_DEFAULT_PROVIDER": "groq", "GROQ_API_KEY": "g", "GROQ_BASE_URL": "http://x/v1", "GROQ_MODEL": "mm"}),
    ("unknown_default_falls_to_openrouter_empty", {"LLM_DEFAULT_PROVIDER": "bogus"}),
    ("unconfigured_default_keeps_others", {"LLM_DEFAULT_PROVIDER": "mistral", "NVIDIA_API_KEY": "n", "GROQ_API_KEY": "g"}),
    ("only_unknown_provider_key_unset_default_cohere", {"LLM_DEFAULT_PROVIDER": "cohere"}),
]:
    patch_settings(s)
    chain = CK._get_provider_chain()
    act = CK._get_active_provider()
    C.append({"name": name, "settings": s, "chain": chain, "active": act})

json.dump(VECTORS, open(os.path.join(OUT, "vectors.json"), "w"), indent=1, ensure_ascii=False, sort_keys=False)
print({k: len(v) for k, v in VECTORS.items()})
