"""Record plan 15 WP-COLLECT vectors from the REAL legacy Python leadgen modules.

Never hand-written. No network, no database: fakes stand in for search, fetch, the LLM and the
enrichment providers. Run from legacy/python with its venv:

    cd legacy/python && TZ=UTC .venv/bin/python -I ../../apps/backend-go/internal/collect/testdata/record.py

Writes next to this file:
    names.json      helpers of job_runner.JobRunner (region, city, location, expand, business name, ...)
    validate.json   lead_validator.validate_lead / validate_lead_light (verdict + cleaned fields)
    dedup.json      pipeline.deduplicate_leads and thefuzz token_sort_ratio
    registry.json   get_all_sources, build_queries (../sources.json: source_registry.SOURCES, embedded)
    ai.json         ai_stages prompts (exact text), clean_page_text and output parsing
    strategies.json each collection strategy over recorded search hits / pages
    job.json        whole _process_job runs: stage rows, progress events, stored leads, ledger
    enrich.json     website enrichment, decision makers, email finder, bulk_enrich
    probe.json      source_health._run_probe yields
"""
import asyncio
import copy
import datetime as dt
import json
import os
import pathlib
import random
import sys

HERE = pathlib.Path(__file__).resolve().parent
ROOT = pathlib.Path.cwd()
sys.path.insert(0, str(ROOT))
os.environ["TZ"] = "UTC"
for k in ("SOURCE_RELIABILITY_RANKING", "REGISTRY_SOURCE_CAP", "DM_DISCOVERY_BUDGET", "EMAIL_WATERFALL_BUDGET",
          "DELIVERABILITY_BUDGET", "SOURCE_HEALTH_ENFORCE"):
    os.environ.pop(k, None)

from apps.api.services.leadgen import job_runner as jr  # noqa: E402
from apps.api.services.leadgen import lead_validator as lv  # noqa: E402
from apps.api.services.leadgen import pipeline  # noqa: E402
from apps.api.services.leadgen import source_registry as reg  # noqa: E402
from apps.api.services.leadgen.models import Lead  # noqa: E402
from thefuzz import fuzz  # noqa: E402

RUNNER = object.__new__(jr.JobRunner)  # the pure helpers need no DB


def dump(name, data):
    p = HERE / name
    p.write_text(json.dumps(data, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    print("wrote", p)


# Lead fields compared by the Go tests (timestamps are clocks, excluded).
LEAD_FIELDS = [f for f in Lead.__dataclass_fields__ if f not in ("created_at", "updated_at", "id")]


def lead_out(l):
    return {f: getattr(l, f) for f in LEAD_FIELDS}


QUERIES = [
    "IT staffing companies in Bangalore", "HR consultancy Mumbai", "recruitment agency in bengaluru",
    "software companies", "marketing agency in New York", "logistics firms Gurugram", "fintech startups in Pune",
    "staffing agencies", "SaaS companies London", "manufacturing companies in Chennai pvt ltd",
    "AI startups in San Francisco", "chemical exporters india", "cloud consultancies Berlin",
    "payroll firms Noida", "data analytics companies in Hyderabad", "EdTech in Delhi",
    "construction staffing in Kolkata", "Bangalore", "  ", "consultancies in Ahmedabad",
    "agencies list", "IT Services in DELHI", "companies in Thane", "bpo in Gurgaon India",
    "Pvt Ltd manufacturers", "ad agencies in Miami USA", "Mumbai Delhi Pune", "café owners in Paris",
    "Ünïcode Firms Bengaluru", "biotech united states",
]

TITLES = [
    "Acme Corp | LinkedIn", "Top 10 IT Staffing Companies in Bangalore 2024", "Infosys - Wikipedia",
    "TechNova Solutions — Home", "Best HR Firms – Guide", "GoodFirms | Clutch.co", "Zeta Labs · Careers",
    "Ranking of companies", "Quess Corp Reviews", "Company Profile - Wipro", "A | B", "",
    "This is a very long title that goes on and on about nothing in particular at all, really long",
    "Teamlease Services Ltd - Jobs", "Clutch.co", "Hiring: Freshworks", "Naukri.com - Jobs in India",
    "Mphasis — IT Services 2025 – Bangalore", "Datamatics Global Services Ltd | Company Profile",
    "XYZ – – –", "Søren & Co | Profile", "LinkedIn", "Best 5 CRM Software | Capterra",
]

DOMAINS = [
    "www.datamatics.com", "talentleads.co.in", "v3staffing.in", "linkedin.com", "my-company_name.io",
    "www.ab.com", "abc.co", "clutch.co", "sub.domain.example.org", "", "www.naukri.com", "teamLease.net",
    "123go.com", "go2market-labs.co.in", "x.in", "www.Zeta-Tech.com", "quora.com", "medium.com", "cafe-münchen.de",
]

SIZES = [
    "We have 150 employees", "1,200 people work here", "40 staff", "5000 workers", "10 employees",
    "no number", "employees 50", "199 Employees", "200 people", "499 staff", "500 employees",
    "1,0 employees", "Team of 25 people and 2 offices", "٣٤٥ employees",
]


def record_names():
    out = {"region": [], "city": [], "location": [], "expand": [], "business_name": [], "clean_name": [],
           "company_from_domain": [], "size": [], "skip_domain": []}
    for q in QUERIES:
        city = RUNNER._extract_city(q)
        out["city"].append({"in": q, "out": city})
        out["location"].append({"in": q, "out": RUNNER._is_location_query(q)})
        out["expand"].append({"in": q, "out": RUNNER._expand_query(q)})
        out["region"].append({"in": q, "city": city, "out": RUNNER._detect_region(q, city)})
        out["region"].append({"in": q, "city": "", "out": RUNNER._detect_region(q)})
    for t in TITLES:
        out["business_name"].append({"in": t, "out": RUNNER._extract_business_name(t)})
        out["clean_name"].append({"in": t, "out": RUNNER._clean_name(t)})
    for d in DOMAINS:
        out["company_from_domain"].append({"in": d, "out": RUNNER._company_from_domain(d)})
        out["skip_domain"].append({"in": d, "out": RUNNER._is_skip_domain(d)})
    for s in SIZES:
        out["size"].append({"in": s, "out": RUNNER._extract_size(s)})
    dump("names.json", out)


NAMES = [
    "", " ", "A", "Ab", "Acme", "Acme Technologies Pvt Ltd", "Top 10 Staffing Firms", "best 5 agencies",
    "20 Best Companies", "The Ultimate Guide", "Updated 2024 list", "Firms 2026", "How to hire",
    "What is RPO", "Discover the best", "Complete list of vendors", "List of firms", "Companies that use SAP",
    "Websites using React", "Customers of Workday", "Working at Infosys", "Jobs at TCS", "results", "Contact Us",
    "staffing agency", "IT Solutions", "Clutch", "Good Firms", "goodfirms", "The Manifest", "LinkedIn",
    "IT Services in Mumbai", "Best HR firms in Pune", "Staffing companies in Delhi", "Find HR Consultants",
    "Looking for staff", "Hire Top Developers", "Free Classifieds India", "Ads in Mumbai", "All Companies",
    "Page 3", "Multi Recruit", "Verified Suppliers", "52 Bangalore Based Staffing Agency Companies",
    "Business directory", "HR consulting service Mumbai", "IT staffing & recruiting partners", "example.com/",
    "IT Staffing Companies/Agencies", "HR Outsourcing - SiteName", "HR solutions india", "Staffing agency in Pune",
    "Need a plumber", "Compare quotes", "Reviews of TCS", "Next", "show more", "acme.com", "Staffing in Bangalore",
    "HR Consultancy in Mumbai", "Manpower services in Delhi", "HR Consulting & Career Choice in Bangalore",
    "Acme Corp |", "Steel Manufacturers Suppliers", "Recruitment agency companies", "Sales Executive",
    "Managing Director", "Bing", "G Mail", "gmail", "12", "1 2 3 ab", "Big Bold Amazing Fantastic Wonderful Company Name Ltd",
    "Big Bold Amazing Fantastic Wonderful", "x" * 61, "Café Société", "Ünïcode Systems", "Teamlease", "Quess",
    "ABC Consultants", "Top Notch Consulting", "Desktop10", "Café 2024", "SAP Experts In India", "Mphasis",
    "Infosys Ltd.", "Zoho Corporation", "Freshworks Inc", "Head", "Team Lead", "Acme Lead Gen Co", "The Hindu",
    "AI-driven HR", "Tata Consultancy Services", "Wipro;", "HCL:", "Bharat Forge", "Ola Electric",
    "Paytm (One97 Communications)", "Ninja Head Hunters", "Agency of Record",
]

CONTACTS = [
    {},
    {"email": "info@acme.com", "website": "https://www.acme.com/about/team"},
    {"email": "sales@clutch.co"},
    {"email": "logo@2x.png"},
    {"email": "hr@other.com", "website": "acme.in"},
    {"email": "owner@gmail.com", "website": "https://acme.in"},
    {"phone": "1234567890"},
    {"phone": "+1 415 555 0100"},
    {"phone": "+971 4 123 4567"},
    {"phone": "16000000000000"},
    {"phone": " 080 4123 4567 "},
    {"website": "https://www.glassdoor.co.in/Reviews/x"},
    {"website": "http://sub.linkedin.com/company/x"},
    {"linkedin_url": "https://www.linkedin.com/company/acme"},
    {"website": "N/A"},
    {"email": "x@acme.com", "website": "acme.com", "email_confidence": "pattern", "email_provider": "hunter"},
    {"email": "nobody", "website": "https://acme.io?x=1#y"},
    {"email": "a@b.co", "website": "  https://B.co/path  "},
]


def record_validate():
    out = []
    rnd = random.Random(15)
    for i, name in enumerate(NAMES):
        contacts = [CONTACTS[0], CONTACTS[(i % (len(CONTACTS) - 1)) + 1], rnd.choice(CONTACTS)]
        for c in contacts:
            for fn, label in ((lv.validate_lead, "full"), (lv.validate_lead_light, "light")):
                lead = Lead(company=name, **c)
                ok, reason = fn(lead)
                out.append({"fn": label, "company": name, "in": c, "ok": ok, "reason": reason, "out": {
                    k: getattr(lead, k) for k in ("email", "email_confidence", "email_provider", "phone", "website",
                                                  "linkedin_url")}})
    dump("validate.json", out)


def record_dedup():
    ratios = []
    pairs = [("Acme Corp", "acme corp"), ("Acme Corporation", "Corporation Acme"), ("Café Ü", "cafe u"),
             ("", ""), ("a", ""), ("ĀB ab", "ab"), ("Infosys Ltd", "Infosys Limited"), ("TCS", "TCS Ltd"),
             ("Tata Consultancy Services", "Tata Consulting Services"), ("Wipro", "Wipro Technologies"),
             ("  x--y  ", "y x"), ("Zeta-Labs", "zeta labs"), ("ab", "abc"), ("abcd", "abce"), ("Ølstad AS", "olstad as")]
    for a, b in pairs:
        ratios.append({"a": a, "b": b, "out": fuzz.token_sort_ratio(a, b)})
    rnd = random.Random(7)
    base = ["Acme Corp", "acme corporation", "ACME CORP", "Zeta Labs", "Zeta Labs Pvt Ltd", "Infosys", "Infosys Ltd",
            "Wipro", "TCS", "Tata Consultancy", "Quess Corp", "Quess", "Mphasis", "Freshworks", "Fresh Works",
            "Teamlease", "Team Lease Services", "Ola", "Olaa", "Ol a"]
    cases = []
    for n in range(12):
        leads = []
        for j in range(rnd.randint(0, 9)):
            d = {"company": rnd.choice(base)}
            for f in ("website", "email", "phone", "linkedin_url", "contact_person"):
                if rnd.random() < 0.35:
                    d[f] = f + str(j)
            leads.append(d)
        got = pipeline.deduplicate_leads([Lead(**d) for d in leads])
        cases.append({"in": leads, "out": [l.company + "|" + l.website + "|" + l.email for l in got]})
    dump("dedup.json", {"ratio": ratios, "dedup": cases})


def record_registry():
    regions = ["india", "us", "eu", "global", None, "mars"]
    all_sources = {str(r): [s["name"] for s in reg.get_all_sources(region=r)] for r in regions}
    all_sources["all_disabled_too"] = [s["name"] for s in reg.get_all_sources(enabled_only=False)]
    build = []
    for s in reg.SOURCES[::7]:
        for q, city in (("IT staffing companies in Bangalore", "Bangalore"), ("software companies", ""),
                        ("marketing agency", "New York"), ("HR In Mumbai", "Mumbai")):
            build.append({"source": s["name"], "q": q, "city": city, "out": reg.build_queries(s, q, city)})
    dump("registry.json", {"get_all_sources": all_sources, "build_queries": build})
    src = HERE.parent / "sources.json"  # embedded by the Go package
    src.write_text(json.dumps(reg.SOURCES, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    print("wrote", src)


if __name__ == "__main__":
    which = set(sys.argv[1:])
    for name, fn in (("names", record_names), ("validate", record_validate), ("dedup", record_dedup),
                     ("registry", record_registry)):
        if not which or name in which:
            fn()
    if not which or "rest" in which:
        sys.path.insert(0, str(HERE))
        import record_rest  # noqa: F401  (strategies, ai, job, enrich, probe)
