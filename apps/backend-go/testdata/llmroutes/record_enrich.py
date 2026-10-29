"""Records POST /api/lead/{id}/enrich (routers/leads.py enrich_lead) as SSE bytes, with every dependency faked.

Run from legacy/python:  .venv/bin/python <this file> <out.json>
The chat provider is a scripted local HTTP server (127.0.0.1); DDGS, Nominatim (aiohttp), the email finder and the
website scraper are patched. No real network.
"""
import json
import os
import sys
import threading
import types
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.getcwd())

from fastapi import FastAPI  # noqa: E402
from fastapi.testclient import TestClient  # noqa: E402
from slowapi.errors import RateLimitExceeded  # noqa: E402
from slowapi import _rate_limit_exceeded_handler  # noqa: E402

import apps.api.routers.leads as L  # noqa: E402
import apps.api.routers.copilotkit as CK  # noqa: E402
from apps.api.core.ratelimit import limiter  # noqa: E402
from apps.api.services.leadgen.models import Lead  # noqa: E402

OUT = sys.argv[1]
STATE = {"script": None, "log": []}


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("content-length") or 0)
        body = json.loads(self.rfile.read(n))
        STATE["log"].append({"path": self.path, "authorization": self.headers.get("authorization"), "content_type": self.headers.get("content-type"), "body": body})
        r = STATE["script"]
        status = r.get("status", 200)
        data = ("".join(l + "\n" for l in r["sse"]) if "sse" in r else r.get("text", "")).encode()
        self.send_response(status)
        self.send_header("content-type", "text/event-stream" if "sse" in r else "text/plain")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
threading.Thread(target=srv.serve_forever, daemon=True).start()
BASE = f"http://127.0.0.1:{srv.server_port}/v1"


class FakeDB:
    def __init__(self, leads):
        self.leads = leads
        self.updates = []

    def get_lead(self, i):
        d = self.leads.get(i)
        return Lead.from_dict(d) if d else None

    def update_lead_fields(self, i, fields):
        self.updates.append({"id": i, "fields": fields})

    def close(self):
        pass


class Ctx:
    def __init__(self, db):
        self.db = db

    def lead_db(self):
        return self.db


LEADS = {
    1: {"id": 1, "company": "Acme Corp", "city": "Pune", "website": "https://acme.test", "specialization": "IT", "description": "old", "email": "", "phone": "", "notes": "n1  ", "state": ""},
    2: {"id": 2, "company": "NoSite", "city": "", "website": "", "email": "x@x.test"},
    3: {"id": 3, "company": "Grüße GmbH", "city": "Zürich", "website": "https://g.test", "state": "Karnataka", "notes": ""},
}
holder = {"db": None}
app = FastAPI()
app.state.limiter = limiter
app.add_exception_handler(RateLimitExceeded, _rate_limit_exceeded_handler)
app.dependency_overrides[L.require_editor] = lambda: Ctx(holder["db"])
app.include_router(L.router)
tc = TestClient(app)


def sse(*chunks, done=True):
    out = []
    for c in chunks:
        out.append("data: " + (c if isinstance(c, str) else json.dumps(c)))
        out.append("")
    if done:
        out += ["data: [DONE]", ""]
    return out


def delta(t):
    return {"choices": [{"delta": {"content": t}}]}


class FakeDDGS:
    results = {}
    raise_on = set()
    queries = []

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def text(self, q, max_results=5):
        FakeDDGS.queries.append([q, max_results])
        if q in FakeDDGS.raise_on or "*" in FakeDDGS.raise_on:
            raise RuntimeError("ddg down")
        return FakeDDGS.results.get(q, FakeDDGS.results.get("*", []))


import ddgs  # noqa: E402
ddgs.DDGS = FakeDDGS


class FakeResp:
    def __init__(self, status, data):
        self.status, self.data = status, data

    async def __aenter__(self):
        return self

    async def __aexit__(self, *a):
        return False

    async def json(self):
        return self.data


class FakeSession:
    plan = {"status": 200, "data": []}
    calls = []

    async def __aenter__(self):
        return self

    async def __aexit__(self, *a):
        return False

    def get(self, url, params=None, headers=None, timeout=None):
        FakeSession.calls.append({"url": url, "params": params, "headers": headers})
        if FakeSession.plan.get("raise"):
            raise RuntimeError(FakeSession.plan["raise"])
        return FakeResp(FakeSession.plan["status"], FakeSession.plan["data"])


import aiohttp  # noqa: E402
aiohttp.ClientSession = FakeSession

import apps.api.services.leadgen.enrichment.email_finder as EF  # noqa: E402
import apps.api.services.leadgen.enrichment.website_scraper as WS  # noqa: E402

EMAIL = {"email": ""}


def fake_enrich_emails(leads, delay=0.5):
    EMAIL["delay"] = delay
    for l in leads:
        l.email = EMAIL["email"]
    return leads


EF.enrich_emails = fake_enrich_emails
SCRAPE = {"fields": {}, "empty": False, "raise": None}


async def fake_scrape(leads):
    if SCRAPE["raise"]:
        raise RuntimeError(SCRAPE["raise"])
    if SCRAPE["empty"]:
        return []
    l = leads[0]
    if SCRAPE.get("copy"):
        import copy
        l = copy.copy(l)
    for k, v in SCRAPE["fields"].items():
        setattr(l, k, v)
    return [l]


WS.enrich_leads_from_websites = fake_scrape

PROV = {"api_key": "sk-test", "model": "m-1", "base_url": BASE + "/", "id": "x", "name": "X", "openai_compatible": True}
CK._get_active_provider = lambda: dict(PROV)

CASES = []


def run(name, lead_id, action=None, script=None, prov=None, ddg=None, ddg_raise=(), osm=None, email="", scrape=None, empty_scrape=False, scrape_raise=None, scrape_copy=False):
    holder["db"] = FakeDB({k: dict(v) for k, v in LEADS.items()})
    STATE["script"], STATE["log"] = script or {"sse": []}, []
    CK._get_active_provider = lambda: dict(prov if prov is not None else PROV)
    FakeDDGS.results, FakeDDGS.raise_on, FakeDDGS.queries = ddg or {}, set(ddg_raise), []
    FakeSession.plan, FakeSession.calls = osm or {"status": 200, "data": []}, []
    EMAIL["email"] = email
    SCRAPE.update({"fields": scrape or {}, "empty": empty_scrape, "raise": scrape_raise, "copy": scrape_copy})
    limiter.reset()
    url = f"/api/lead/{lead_id}/enrich" + (f"?action={action}" if action is not None else "")
    r = tc.post(url)
    entry = {"name": name, "lead_id": lead_id, "action": action, "status": r.status_code, "content_type": r.headers.get("content-type"),
             "cache_control": r.headers.get("cache-control"), "x_accel": r.headers.get("x-accel-buffering"), "body": r.text,
             "updates": holder["db"].updates, "llm_requests": STATE["log"], "ddg_queries": FakeDDGS.queries, "osm_calls": FakeSession.calls}
    entry["inputs"] = {"script": script, "prov_api_key": (prov if prov is not None else PROV)["api_key"], "ddg": ddg or {}, "ddg_raise": list(ddg_raise), "osm": osm,
                       "email": email, "scrape": scrape, "scrape_empty": empty_scrape, "scrape_raise": scrape_raise, "scrape_copy": scrape_copy}
    if action in (None, "find_emails"):
        entry["email_delay"] = EMAIL.get("delay")
    CASES.append(entry)


run("web_research_ok", 1, script={"sse": sse(delta("Hello "), {"choices": [{"delta": {}}]}, delta("wörld ☃ \"q\""), "not json", {"choices": [{"delta": {"content": ""}}]})})
run("web_research_default_action", 3, None, script={"sse": sse(delta("x" * 2100))})
run("web_research_no_content_no_save", 1, "web_research", script={"sse": sse({"choices": [{"delta": {"role": "assistant"}}]})})
run("web_research_no_done", 1, "web_research", script={"sse": sse(delta("abc"), done=False)})
run("web_research_http_error", 1, "web_research", script={"status": 429, "text": "rate limited " + "z" * 300})
run("web_research_no_provider", 1, "web_research", prov={**PROV, "api_key": ""})
run("web_research_trailing_slash_base", 2, "web_research", script={"sse": sse(delta("ok"))})
run("lead_not_found", 99, "web_research")
run("lead_not_found_bad_action", 99, "nope")
run("unknown_action", 1, "nope")
run("find_emails_found", 1, "find_emails", email="found@acme.test")
run("find_emails_none", 1, "find_emails", email="")
run("scrape_no_website", 2, "scrape_website")
run("scrape_found", 1, "scrape_website", scrape={"email": "s@acme.test", "phone": "+91 98765 43210", "description": "new desc", "contact_person": "Jane"})
# The real enrich_leads_from_websites mutates the lead in place, so the route compares the lead with itself and
# never saves anything (Python bug). The *_copy cases record the intended behaviour with a fake that returns a copy.
run("scrape_found_copy", 1, "scrape_website", scrape={"email": "s@acme.test", "phone": "+91 98765 43210", "description": "new desc", "contact_person": "Jane"}, scrape_copy=True)
run("scrape_phone_only_copy", 1, "scrape_website", scrape={"phone": "123"}, scrape_copy=True)
run("scrape_same_values_copy", 1, "scrape_website", scrape={"description": "old"}, scrape_copy=True)
run("scrape_same_values", 1, "scrape_website", scrape={"description": "old"})
run("scrape_no_new", 1, "scrape_website", scrape={})
run("scrape_empty_result", 1, "scrape_website", empty_scrape=True)
run("scrape_raises", 1, "scrape_website", scrape_raise="scraper exploded")
Q1, Q2 = "Acme Corp Pune contact number phone", "Acme Corp Pune office phone number"
run("find_phone_found_first", 1, "find_phone", ddg={Q1: [{"title": "Acme", "body": "call us on +91 98765-43210 today"}]})
run("find_phone_second_query", 1, "find_phone", ddg={Q1: [{"title": "x", "body": "nothing"}], Q2: [{"title": "y", "body": "Office 022 2345678 open"}]})
run("find_phone_pattern_order", 1, "find_phone", ddg={Q1: [{"title": "a 9876543210", "body": "1800 123 4567"}]})
run("find_phone_none", 1, "find_phone", ddg={"*": [{"title": "a", "body": "b"}]})
run("find_phone_ddg_raises", 1, "find_phone", ddg_raise=["*"])
run("find_phone_no_city", 2, "find_phone", ddg={"*": [{"title": "t", "body": "+919876543210"}]})
OSM_OK = {"status": 200, "data": [{"display_name": "Acme Corp, Baner Road, Pune, Maharashtra, 411045, India", "lat": "18.5", "lon": 73.8}]}
run("find_address_osm", 1, "find_address", osm=OSM_OK)
run("find_address_osm_state_present", 3, "find_address", osm=OSM_OK)
run("find_address_osm_long", 1, "find_address", osm={"status": 200, "data": [{"display_name": "A" * 400, "lat": 1, "lon": 2}]})
run("find_address_osm_no_latlon", 1, "find_address", osm={"status": 200, "data": [{"display_name": "Somewhere, Kerala"}]})
run("find_address_osm_empty_ddg_found", 1, "find_address", ddg={"Acme Corp Pune office address location": [{"body": "Our office is Located: 5 MG Road, Bengaluru, Karnataka. More text"}]})
run("find_address_osm_fail_ddg_none", 1, "find_address", osm={"raise": "connection refused " + "q" * 100}, ddg={"*": [{"body": "nothing here"}]})
run("find_address_osm_http500", 1, "find_address", osm={"status": 500, "data": {}}, ddg_raise=["*"])
run("find_address_none", 1, "find_address")
run("find_address_ddg_first_match_wins", 1, "find_address", ddg={"Acme Corp Pune office address location": [{"body": "no match"}, {"body": "Address: 1 First St. x"}, {"body": "office: 2 Second St"}]})
# rate limit: 30/minute
limiter.reset()
holder["db"] = FakeDB({k: dict(v) for k, v in LEADS.items()})
codes = [tc.post("/api/lead/2/enrich?action=scrape_website").status_code for _ in range(32)]
CASES.append({"name": "rate_limit_30_per_minute", "codes": codes})
r = tc.post("/api/lead/abc/enrich")
CASES.append({"name": "bad_lead_id", "status": r.status_code, "json": r.json()})
json.dump({"base": BASE, "cases": CASES}, open(OUT, "w"), indent=1, ensure_ascii=False)
print("ok", len(CASES))
