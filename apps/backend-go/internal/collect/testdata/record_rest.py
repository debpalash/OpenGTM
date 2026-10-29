"""Part 2 of record.py: strategies, AI stages, whole jobs, enrichment stages, bulk_enrich, probes.

Every external effect is a deterministic fake; what the fakes served (search hits per query, pages
per URL, LLM answers per prompt) is written into the vectors so the Go tests replay the same world.
"""
import asyncio
import json
import os
import random
import sys
import types
import zlib

from record import HERE, LEAD_FIELDS, dump, lead_out  # noqa: E402

from apps.api.services.leadgen import ai_stages  # noqa: E402
from apps.api.services.leadgen import job_runner as jr  # noqa: E402
from apps.api.services.leadgen import proxy_client  # noqa: E402
from apps.api.services.leadgen import source_health  # noqa: E402
from apps.api.services.leadgen.enrichment import email_finder, email_verify  # noqa: E402
from apps.api.services.leadgen.enrichment.provider import EnrichmentResult  # noqa: E402
from apps.api.services.leadgen.models import Lead  # noqa: E402
from apps.api.services.leadgen.scrapers import linkedin as li_scraper  # noqa: E402


def crc(s):
    return zlib.crc32(s.encode("utf-8"))


def slug(name):
    return "".join(c for c in name.lower() if c.isalnum())


# ── the fake web ─────────────────────────────────────────────────────────────

COMPANIES = [("Acme Staffing", "acmestaffing.com"), ("Zeta Labs", "zetalabs.in"), ("Nova HR Solutions", "novahr.co.in"),
             ("Quanta Tech", "quantatech.io"), ("Bright Path Consultants", "brightpath.com"),
             ("Kite Systems", "kite-systems.com"), ("Orbit Analytics", "orbitanalytics.net"),
             ("Lotus Payroll", "lotuspayroll.in"), ("Peak Talent", "peaktalent.com"), ("Cedar Works", "cedarworks.org"),
             ("Vertex Hiring", "vertexhiring.com"), ("Saffron Digital", "saffrondigital.in"),
             ("Delta Recruiters", "deltarecruiters.co.in"), ("Helix Labs", "helixlabs.io"),
             ("Pioneer Staffing", "pioneerstaffing.com"), ("Top 10 Staffing Firms", "top10staffing.com")]
PEOPLE = ["Asha Rao", "Vikram Mehta", "Priya Nair", "Rahul Sharma", "Neha Gupta", "LinkedIn", ""]

SEARCHES = {}  # query -> hits served (recorded)
PAGES = {}  # url -> html (absent = HTTP 404)


def company_page(name, dom, rnd):
    emails = rnd.choice([[], [f"info@{dom}"], [f"sales@{dom}", "owner@gmail.com"], ["logo@2x.png", f"hr@{dom}"],
                         ["press@clutch.co"]])
    phones = rnd.choice([[], ["+91 98765 43210"], ["080-41234567", "+1 415 555 0100"], ["1800 123 4567"]])
    tel = rnd.random() < 0.3
    footer = rnd.random() < 0.3
    body = f"<p>{name} provides staffing and HR services.</p>"
    body += "".join(f"<p>Mail {e}</p>" for e in emails)
    if footer:
        body += "<footer>" + " ".join(f"Call {p}" for p in phones) + "</footer>"
    else:
        body += "".join(f"<p>Phone: {p}</p>" for p in phones)
    if tel:
        body += '<a href="tel:+91-80-4000-1234">Call</a>'
    if rnd.random() < 0.5:
        body += f'<a href="https://www.linkedin.com/company/{slug(name)}">in</a><a href="https://twitter.com/{slug(name)}">x</a>'
    if rnd.random() < 0.3:
        body += f'<a href="mailto:contact@{dom}?subject=hi">mail</a>'
    desc = f'<meta name="description" content="{name} — trusted partner since 2009">' if rnd.random() < 0.6 else ""
    return f"<html><head><title>{name}</title>{desc}<script>var x='a@b.co';</script></head><body><nav>Home</nav>{body}</body></html>"


def directory_page(kind, rnd):
    names = rnd.sample(COMPANIES, 5)
    if kind == "clutch.co":
        cards = "".join(f'<li class="provider-row"><h3><a href="/profile/{slug(n)}">{n}</a></h3></li>' for n, _ in names)
    elif kind == "goodfirms.co":
        cards = "".join(f'<div class="firm-wrapper"><a class="company-name" href="/company/{slug(n)}">{n}</a></div>' for n, _ in names)
    elif kind == "g2.com":
        cards = "".join(f'<div class="product-listing__card" data-product-name="{n}"></div>' for n, _ in names[:3])
        cards += f'<div class="product-listing__card"><h3><a>{names[3][0]}</a></h3></div>'
    else:
        cards = "".join(f'<article><h2>{n}</h2></article>' for n, _ in names) + "<div class='card'><h3>About</h3></div>"
    return f"<html><body><ul class='directory-list'>{cards}</ul></body></html>"


DOMAINS = {dom: name for name, dom in COMPANIES}
DIRECTORIES = ("clutch.co", "goodfirms.co", "g2.com", "softwaresuggest.com")


def gen_page(url):
    host = url.split("//", 1)[-1].split("/", 1)[0].split("?", 1)[0].lower()
    host = host[4:] if host.startswith("www.") else host
    if host in DIRECTORIES:
        return directory_page(host, random.Random(url)) if crc(url) % 5 else None
    if host in DOMAINS:
        return company_page(DOMAINS[host], host, random.Random(url)) if crc(url) % 4 else None
    return None


def serve_page(url):
    if url not in PAGES:
        PAGES[url] = gen_page(url)
    return PAGES[url]


def make_hits(query, n):
    if query not in SEARCHES:
        SEARCHES[query] = gen_hits(query)
    return list(SEARCHES[query][:n])


def gen_hits(query):
    n = 12
    rnd = random.Random(query)
    low = query.lower()
    hits = []
    for _ in range(rnd.randint(0, n + 2)):
        name, dom = rnd.choice(COMPANIES)
        if "site:linkedin.com/in" in low:
            p = rnd.choice(PEOPLE)
            hits.append({"title": f"{p} - CEO - {name} | LinkedIn" if p else "| LinkedIn",
                         "href": rnd.choice([f"https://in.linkedin.com/in/{slug(p) or 'x'}?trk=1",
                                             f"https://www.linkedin.com/company/{slug(name)}"]),
                         "body": f"{p} works at {name}. Email {slug(p)}@{dom}" if rnd.random() < 0.4 else ""})
            continue
        if "linkedin.com/company" in low:
            hits.append({"title": rnd.choice([f"{name} | LinkedIn", f"{name} - Overview", "LinkedIn"]),
                         "href": f"https://www.linkedin.com/company/{slug(name)}/" + rnd.choice(["", "about/"]),
                         "body": rnd.choice(["", f"{name} | 1,250 followers", "Staffing · 45 employees", "120 employees"])})
            continue
        kind = rnd.random()
        if kind < 0.35:
            path = rnd.choice(["/", "/about-us/team", "", "/contact?x=1"])
            href = f"https://www.{dom}{path}"
            hits.append({"title": rnd.choice([f"{name} | Home", f"{name} - IT Staffing", f"Welcome to {name}"]),
                         "href": href, "body": rnd.choice(["", f"{name}: 150 employees. Mail info@{dom} or call +91 98765 43210",
                                                             f"Leading firm. Rated 4.2/5 stars by 300 staff", f"Contact hr@{dom}"])})
        elif kind < 0.45:
            d = rnd.choice(["clutch.co", "goodfirms.co", "g2.com", "softwaresuggest.com"])
            href = f"https://{d}/in/it-services/{rnd.randint(1, 9)}"
            hits.append({"title": f"Top IT Services Companies - {d}", "href": href, "body": "Find the best"})
        elif kind < 0.55:
            hits.append({"title": f"{name} Jobs - Naukri.com", "href": f"https://www.naukri.com/{slug(name)}-jobs-{rnd.randint(1, 999)}",
                         "body": rnd.choice(["", "200 employees hiring now"])})
        elif kind < 0.6:
            hits.append({"title": f"{name} Careers", "href": f"https://www.indeed.com/cmp/{name.replace(' ', '-')}/reviews",
                         "body": "Rated 3.9 out of 5"})
        elif kind < 0.68:
            hits.append({"title": rnd.choice([f"{name} Reviews | AmbitionBox", "Top 20 companies in Bangalore", f"{name} Review"]),
                         "href": rnd.choice([f"https://www.ambitionbox.com/reviews/{slug(name)}-reviews",
                                             f"https://www.glassdoor.co.in/Reviews/{name.replace(' ', '-')}-Reviews-E{rnd.randint(1, 9999)}.htm"]),
                         "body": rnd.choice(["Rated 4.1/5 by 1,200 employees", "3.5 stars", ""])})
        elif kind < 0.76:
            hits.append({"title": rnd.choice(["Top 10 Staffing Companies in India 2025", "Best HR Firms – Guide",
                                              f"{name} · Profile", "IT Staffing & Recruiting in Mumbai"]),
                         "href": rnd.choice(["https://blog.example.com/top-10", "https://medium.com/x/y",
                                             "https://cdn.example.com/brochure.pdf", "https://media.example.org/a"]),
                         "body": "info@example.com 9999999999"})
        else:
            m = rnd.choice(["indiamart.com", "tradeindia.com", "justdial.com", "crunchbase.com"])
            hits.append({"title": rnd.choice([f"{name} - {m}", f"{name} | Company Profile", f"Best CRM Software 2026 | {m}"]),
                         "href": rnd.choice([f"https://www.{m}/company/{slug(name)}", f"https://www.{dom}/"]),
                         "body": rnd.choice(["", f"Reach {name} at sales@{dom}, +91-80-2345-6789"])})
    return hits[:n]


async def fake_ddg(query, max_results=15):
    return make_hits(query, max_results)


class FakeDDGS:
    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def text(self, query, max_results=10):
        return make_hits(query, max_results)


class Resp:
    def __init__(self, url):
        page = serve_page(url)
        self.text = page or ""
        self.ok = page is not None
        self.status_code = 200 if self.ok else 404


FETCHES = []


class FakeClient:
    def __init__(self, *a, **k):
        pass

    async def fetch(self, url, tier=2, timeout=8, **k):
        FETCHES.append([url, tier, timeout])
        return Resp(url)


# ── the fake LLM ─────────────────────────────────────────────────────────────

class FakeUsage:
    def __init__(self):
        self.errors = []

    def to_dict(self):
        return {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0, "cache_write_tokens": 0,
                "cache_read_tokens": 0, "calls": 0, "provider": "", "errors": self.errors[-5:]}


class FakeLLM:
    def __init__(self, mode="ok", bools=False):
        self.mode = mode
        self.bools = bools  # JSON true as a score: only the ai.json vectors (Go fixes it)
        self.calls = []
        self.usage = FakeUsage()

    async def extract_json(self, prompt, system="", max_tokens=1000):
        if self.mode == "down":
            self.usage.errors.append("groq: HTTP 503: unavailable")
            out = {}
        elif "Generate exactly 4 DuckDuckGo" in prompt:
            q = prompt.split('"')[1]
            out = {"queries": [f"{q} company", f"{q} pvt ltd", f"{q} contact page"]}
        elif "Extract business information" in prompt:
            url = prompt.split("URL: ")[1].split("\n")[0]
            c = crc(url)
            if c % 3 == 0:
                out = {"is_real_company": False}
            else:
                out = {"company_name": "AB" if c % 5 == 0 else f"AI Co {c % 97}", "description": None,
                       "email": f"ai{c % 7}@aico.com" if c % 2 else None, "phone": "+91 99887 76655",
                       "city": None, "state": "KA", "address": "1 MG Road", "specialization": "RPO",
                       "industry_tags": "Staffing, RPO", "employee_count": "50-200",
                       "employee_count_exact": (c % 300) if c % 2 else None, "founded_year": "2011",
                       "technologies": None, "contact_person": "Ravi K, CEO", "is_real_company": True}
        elif "Score each lead 0-100" in prompt:
            lines = [ln for ln in prompt.split("LEADS:\n")[1].split("\n\nScoring guide")[0].split("\n") if ln]
            scores = []
            for ln in lines:
                i = int(ln.split(".")[0])
                c = crc(ln)
                if c % 7 == 0:
                    continue
                s = "high" if c % 11 == 0 else (True if self.bools and c % 13 == 0 else (c % 130) - 10)
                scores.append({"id": i, "score": s, "tier": "x", "reason": f"reason {i}" if c % 2 else ""})
            out = {"scores": scores}
        else:
            out = {}
        self.calls.append({"prompt": prompt, "system": system, "max_tokens": max_tokens, "out": out})
        return out


# ── AI stage vectors ─────────────────────────────────────────────────────────

HTMLS = [
    "<html><head><style>p{}</style><script>x()</script></head><body><nav>Menu</nav><header>H</header>"
    "<p>Acme   builds\n\nthings.</p><footer>F</footer><noscript>n</noscript><iframe>i</iframe></body></html>",
    "<div>short</div>",
    "plain text without tags " * 200,
    "<p>Unicode café — naïve</p><p>" + "x" * 60 + "</p>",
    "<p>a</p><p>b</p><br/>c&amp;d &nbsp; e",
    "<html><body><div><span>Nested</span><b>bold</b>text</div><table><tr><td>1</td><td>2</td></tr></table></body></html>",
]


def record_ai():
    out = {"clean": [], "expand": [], "extract": [], "score": []}
    for h in HTMLS:
        for n in (3000, 50):
            out["clean"].append({"html": h, "max": n, "out": ai_stages.clean_page_text(h, n)})
    for q in ("IT staffing companies in Bangalore", "software"):
        for mode in ("ok", "down"):
            llm = FakeLLM(mode)
            got = asyncio.run(ai_stages.ai_expand_query(llm, q))
            out["expand"].append({"q": q, "mode": mode, "calls": llm.calls, "out": got})
    for i, h in enumerate(HTMLS):
        llm = FakeLLM()
        url = f"https://site{i}.example.com/"
        got = asyncio.run(ai_stages.ai_extract_company(llm, h, url, "q"))
        out["extract"].append({"html": h, "url": url, "calls": llm.calls, "out": got})
    rnd = random.Random(3)
    for n in (0, 1, 5, 7, 12):
        leads = []
        for j in range(n):
            nm, dom = rnd.choice(COMPANIES)
            leads.append(Lead(company=f"{nm} {j}", description=rnd.choice(["", "desc"]), city=rnd.choice(["", "Pune"]),
                              specialization=rnd.choice(["", "IT Staffing"]), company_size=rnd.choice(["", "51-200"]),
                              email=rnd.choice(["", f"a@{dom}"]), phone=rnd.choice(["", "123"]), score=rnd.choice([0, 33])))
        before = [lead_out(l) for l in leads]
        llm = FakeLLM(bools=True)
        _, applied = asyncio.run(ai_stages.ai_score_leads(llm, leads, ai_stages_icp()))
        out["score"].append({"in": before, "calls": llm.calls, "applied": applied, "out": [lead_out(l) for l in leads]})
    dump("ai.json", out)


def ai_stages_icp():
    from apps.api.services.leadgen.config import ICP
    return ICP


# ── harness for whole jobs and strategies ────────────────────────────────────

class FakeLedger:
    def __init__(self, cancel_at=None):
        self.job = {"status": "pending", "attempts": 0, "max_attempts": 3, "error": "", "leads_found": 0}
        self.stages = []
        self.calls = []
        self.status_reads = 0
        self.cancel_at = cancel_at

    def mark_running(self, job_id):
        self.calls.append(["mark_running"])
        self.job["status"] = "running"
        self.job["attempts"] += 1

    def mark_failed(self, job_id, error):
        self.calls.append(["mark_failed", error])
        self.job.update(status="failed", error=error)

    def create_stage(self, job_id, stage):
        self.stages.append({"stage": stage, "status": "running", "input_count": 0, "output_count": 0,
                            "rejected_count": 0, "details": "{}"})
        return len(self.stages)

    def complete_stage(self, sid, input_count=0, output_count=0, rejected_count=0, details="{}", status="done"):
        self.stages[sid - 1].update(status=status, input_count=input_count, output_count=output_count,
                                    rejected_count=rejected_count, details=details)

    def complete_job(self, job_id, leads_found=0):
        self.calls.append(["complete_job", leads_found])
        self.job.update(status="done", leads_found=leads_found)

    def fail_job(self, job_id, error):
        self.calls.append(["fail_job", error])
        if self.job["attempts"] < self.job["max_attempts"]:
            self.job.update(status="pending", error=error)
        else:
            self.job.update(status="failed", error=error)

    def get_status(self, job_id):
        self.status_reads += 1
        if self.cancel_at is not None and self.status_reads >= self.cancel_at:
            self.job["status"] = "cancelled"
        return self.job["status"]


class FakeStore:
    def __init__(self, existing=(), fail=False):
        self.existing = [Lead(company=c) for c in existing]
        self.upserts = []
        self.fail = fail

    def get_leads(self, limit=500, **k):
        return self.existing[:limit]

    def upsert_lead(self, lead):
        if self.fail:
            raise RuntimeError("database is down")
        self.upserts.append(lead_out(lead))


EVENTS = []


def fake_emit(event_type, data):
    EVENTS.append({"type": event_type, **json.loads(json.dumps(data, default=str))})


async def no_sleep(*a, **k):
    return None


def setup_world(enabled, health_disabled=()):
    import apps.api.routers.settings as settings_router
    from apps.api.services.leadgen import progress as progress_mod
    from apps.api.services.leadgen import http as http_mod

    settings_router.get_source_enabled = lambda sid: enabled.get(sid, False)
    progress_mod.progress.emit = fake_emit
    progress_mod.progress.bind_job = lambda *a: None
    jr.progress.emit = fake_emit
    jr._ddg_search = fake_ddg
    jr.asyncio.sleep = no_sleep
    proxy_client.get_ddgs = lambda *a, **k: FakeDDGS()
    li_scraper.get_ddgs = lambda *a, **k: FakeDDGS()
    li_scraper.time.sleep = lambda *a: None
    email_finder.get_ddgs = lambda *a, **k: FakeDDGS()
    email_finder.time.sleep = lambda *a: None
    http_mod.StealthClient = FakeClient
    source_health.health_disabled_names = lambda db=None: set(health_disabled)


MAPS_PLACES = {}


async def fake_maps(query, city, max_results=20, headless=True):
    rnd = random.Random(query + "|" + city)
    places = []
    for name, dom in rnd.sample(COMPANIES, 4):
        places.append({"name": name, "address": rnd.choice(["", "12 MG Road"]), "phone": rnd.choice(["", "080 2222 3333"]),
                       "website": rnd.choice(["", f"https://www.{dom}/"]), "rating": "4.4",
                       "category": rnd.choice(["", "Employment agency"])})
    MAPS_PLACES[query + "|" + city] = {"query": query, "city": city, "max_results": max_results, "places": places}
    return [Lead(company=p["name"], website=p["website"], phone=p["phone"], city=city,
                 specialization=p["category"] or query, notes=f"Address: {p['address']}" if p["address"] else "",
                 source="google_maps") for p in places]


def make_runner(llm, ledger, store):
    r = object.__new__(jr.JobRunner)
    r.ledger = ledger
    r.llm = llm
    r.client = FakeClient()
    r.db = store
    r._lead_store = lambda ws: store
    r._cancelled = set()
    return r


# ── strategies ───────────────────────────────────────────────────────────────

STRATEGY_QUERIES = ["IT staffing companies in Bangalore", "software companies", "marketing agency in New York",
                    "HR consultancy Mumbai", "payroll firms in Pune"]


def record_strategies():
    setup_world({})
    out = []
    for q in STRATEGY_QUERIES:
        for name in ("_search_and_scrape", "_search_directories", "_search_linkedin", "_search_job_boards",
                     "_search_review_sites", "_search_maps"):
            for mode in (("ok", "down") if name == "_search_and_scrape" else ("ok",)):
                EVENTS.clear()
                FETCHES.clear()
                llm = FakeLLM(mode)
                r = make_runner(llm, FakeLedger(), FakeStore())
                import apps.api.services.leadgen.scrapers.google_maps as gm
                gm.scrape_google_maps = fake_maps
                leads = asyncio.run(getattr(r, name)("job1", q))
                out.append({"strategy": name, "query": q, "llm": mode, "leads": [lead_out(l) for l in leads],
                            "events": EVENTS[:], "fetches": FETCHES[:], "llm_calls": llm.calls})
    dump("strategies.json", {"cases": out})


def record_registry_strategy():
    out = []
    for q, enabled, health, cap in (
            ("IT staffing companies in Bangalore", "all", (), None),
            ("marketing agency in New York", "all", ("yelp_us",), "5"),
            ("software companies", "some", (), None),
            ("chemical exporters", "none", (), None)):
        from apps.api.services.leadgen import source_registry as reg
        names = [s["name"] for s in reg.SOURCES]
        if enabled == "all":
            en = {n: True for n in names}
        elif enabled == "some":
            en = {n: (crc(n) % 2 == 0) for n in names}
        else:
            en = {}
        en["directories"] = True
        setup_world(en, health)
        if cap:
            os.environ["REGISTRY_SOURCE_CAP"] = cap
        else:
            os.environ.pop("REGISTRY_SOURCE_CAP", None)
        EVENTS.clear()
        r = make_runner(FakeLLM(), FakeLedger(), FakeStore())
        leads = asyncio.run(r._search_registry_sources("job1", q))
        out.append({"query": q, "enabled": sorted(k for k, v in en.items() if v), "health_disabled": list(health),
                    "cap": cap, "leads": [lead_out(l) for l in leads], "events": EVENTS[:]})
    os.environ.pop("REGISTRY_SOURCE_CAP", None)
    dump("registry_strategy.json", {"cases": out})


# ── enrichment stage fakes shared by the job vectors ─────────────────────────

async def fake_waterfall(leads, limit=None):
    cands = [l for l in leads if l.company and not l.has_email]
    if limit is not None:
        cands = cands[:limit]
    for l in cands:
        if crc(l.company) % 3 == 0:
            l.email = f"hello@{slug(l.company)}.com"
            l.email_provider = "hunter"
    return leads


class Res:
    def __init__(self, keep, confidence):
        self.keep, self.confidence = keep, confidence


async def fake_tag(lead, workspace_id=None):
    c = crc(lead.company + lead.email) % 5
    if c == 0:
        lead.email, lead.email_confidence, lead.email_provider = "", "", ""
        return Res(False, "")
    conf = {1: "verified", 2: "risky"}.get(c, "unknown")
    lead.email_confidence = conf
    return Res(True, conf)


async def fake_crosslinked(self, lead):
    if crc(lead.company) % 2 == 0:
        return EnrichmentResult(provider="crosslinked", success=True, fields={
            "decision_makers": json.dumps([{"name": "Asha Rao", "title": "CEO"}]),
            "contact_person": "Asha Rao", "contact_title": "CEO"})
    return EnrichmentResult(provider="crosslinked", success=False, error="none")


async def fake_jobspy(self, lead):
    if crc(lead.company) % 2 == 1:
        return EnrichmentResult(provider="jobspy", success=True,
                                fields={"hiring_signals": json.dumps({"total_jobs": 3, "score_boost": 7})})
    return EnrichmentResult(provider="jobspy", success=False)


async def fake_mailscout(self, lead):
    c = crc(lead.email) % 4
    if c == 0:
        return EnrichmentResult(provider="mailscout", success=True,
                                fields={"email": lead.email, "email_confidence": "smtp_verified"})
    if c == 1:
        return EnrichmentResult(provider="mailscout", success=True, fields={"email_confidence": "risky"})
    return EnrichmentResult(provider="mailscout", success=False)


MX = {"acmestaffing.com", "zetalabs.in", "novahr.co.in", "quantatech.io", "brightpath.com", "kite-systems.com"}


class Pick:
    def __init__(self, email, confidence, deliverable):
        self.email, self.confidence, self.deliverable = email, confidence, deliverable


def fake_pick(patterns):
    c = crc(patterns[0]) % 3
    if c == 0:
        return Pick(patterns[1], "smtp_verified", True)
    if c == 1:
        return Pick("", "", False)
    return Pick("", "", None)


def patch_enrichment():
    from apps.api.services.leadgen.enrichment import email_waterfall, email_deliverability
    from apps.api.services.leadgen.enrichment.providers import crosslinked, jobspy_signals, mailscout_verify
    email_waterfall.enrich_emails_waterfall = fake_waterfall
    email_deliverability.tag_email_confidence = fake_tag
    crosslinked.CrossLinkedProvider.enrich = fake_crosslinked
    jobspy_signals.JobSpySignalProvider.enrich = fake_jobspy
    mailscout_verify.MailScoutVerifyProvider.enrich = fake_mailscout
    email_finder.verify_email_mx = lambda d: d in MX
    email_verify.pick_best_email = fake_pick
    import apps.api.services.leadgen.source_stats as ss
    ss.record_run = lambda db, region, counters: RUNS.append(
        {"region": region, "counters": {k: [c.emitted, c.survived_dedup, c.validated] for k, c in counters.items()}})
    import apps.api.database as database

    class Sess:
        def commit(self):
            pass

        def rollback(self):
            pass

        def close(self):
            pass
    database.SessionLocal = Sess


RUNS = []


# ── whole jobs ───────────────────────────────────────────────────────────────

JOBS = [
    {"name": "location_all_sources", "query": "IT staffing companies in Bangalore", "intent": None,
     "enabled": {"google_maps": True, "duckduckgo": True, "directories": True, "linkedin": True, "job_boards": True,
                 "ambitionbox": True, "indiamart": True, "tradeindia": True, "clutch": True, "justdial": True,
                 "generic_hiring_surge": True},
     "existing": ["Zeta Labs", "pioneer staffing "], "llm": "ok"},
    {"name": "web_only_llm_down", "query": "software companies", "intent": None,
     "enabled": {"duckduckgo": True, "linkedin": True, "google_maps": True}, "existing": [], "llm": "down"},
    {"name": "cancel_after_strategies", "query": "HR consultancy Mumbai", "intent": None,
     "enabled": {"duckduckgo": True, "job_boards": True}, "existing": [], "llm": "ok", "cancel_at": 1},
    {"name": "cancel_after_dedup", "query": "HR consultancy Mumbai", "intent": None,
     "enabled": {"duckduckgo": True, "job_boards": True}, "existing": [], "llm": "ok", "cancel_at": 2},
    {"name": "blocked_intent", "query": "stripe.com", "intent": None, "enabled": {}, "existing": [], "llm": "ok"},
    {"name": "store_failure", "query": "payroll firms in Pune", "intent": None,
     "enabled": {"duckduckgo": True, "ambitionbox": True, "job_boards": True}, "existing": [], "llm": "ok",
     "store_fail": True},
    {"name": "maps_failure", "query": "staffing agencies in Chennai", "intent": None,
     "enabled": {"google_maps": True, "linkedin": True}, "existing": [], "llm": "ok", "maps_fail": True},
    {"name": "nothing_enabled", "query": "marketing agency in New York", "intent": None, "enabled": {},
     "existing": [], "llm": "ok"},
]


async def failing_maps(*a, **k):
    raise RuntimeError("browser crashed")


def record_jobs():
    patch_enrichment()
    out = []
    for spec in JOBS:
        setup_world(spec["enabled"])
        import apps.api.services.leadgen.scrapers.google_maps as gm
        gm.scrape_google_maps = failing_maps if spec.get("maps_fail") else fake_maps
        EVENTS.clear()
        RUNS.clear()
        FETCHES.clear()
        llm = FakeLLM(spec["llm"])
        ledger = FakeLedger(spec.get("cancel_at"))
        store = FakeStore(spec["existing"], spec.get("store_fail", False))
        r = make_runner(llm, ledger, store)
        asyncio.run(r._process_job({"id": "job1", "query": spec["query"], "tier": 1, "workspace_id": "ws1",
                                    "intent": spec["intent"]}))
        stages = []
        for s in ledger.stages:
            d = dict(s)
            d["details"] = json.loads(d["details"])
            stages.append(d)
        out.append({"spec": spec, "stages": stages, "events": EVENTS[:], "stored": store.upserts, "ledger": ledger.job,
                    "ledger_calls": ledger.calls, "source_runs": RUNS[:], "llm_calls": llm.calls})
    dump("job.json", {"jobs": out, "maps": MAPS_PLACES})


# ── enrichment stage vectors (email finder, decision makers, website) ────────

def record_enrich():
    setup_world({})
    patch_enrichment()
    from apps.api.services.leadgen.enrichment import decision_maker_finder as dmf
    from apps.api.services.leadgen.enrichment import website_scraper as ws
    out = {"personal": [], "company_email": [], "decision_makers": [], "find_dm": [], "website": []}
    rnd = random.Random(11)
    for i in range(14):
        name, dom = rnd.choice(COMPANIES)
        person = rnd.choice(PEOPLE[:5] + ["Madonna", ""])
        lead = Lead(company=name, website=rnd.choice([f"https://www.{dom}/x", "", dom]), contact_person=person,
                    email=rnd.choice(["", f"info@{dom}"]), email_confidence=rnd.choice(["", "pattern", "generic"]),
                    decision_makers=rnd.choice(["", json.dumps([{"name": person, "title": "CEO"}]), "not json"]),
                    secondary_emails=rnd.choice(["", "x@y.com"]))
        before = lead_out(lead)
        email_finder.enrich_personal_emails([lead], delay=0)
        out["personal"].append({"in": before, "out": lead_out(lead)})
        lead2 = Lead(company=name, city=rnd.choice(["", "Pune"]), website=before["website"])
        out["company_email"].append({"company": name, "city": lead2.city, "domain": email_finder._domain_from_url(lead2.website),
                                     "out": email_finder.find_email_via_search(name, lead2.city,
                                                                               email_finder._domain_from_url(lead2.website))})
    for i in range(10):
        leads = []
        for j in range(rnd.randint(1, 4)):
            name, dom = rnd.choice(COMPANIES)
            leads.append(Lead(company=rnd.choice([name, "AB"]), contact_person=rnd.choice(["", "X Y"]),
                              decision_makers=rnd.choice(["", ""]), linkedin_url=rnd.choice(["", "https://li/x"])))
        before = [lead_out(l) for l in leads]
        asyncio.run(dmf.enrich_decision_makers(leads, concurrency=2, max_contacts=2))
        out["decision_makers"].append({"in": before, "out": [lead_out(l) for l in leads]})
    for name, _ in COMPANIES[:8]:
        out["find_dm"].append({"company": name, "out": li_scraper.find_decision_makers(name, None, 3)})
    for i in range(10):
        name, dom = rnd.choice(COMPANIES)
        leads = [Lead(company=name, website=rnd.choice([f"https://www.{dom}/deep/path", f"www.{dom}", ""]),
                      phone=rnd.choice(["", "080 1"]), email=rnd.choice(["", "a@b.co"]),
                      description=rnd.choice(["", "has"]), twitter_url=rnd.choice(["", "t"]))]
        before = [lead_out(l) for l in leads]
        asyncio.run(ws.enrich_leads_from_websites(leads, batch_size=5))
        out["website"].append({"in": before, "out": [lead_out(l) for l in leads]})
    dump("enrich.json", out)


# ── source probes ────────────────────────────────────────────────────────────

def record_probe():
    setup_world({})
    from apps.api.services.leadgen import source_registry as reg
    out = []
    for s in reg.get_all_sources(enabled_only=False)[::5]:
        out.append({"name": s["name"], "yield": asyncio.run(source_health._run_probe(s))})
    dump("probe.json", out)


record_ai()
record_strategies()
record_registry_strategy()
record_jobs()
record_enrich()
record_probe()
dump("world.json", {"searches": SEARCHES, "pages": PAGES})
