"""Records the copilot engine the Go GO_COPILOT_NATIVE path replaces (plan 15 WP-COPILOT-CORE).

Run from the repository root (the system prompt reads data/openui_system_prompt.txt relative to the cwd):
    legacy/python/.venv/bin/python apps/backend-go/internal/copilot/testdata/record_native.py <outdir>

No network beyond 127.0.0.1 and no database. Writes native_prompt.json, native_describe.json, native_brief.json,
native_preroute.json and native_llm.json into <outdir>.
"""
import asyncio
import json
import os
import sys
import threading
import time
import uuid as _uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.getcwd()
sys.path.insert(0, os.path.join(ROOT, "legacy/python"))
os.environ.setdefault("SQLITE_STORES_BACKEND", "sqlite")

from apps.api.services.copilot import prompt as P  # noqa: E402
from apps.api.services.copilot import tools as T  # noqa: E402
from apps.api.services.copilot import preroute as PR  # noqa: E402
from apps.api.services.leadgen import account_discovery as AD  # noqa: E402
from apps.api.services.signals import tracking as TR  # noqa: E402
from apps.api.routers import internal_copilot as IC  # noqa: E402

OUT = sys.argv[1]


def dump(name, doc):
    with open(os.path.join(OUT, name), "w") as f:
        json.dump(doc, f, indent=1, ensure_ascii=False)
        f.write("\n")


# ── system prompt ────────────────────────────────────────────────────
class _Store:
    def __init__(self, stats=None, boom=False):
        self.stats, self.boom = stats, boom

    def get_stats(self):
        if self.boom:
            raise RuntimeError("db down")
        return self.stats


prompt_cases = [
    {"name": "no_store", "store": None},
    {"name": "empty", "store": {"total": 0, "by_tier": {}}},
    {"name": "typical", "store": {"total": 42, "by_tier": {"hot": 5, "warm": 10, "cold": 20, "unqualified": 7}}},
    {"name": "partial_tiers", "store": {"total": 3, "by_tier": {"hot": 3, "null": 1}}},
    {"name": "stats_error", "store": "boom"},
]
out = []
for c in prompt_cases:
    s = c["store"]
    store = None if s is None else (_Store(boom=True) if s == "boom" else _Store(s))
    out.append({"name": c["name"], "stats": s, "prompt": P.build_system_prompt(store)})
dump("native_prompt.json", {"openui_spec_len": len(open("data/openui_system_prompt.txt").read()), "cases": out})

# ── describe_action ──────────────────────────────────────────────────
describe_cases = [
    ("start_collection", {"query": "IT staffing in Pune"}),
    ("start_collection", {}),
    ("update_lead_status", {"lead_id": 7, "status": "contacted"}),
    ("update_lead_status", {}),
    ("enrich_lead", {"lead_id": 3}),
    ("enrich_lead", {"lead_id": "x"}),
    ("import_ambitionbox_to_workbook", {}),
    ("import_ambitionbox_to_workbook", {"limit": 25, "industry": "IT Services", "name": "AB list"}),
    ("import_ambitionbox_to_workbook", {"limit": 5, "industry": "", "name": None}),
    ("create_people_workbook", {"conversation_id": "c1", "person_ids": ["p1", "p2"], "name": "Acme people"}),
    ("create_people_workbook", {}),
    ("create_people_workbook", {"conversation_id": "c1", "person_ids": []}),
    ("enrich_people_contacts", {"conversation_id": "c2", "person_ids": ["a"]}),
    ("enrich_people_contacts", {}),
    ("create_source_workbook", {"target_rows": 50, "icp_description": "SaaS in Berlin"}),
    ("create_source_workbook", {}),
    ("track_account_signals", {"workbook_id": "wb1", "account_ids": ["a", "b"], "cadence": "daily",
                               "signal_types": ["funding", "leadership_change"]}),
    ("track_account_signals", {}),
    ("track_account_signals", {"account_ids": None, "signal_types": None}),
    ("draft_grounded_outreach", {"conversation_id": "c3", "person_id": "person_x", "allow_risky": True}),
    ("draft_grounded_outreach", {}),
    ("set_workbook_refresh", {"workbook_id": "wb", "interval": "daily"}),
    ("add_agent_column", {"workbook_id": "wb"}),
    ("add_signal_trigger", {"workbook_id": "wb", "signals": ["funding"]}),
    ("not_a_tool", {"x": 1}),
    ("search_leads", {"query": "x"}),
    ("start_collection", {"query": 12}),
    ("start_collection", {"query": "naïve “quotes” ✓"}),
    ("update_lead_status", {"lead_id": 1.5, "status": True}),
    ("update_lead_status", {"lead_id": None, "status": None}),
    ("create_source_workbook", {"target_rows": [1], "icp_description": {"a": 1}}),
    ("import_ambitionbox_to_workbook", {"limit": 0, "industry": 0, "name": 0}),
]
dump("native_describe.json", {"cases": [
    {"name": n, "args": a, "description": T.describe_action(n, a, "ws")} for n, a in describe_cases
]})

# ── deterministic parsers ────────────────────────────────────────────
brief_corpus = [
    "find 50 IT staffing companies in Pune",
    "Find 25 fintech companies in Berlin that use Stripe and are hiring for partnerships roles",
    "source 10 SaaS accounts in New York using HubSpot",
    "list 100 cybersecurity businesses in London, UK",
    "build me a list of 30 logistics companies in Mumbai which are hiring sales",
    "build a list of 12 healthcare companies in Delhi",
    "show 5 B2B companies",
    "find companies in Pune",
    "find 0 widget companies",
    "find 9999 widget companies in Goa",
    "find 600 widget companies",
    "please find 20 retail companies in São Paulo",
    "Find 20 ed-tech companies in bangalore that are using AWS and hiring for data engineers",
    "find 15 companies",
    "list 40 manufacturing businesses in Ohio with ISO certification",
    "source 7 agencies in Paris",
    "find 3 D2C brands companies powered by Shopify in Toronto.",
    "who are the partnerships people at Stripe",
    "find 20 IT staffing companies in Pune!",
    "find   8   solar   companies   in   Austin",
    "FIND 11 HR COMPANIES IN CHENNAI WHO ARE HIRING RECRUITERS",
    "find ٥٠ textile companies in Cairo",
    "find 50 İstanbul logistics companies in İzmir",
    "build 60 accounts",
    "find 20 IT staffing companies in Pune using salesforce and hiring for SDR roles",
    "find 20 IT staffing companies in Pune and are hiring",
    "find 20 IT staffing companies in Pune, and in Mumbai",
    "source 1 company in a",
    "find 22 marketing agencies businesses in Los Angeles that use Webflow",
]
track_corpus = [
    "track these accounts for funding",
    "Track these accounts daily for partnership hiring and leadership changes",
    "track the accounts hourly for pricing page changes",
    "track my account weekly",
    "track those accounts for executive changes and fundraising",
    "please track these accounts",
    "track these accounts for funding daily weekly",
    "track these accounts for partnership jobs",
    "track these accounts for pricing",
    "I want to track the accounts for funding",
    "track accounts for funding",
    "TRACK THESE ACCOUNTS FOR FUNDING HOURLY",
    "track these accounts biweekly for funding",
    "  track these accounts for funding  ",
    "track these accountsfor funding",
]
dump("native_brief.json", {
    "briefs": [{"query": q, "brief": AD.build_account_discovery_brief(q),
                "extract": AD.extract_account_discovery_request(q)} for q in brief_corpus],
    "tracking": [{"text": q, "request": TR.extract_signal_tracking_request(q)} for q in track_corpus],
})

# ── preroute ─────────────────────────────────────────────────────────
class _Hist:
    def __init__(self):
        self.n = 0
        self.calls = []

    def create_tool_approval(self, ws, uid, tool_call):
        self.n += 1
        self.calls.append({"workspace_id": ws, "user_id": uid, "tool_call": tool_call})
        return f"appr-{self.n}"


class _Settings:
    def __init__(self, poller, pg):
        self.INTENT_POLLER_ENABLED, self.PG_LEAD_STORE = poller, pg


class _UUID:
    def __init__(self):
        self.n = 0

    def uuid4(self):
        self.n += 1
        return _uuid.UUID(int=self.n)


PR.uuid = _UUID()


class _NoDB:
    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


import apps.api.database as _db  # noqa: E402

_db.SessionLocal = lambda: _NoDB()
_ACCOUNTS = {}
TR.workbook_account_ids = lambda db, ws, wb: list(_ACCOUNTS.get(wb, []))


async def _people_unavailable(**kw):
    raise RuntimeError("people research is stubbed in the recorder")


import apps.api.services.leadgen.targeted_people as _tp  # noqa: E402

_tp.research_people_at_company = _people_unavailable
_tp.verify_people_at_company = _people_unavailable

people_result = {"company": "Acme", "function": "partnerships", "people": [
    {"name": "Ann Lee", "linkedin_url": "https://www.linkedin.com/in/annlee/"},
    {"name": "Bob Roy", "title": "VP"}]}
pre_corpus = [
    # (name, message, approved, priors, settings, accounts)
    ("market", "find 50 IT staffing companies in Pune", False, {}, (True, True), {}),
    ("market_approved", "find 50 IT staffing companies in Pune", True, {}, (True, True), {}),
    ("accounts_full", "Find 25 fintech companies in Berlin that use Stripe and are hiring for partnerships roles", False, {}, (True, True), {}),
    ("accounts_no_geo", "show 5 B2B companies", False, {}, (True, True), {}),
    ("team_clarify", "Stripe partnerships team", False, {}, (True, True), {}),
    ("team_clarify_approved", "Stripe partnerships team", True, {}, (True, True), {}),
    ("team_domain", "find stripe.com sales leaders", False, {}, (True, True), {}),
    ("explicit_people", "Find people currently working on Stripe's partnerships team.", False, {}, (True, True), {}),
    ("explicit_people_card", "Find named people currently working on Stripe's partnerships team. Require evidence for current employment and their partnerships remit. Return name, title, location, LinkedIn profile, evidence URL, retrieval date, and confidence. Reject similarly named companies and do not guess emails.", False, {}, (True, True), {}),
    ("explicit_people_card_approved", "Find named people currently working on Stripe's partnerships team.", True, {}, (True, True), {}),
    ("contacts_no_prior", "find their work emails", False, {}, (True, True), {}),
    ("contacts", "find their work emails", False, {"find_people_at_company": people_result}, (True, True), {}),
    ("verify", "verify them", False, {"verify_people_at_company": people_result}, (True, True), {}),
    ("workbook", "make a workbook with them", False, {"find_people_at_company": people_result}, (True, True), {}),
    ("draft", "draft an email to the verified contact but do not send", False, {}, (True, True), {}),
    ("draft_no_send_word", "draft an email to the verified contact", False, {}, (True, True), {}),
    ("track_disabled", "track these accounts daily for funding", False, {}, (False, True), {}),
    ("track_no_pg", "track these accounts daily for funding", False, {}, (True, False), {}),
    ("track_no_workbook", "track these accounts daily for funding", False, {}, (True, True), {}),
    ("track_empty_workbook", "track these accounts daily for funding", False,
     {"create_source_workbook": {"workbook_id": "wb-1"}}, (True, True), {}),
    ("track", "Track these accounts daily for partnership hiring and funding", False,
     {"create_source_workbook": {"workbook_id": "wb-1"}}, (True, True), {"wb-1": ["acct_b", "acct_a"]}),
    ("track_approved", "track these accounts daily for funding", True,
     {"create_source_workbook": {"workbook_id": "wb-1"}}, (True, True), {"wb-1": ["acct_a"]}),
    ("track_no_signals", "track these accounts", False, {}, (True, True), {}),
    ("plain", "hello there", False, {}, (True, True), {}),
    ("empty", "", False, {}, (True, True), {}),
    ("accounts_unicode", "find 20 retail companies in São Paulo using Shopify", False, {}, (True, True), {}),
    ("accounts_tech_hiring", "source 10 SaaS accounts in New York using HubSpot and are hiring for SDR roles", False, {}, (True, True), {}),
]


async def _run_preroute(msg, approved, priors, sett, accounts):
    hist = _Hist()
    _ACCOUNTS.clear()
    _ACCOUNTS.update(accounts)
    PR.uuid.n = 0

    def latest(conv_id, ws, uid, names):
        for n in names:
            if n in priors:
                return {"name": n, "result": json.loads(json.dumps(priors[n]))}
        return None

    ctx = PR.PrerouteContext(workspace_id="ws-1", user_id=7, chat_history=hist, settings=_Settings(*sett),
                             latest_tool_result=latest, describe_action=T.describe_action,
                             dangerous_tools=T.DANGEROUS_TOOLS)
    frames = []
    async for f in PR.run(ctx, msg, "conv-1", [True] if approved else []):
        if f.kind == "none":
            frames.append({"t": "none"})
        elif f.kind == "persist":
            frames.append({"t": "persist", "role": f.role, "content": f.content, "tool_data": f.tool_data})
        else:
            frames.append({"t": "sse", "line": f.line})
    return frames, hist.calls


pre_out = []
for name, msg, appr, priors, sett, accounts in pre_corpus:
    frames, approvals = asyncio.run(_run_preroute(msg, appr, priors, sett, accounts))
    pre_out.append({"name": name, "message": msg, "approved": appr, "priors": priors,
                    "settings": {"INTENT_POLLER_ENABLED": sett[0], "PG_LEAD_STORE": sett[1]},
                    "accounts": accounts, "frames": frames, "approvals": approvals})
dump("native_preroute.json", {"workspace_id": "ws-1", "user_id": 7, "conversation_id": "conv-1",
                              "uuid_hex_sequence": [_uuid.UUID(int=i).hex for i in range(1, 4)], "cases": pre_out})

# ── llm RPC (one provider round) against a fake provider ─────────────
STATE = {"script": {}, "log": [], "counts": {}}


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("content-length") or 0)
        raw = self.rfile.read(n) if n else b""
        body = json.loads(raw) if raw else None
        STATE["log"].append({"path": self.path, "authorization": self.headers.get("authorization"), "body": body})
        i = STATE["counts"].get(self.path, 0)
        STATE["counts"][self.path] = i + 1
        seq = STATE["script"].get(self.path) or [{"status": 404, "text": "no script"}]
        r = seq[min(i, len(seq) - 1)]
        data = ("".join(l + "\n" for l in r["sse"]) if "sse" in r else r.get("text", "")).encode()
        self.send_response(r.get("status", 200))
        self.send_header("content-type", "text/event-stream" if "sse" in r else "text/plain")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
threading.Thread(target=srv.serve_forever, daemon=True).start()
BASE = f"http://127.0.0.1:{srv.server_address[1]}"
DEAD = "http://127.0.0.1:1"


def prov(pid, name, key, path, model="m", dead=False):
    return {"id": pid, "name": name, "api_key": key, "base_url": (DEAD if dead else BASE) + path, "model": model,
            "openai_compatible": True}


def chunk(**delta):
    return "data: " + json.dumps({"choices": [{"delta": delta}]})


def fin(reason, **delta):
    return "data: " + json.dumps({"choices": [{"delta": delta, "finish_reason": reason}]})


TC1 = [{"index": 0, "id": "call_a", "function": {"name": "search_leads", "arguments": "{\"query\":"}}]
TC2 = [{"index": 0, "function": {"arguments": " \"acme\"}"}}]
TC3 = [{"index": 1, "id": "call_b", "function": {"name": "get_lead_stats", "arguments": ""}}]
MSGS = [{"role": "system", "content": "SYS"}, {"role": "user", "content": "hi"}]
llm_cases = [
    {"name": "text_done", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1")],
     "body": {"messages": MSGS},
     "script": {"/p1/chat/completions": [{"sse": [chunk(content="Hel"), chunk(content="lo sk-primary-123456"), "data: [DONE]"]}]}},
    {"name": "tool_calls", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1")],
     "body": {"messages": MSGS},
     "script": {"/p1/chat/completions": [{"sse": [chunk(content="Looking"), chunk(tool_calls=TC1), chunk(tool_calls=TC2),
                                                  chunk(tool_calls=TC3), fin("tool_calls")]}]}},
    {"name": "no_done_eof", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1")],
     "body": {"messages": MSGS}, "script": {"/p1/chat/completions": [{"sse": [chunk(content="partial")]}]}},
    {"name": "rate_limit_failover", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1"),
                                              prov("groq", "Groq", "sk-fallback-654321", "/p2")],
     "body": {"messages": MSGS},
     "script": {"/p1/chat/completions": [{"status": 429, "text": "slow down"}],
                "/p2/chat/completions": [{"sse": [chunk(content="ok"), "data: [DONE]"]}]}},
    {"name": "rate_limit_no_fallback", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1")],
     "body": {"messages": MSGS},
     "script": {"/p1/chat/completions": [{"status": 429, "text": "quota sk-primary-123456 exceeded"}]}},
    {"name": "server_error_failover_tools", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1"),
                                                      prov("groq", "Groq", "sk-fallback-654321", "/p2")],
     "body": {"messages": MSGS},
     "script": {"/p1/chat/completions": [{"status": 502, "text": "bad gateway"}],
                "/p2/chat/completions": [{"sse": [chunk(tool_calls=TC1), chunk(tool_calls=TC2), fin("tool_calls")]}]}},
    {"name": "auth_rejected_failover", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1"),
                                                 prov("groq", "Groq", "sk-fallback-654321", "/p2")],
     "body": {"messages": MSGS},
     "script": {"/p1/chat/completions": [{"status": 401, "text": "bad key"}],
                "/p2/chat/completions": [{"sse": ["data: [DONE]"]}]}},
    {"name": "bad_request", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1"),
                                      prov("groq", "Groq", "sk-fallback-654321", "/p2")],
     "body": {"messages": MSGS}, "script": {"/p1/chat/completions": [{"status": 400, "text": "bad body"}]}},
    {"name": "connect_failover", "chain": [prov("groq", "Groq", "sk-dead-key-000000", "/p2", dead=True),
                                           prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1")],
     "body": {"messages": MSGS},
     "script": {"/p1/chat/completions": [{"sse": [chunk(content="back"), "data: [DONE]"]}]}},
    {"name": "connect_dead", "chain": [prov("groq", "Groq", "sk-dead-key-000000", "/p2", dead=True)],
     "body": {"messages": MSGS}, "script": {}},
    {"name": "no_key", "chain": [prov("openrouter", "OpenRouter", "", "/p1")],
     "body": {"messages": MSGS}, "script": {}},
    {"name": "cursor_resumes", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1"),
                                         prov("groq", "Groq", "sk-fallback-654321", "/p2")],
     "body": {"messages": MSGS, "provider_cursor": 1},
     "script": {"/p2/chat/completions": [{"sse": [chunk(content="from groq"), "data: [DONE]"]}]}},
    {"name": "cursor_clamped", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1")],
     "body": {"messages": MSGS, "provider_cursor": 5},
     "script": {"/p1/chat/completions": [{"sse": ["data: [DONE]"]}]}},
    {"name": "tools_disabled", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1")],
     "body": {"messages": MSGS, "tools_enabled": False},
     "script": {"/p1/chat/completions": [{"sse": ["data: [DONE]"]}]}},
    {"name": "budget_hit", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1")],
     "body": {"messages": MSGS, "round_budget_hit": True},
     "script": {"/p1/chat/completions": [{"sse": ["data: [DONE]"]}]}},
    {"name": "gemini_sanitize", "chain": [prov("google_ai", "Google AI", "sk-gemini-123456", "/g", model="gemini-2.0-flash")],
     "body": {"messages": [{"role": "system", "content": "SYS"}, {"role": "user", "content": None},
                           {"role": "assistant", "content": "", "tool_calls": [{"id": "c1", "type": "function",
                            "function": {"name": "get_lead_stats", "arguments": "{}"}}]},
                           {"role": "tool", "tool_call_id": "c1", "name": "get_lead_stats", "content": "{}"},
                           {"role": "function", "name": "legacy", "content": 5},
                           {"role": "tool", "content": "orphan"},
                           {"role": "assistant", "content": ""},
                           {"content": "no role"}, "not a dict"]},
     "script": {"/g/chat/completions": [{"sse": ["data: [DONE]"]}]}},
    {"name": "sanitize_non_gemini", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1")],
     "body": {"messages": [{"role": "user", "content": ""}, {"role": "assistant", "content": None, "tool_calls": []},
                           {"role": "user", "content": {"a": 1}}]},
     "script": {"/p1/chat/completions": [{"sse": ["data: [DONE]"]}]}},
    {"name": "garbage_lines", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1")],
     "body": {"messages": MSGS},
     "script": {"/p1/chat/completions": [{"sse": [": comment", "event: x", "data: {not json", chunk(content=""),
                                                  chunk(role="assistant"), chunk(content="ok"), "data: [DONE]"]}]}},
    {"name": "finish_stop_then_done", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1")],
     "body": {"messages": MSGS},
     "script": {"/p1/chat/completions": [{"sse": [chunk(content="a"), fin("stop"), "data: [DONE]"]}]}},
    {"name": "tool_finish_without_calls", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1")],
     "body": {"messages": MSGS},
     "script": {"/p1/chat/completions": [{"sse": [fin("tool_calls"), chunk(content="x"), "data: [DONE]"]}]}},
    {"name": "failover_chain_exhausted", "chain": [prov("openrouter", "OpenRouter", "sk-primary-123456", "/p1"),
                                                   prov("groq", "Groq", "sk-fallback-654321", "/p2")],
     "body": {"messages": MSGS},
     "script": {"/p1/chat/completions": [{"status": 503, "text": "down"}],
                "/p2/chat/completions": [{"status": 500, "text": "also down sk-fallback-654321"}]}},
    {"name": "short_key_not_scrubbed", "chain": [prov("openrouter", "OpenRouter", "abc", "/p1")],
     "body": {"messages": MSGS},
     "script": {"/p1/chat/completions": [{"sse": [chunk(content="abc def"), "data: [DONE]"]}]}},
]


class _Req:
    async def is_disconnected(self):
        return False


async def _run_llm(case):
    chain = json.loads(json.dumps(case["chain"]))
    IC._provider_chain = lambda: chain
    body = {"workspace_id": "ws-1", "messages": [], "tools_enabled": True, "provider_cursor": 0,
            "round_budget_hit": False, **case["body"]}
    lines = []
    async for line in IC._llm_stream(_Req(), body):
        lines.append(json.loads(line))
    return lines


TOOLS = T.build_tools()
llm_out = []
for case in llm_cases:
    STATE["script"], STATE["log"], STATE["counts"] = case["script"], [], {}
    frames = asyncio.run(_run_llm(case))
    reqs = []
    for r in STATE["log"]:
        b = r["body"]
        if b.get("tools") == TOOLS:
            b["tools"] = "$CATALOG"
        reqs.append({"path": r["path"], "authorization": r["authorization"], "body": b})
    chain = [{**p, "base_url": p["base_url"].replace(BASE, "$BASE").replace(DEAD, "$DEAD")} for p in case["chain"]]
    llm_out.append({"name": case["name"], "chain": chain, "body": case["body"], "script": case["script"],
                    "frames": frames, "requests": reqs})
srv.shutdown()
dump("native_llm.json", {"cases": llm_out})
print("ok")
