"""Records workbook/nl_column.py, routers/campaigns.py and the provider-accuracy scorer (plan 15 WP-LLMROUTES).

Run from legacy/python:  .venv/bin/python <this file> <outdir>
The LLM is a scripted fake (no network). Writes nl_column.json, campaigns.json, accuracy.json, requests.json.
"""
import asyncio
import json
import os
import sys
from datetime import date

sys.path.insert(0, os.getcwd())

from fastapi import FastAPI  # noqa: E402
from fastapi.testclient import TestClient  # noqa: E402

import apps.api.routers.campaigns as CAMP  # noqa: E402
import apps.api.services.workbook.nl_column as N  # noqa: E402
from apps.api.services.leadgen.llm import llm as REAL_LLM  # noqa: E402
from apps.api.services.leadgen.models import Lead  # noqa: E402
from apps.api.services.workbook.schemas import GenerateColumnRequest, GenerateColumnResponse  # noqa: E402

OUT = sys.argv[1]
os.makedirs(OUT, exist_ok=True)


class FakeLLM:
    """Scripted extract_json: each entry is a dict, or an Exception instance to raise."""

    def __init__(self, script):
        self.script = list(script)
        self.calls = []

    async def extract_json(self, prompt, system="", max_tokens=2048, **kw):
        self.calls.append({"prompt": prompt, "system": system, "max_tokens": max_tokens})
        if not self.script:
            return {}
        r = self.script.pop(0)
        if isinstance(r, Exception):
            raise r
        return r


def dump(name, obj):
    with open(os.path.join(OUT, name), "w") as f:
        json.dump(obj, f, indent=1, ensure_ascii=False)


# ── nl_column ──────────────────────────────────────────────────────────

COLS = [
    {"id": "email", "name": "Email", "type": "lead_field"},
    {"id": "company", "name": "Company", "type": "lead_field"},
    {"id": "website", "name": "Website", "type": "lead_field"},
]

FORMULA = {"kind": "formula", "name": "Email Domain", "formula": '{Email}.split("@")[1]', "explanation": "Splits."}
BAD = {"kind": "formula", "name": "Broken", "formula": "import os", "explanation": "bad"}
AIF = {"kind": "ai_formula", "name": "B2B or B2C", "prompt": "Classify {Company}", "explanation": "Needs judgment."}

many = [{"id": f"c{i}", "name": f"Col {i}", "type": "enrichment"} for i in range(70)]

NL_CASES = [
    ("formula_ok", "extract the domain from the email", COLS, [FORMULA]),
    ("formula_no_explanation", "domain of the email", COLS, [{**FORMULA, "explanation": ""}]),
    ("formula_upper_join", "full name in caps", COLS, [{"kind": "formula", "name": "Full", "formula": 'upper(default({First Name}, "")) + " " + upper({Last Name})'}]),
    ("ai_formula_ok", "classify each company as B2B or B2C", COLS, [AIF]),
    ("ai_formula_missing_prompt", "classify companies", COLS, [{"kind": "ai_formula", "name": "X"}]),
    ("http_full", "get the logo from clearbit", COLS, [{"kind": "http", "name": "Logo URL", "http_url": "https://logo.clearbit.com/{Website}", "http_method": "post", "http_headers": {"Authorization": "Bearer {Token}", "N": 5}, "http_body": {"d": "{Website}"}, "http_extract": " $.data.logo ", "explanation": "Calls it."}]),
    ("http_get_drops_body", "get a thing", COLS, [{"kind": "http", "name": "T", "http_url": "https://x.test/{Email}", "http_body": {"a": 1}}]),
    ("http_bad_method_to_get", "get a thing", COLS, [{"kind": "http", "name": "T", "http_url": "https://x.test", "http_method": "DELETE", "http_headers": {}, "http_body": "s"}]),
    ("http_headers_not_dict", "get a thing", COLS, [{"kind": "http", "name": "T", "http_url": "https://x.test", "http_headers": ["a"], "http_method": "put", "http_body": "raw body"}]),
    ("http_missing_url", "call an api", COLS, [{"kind": "http", "name": "T"}]),
    ("unknown_kind", "do something", COLS, [{"kind": "magic", "name": "T"}]),
    ("kind_missing", "do something", COLS, [{"name": "T"}]),
    ("empty_llm_response", "do something", COLS, [{}]),
    ("llm_not_a_dict", "do something", COLS, [[1, 2]]),
    ("empty_instruction", "   ", COLS, []),
    ("formula_retry_ok", "make domain", COLS, [BAD, FORMULA]),
    ("formula_retry_still_bad", "make domain", COLS, [BAD, {"kind": "formula", "name": "Again", "formula": "__import__('os')"}]),
    ("formula_retry_invalid_kind", "make domain", COLS, [BAD, {"kind": "zzz"}]),
    ("formula_retry_empty", "make domain", COLS, [BAD, {}]),
    ("formula_retry_ai_formula", "make domain", COLS, [BAD, AIF]),
    ("formula_retry_llm_empty_name_fallback", "do the thing well please", COLS, [{"kind": "formula", "formula": "x y"}, {}]),
    ("formula_empty_expression", "do thing", COLS, [{"kind": "formula", "name": "E", "formula": "  "}, FORMULA]),
    ("formula_division_by_zero", "ratio", COLS, [{"kind": "formula", "name": "R", "formula": "1 / 0"}, {"kind": "formula", "name": "R", "formula": "{Employees} * 2"}]),
    ("formula_bad_int", "headcount", COLS, [{"kind": "formula", "name": "R", "formula": "int({Email})"}, {"kind": "formula", "name": "R", "formula": "len({Email})"}]),
    ("name_blank_uses_instruction", "make A Nice column from the email address please", COLS, [{"kind": "formula", "name": "  ", "formula": "{Email}"}]),
    ("name_blank_blank_instruction_slug", "!!", COLS, [{"kind": "formula", "formula": "{Email}"}]),
    ("id_collision", "domain", [{"id": "email_domain", "name": "x"}, {"id": "email_domain_2", "name": "y"}], [FORMULA]),
    ("id_slug_symbols", "domain", COLS, [{**FORMULA, "name": "  Ünï Côl -- #1!  "}]),
    ("id_slug_all_symbols", "domain", COLS, [{**FORMULA, "name": "###"}]),
    ("no_columns", "domain", [], [{"kind": "formula", "name": "L", "formula": '"a" + "b"'}]),
    ("none_columns", "domain", None, [AIF]),
    ("many_columns_capped", "summarize", many, [AIF]),
    ("columns_id_only", "domain", [{"id": "a1"}, {"name": "B"}, {}], [FORMULA]),
    ("dummy_values", "mixed", [{"id": "e", "name": "Work Email"}, {"id": "w", "name": "Company Website"}, {"id": "p", "name": "Phone"}, {"id": "s", "name": "Employee Count"}, {"id": "n", "name": "Contact Name"}, {"id": "o", "name": "Other"}],
     [{"kind": "formula", "name": "Z", "formula": '{Work Email}.split("@")[1] + {Company Website}[8:12] + {Phone}[0:2] + str(int({Employee Count}) * 2) + {Contact Name}.split(" ")[0] + {Other}.lower() + {Unknown Thing}'}]),
    ("llm_raises", "x", COLS, [RuntimeError("boom")]),
    ("fallback_refs_capped", "ai thing", [{"id": f"i{i}", "name": f"N{i}"} for i in range(12)], [BAD, {}]),
    ("fallback_no_columns", "ai thing", [], [BAD, {}]),
    ("unicode_instruction", "extrahiere die Domain aus der E-Mail für Zürich", COLS, [FORMULA]),
    ("explanation_non_string", "domain", COLS, [{**FORMULA, "explanation": 5}]),
    ("name_non_string", "domain", COLS, [{**FORMULA, "name": 123}]),
]

nl = []
for name, instr, cols, script in NL_CASES:
    fake = FakeLLM(script)
    N.llm = fake
    entry = {"name": name, "instruction": instr, "columns": cols, "llm": script if not any(isinstance(s, Exception) for s in script) else "raises", "calls": None}
    try:
        res = asyncio.run(N.generate_column(instr, cols))
        entry["result"] = res
        entry["response"] = GenerateColumnResponse.model_validate(res).model_dump(mode="json")
    except N.NLColumnError as e:
        entry["error"] = str(e)
    except Exception as e:  # llm raising propagates in Python (route -> 500)
        entry["exception"] = type(e).__name__
    entry["calls"] = fake.calls
    if entry["llm"] == "raises":
        entry["llm"] = [{"__raise__": str(s)} if isinstance(s, Exception) else s for s in script]
    nl.append(entry)

dump("nl_column.json", {
    "funcs": sorted(N._FUNCS),
    "str_methods": sorted(N._STR_METHODS),
    "dsl_reference": N._DSL_REFERENCE,
    "system": N._SYSTEM,
    "cases": nl,
})

# ── request validation (FastAPI 422 bodies) ───────────────────────────

app = FastAPI()


@app.post("/gc")
async def gc(body: GenerateColumnRequest):
    return {"ok": True, "instruction": body.instruction}


@app.post("/camp")
async def camp(body: CAMP.GenerateRequest):
    return {"ok": True, "body": body.model_dump()}


tc = TestClient(app)
GC_BODIES = [b'{"instruction":"extract the domain"}', b'{"instruction":"ab"}', b'{"instruction":"' + b"x" * 2001 + b'"}', b'{"instruction":"' + b"x" * 2000 + b'"}',
             b"{}", b"[]", b'"s"', b"null", b"", b"{bad", b'{"instruction":5}', b'{"instruction":null}', b'{"instruction":["a"]}', b'{"instruction":"abc","extra":1}',
             b'{"instruction":"  ab  "}', b'{"instruction":"\xf0\x9f\x98\x80\xf0\x9f\x98\x80\xf0\x9f\x98\x80"}']
CAMP_BODIES = [
    b'{"lead_ids":[1,2],"value_proposition":"vp"}',
    b'{"lead_ids":[1],"value_proposition":"vp","tone":"friendly","extra_context":"ctx"}',
    b"{}", b"[]", b"null", b"", b"{bad",
    b'{"lead_ids":"1","value_proposition":"vp"}',
    b'{"lead_ids":[],"value_proposition":"vp"}',
    b'{"lead_ids":["5"],"value_proposition":"vp"}',
    b'{"lead_ids":[5.0],"value_proposition":"vp"}',
    b'{"lead_ids":[5.5],"value_proposition":"vp"}',
    b'{"lead_ids":[true],"value_proposition":"vp"}',
    b'{"lead_ids":[null],"value_proposition":"vp"}',
    b'{"lead_ids":["x"],"value_proposition":"vp"}',
    b'{"lead_ids":[1,"y",2.5],"value_proposition":"vp"}',
    b'{"lead_ids":[99999999999999999999],"value_proposition":"vp"}',
    b'{"lead_ids":[-3,0],"value_proposition":"vp"}',
    b'{"lead_ids":[1]}',
    b'{"value_proposition":"vp"}',
    b'{"lead_ids":[1],"value_proposition":5}',
    b'{"lead_ids":[1],"value_proposition":null}',
    b'{"lead_ids":[1],"value_proposition":"vp","tone":5}',
    b'{"lead_ids":[1],"value_proposition":"vp","tone":null}',
    b'{"lead_ids":[1],"value_proposition":"vp","extra_context":null}',
    b'{"lead_ids":[1],"value_proposition":"","tone":""}',
    b'{"lead_ids":[1,2,3,4,5,6,7,8,9,10,11,12],"value_proposition":"vp"}',
    b'{"lead_ids":[1],"value_proposition":"vp","unknown":true}',
    b'{"lead_ids":{"a":1},"value_proposition":"vp"}',
    b'{"lead_ids":["1.0"],"value_proposition":"vp"}',
    b'{"lead_ids":[" 7 "],"value_proposition":"vp"}',
    b'{"lead_ids":[1e2],"value_proposition":"vp"}',
]
req = {"generate_column": [], "campaigns": []}
for key, path, bodies in (("generate_column", "/gc", GC_BODIES), ("campaigns", "/camp", CAMP_BODIES)):
    for b in bodies:
        r = tc.post(path, content=b, headers={"content-type": "application/json"})
        req[key].append({"body": b.decode("utf8", "replace"), "status": r.status_code, "json": r.json()})
dump("requests.json", req)

# ── campaigns ─────────────────────────────────────────────────────────


class FakeDB:
    def __init__(self, leads):
        self.leads = leads
        self.closed = False
        self.asked = []

    def get_lead(self, i):
        self.asked.append(i)
        v = self.leads.get(i)
        if isinstance(v, Exception):
            raise v
        return Lead.from_dict(v) if v is not None else None

    def close(self):
        self.closed = True


class FakeCtx:
    def __init__(self, db):
        self.db = db

    def lead_db(self):
        return self.db


LEADS = {
    1: {"id": 1, "company": "Acme Corp", "email": "a@acme.test", "city": "Pune", "company_size": "51-200", "specialization": "IT Staffing", "description": "D" * 250, "contact_person": "Jane", "glassdoor_rating": "4.2"},
    2: {"id": 2, "company": "NoEmail", "email": ""},
    3: {"id": 3, "company": "Bare", "email": "b@bare.test"},
    4: {"id": 4, "company": "Unicode Müller", "email": "u@u.test", "city": "Zürich", "description": "héllo " * 60, "glassdoor_rating": "0"},
    5: {"id": 5, "company": "Five", "email": "f@f.test", "contact_person": "Bob", "company_size": "1-50"},
    6: {"id": 6, "company": "Six", "email": "s@six.test"},
    99: RuntimeError("db boom"),
}
for i in range(7, 20):
    LEADS[i] = {"id": i, "company": f"Co{i}", "email": f"e{i}@co.test"}

GOOD = {"subject": "Hi there", "body": "Hello body"}
CAMP_CASES = [
    ("single_ok", {"lead_ids": [1], "value_proposition": "We help", "tone": "friendly", "extra_context": "met at event"}, [GOOD]),
    ("tone_unknown_passthrough", {"lead_ids": [1], "value_proposition": "We help", "tone": "sassy"}, [GOOD]),
    ("tone_default", {"lead_ids": [3], "value_proposition": "vp"}, [GOOD]),
    ("tone_direct_consultative", {"lead_ids": [5, 6], "value_proposition": "vp", "tone": "direct"}, [GOOD, GOOD]),
    ("skip_missing_and_no_email", {"lead_ids": [2, 404, 1], "value_proposition": "vp"}, [GOOD]),
    ("llm_empty_skipped", {"lead_ids": [1, 3], "value_proposition": "vp"}, [{}, GOOD]),
    ("llm_missing_body_skipped", {"lead_ids": [1, 3], "value_proposition": "vp"}, [{"subject": "s"}, {"body": "b"}]),
    ("llm_empty_strings_skipped", {"lead_ids": [1], "value_proposition": "vp"}, [{"subject": "", "body": "x"}]),
    ("llm_non_string_values", {"lead_ids": [1], "value_proposition": "vp"}, [{"subject": 5, "body": ["x"]}]),
    ("llm_raises_continues", {"lead_ids": [1, 3], "value_proposition": "vp"}, [RuntimeError("x"), GOOD]),
    ("db_raises_continues", {"lead_ids": [99, 3], "value_proposition": "vp"}, [GOOD]),
    ("capped_at_10", {"lead_ids": list(range(7, 20)), "value_proposition": "vp"}, [GOOD] * 13),
    ("cap_counts_skipped", {"lead_ids": [2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 1], "value_proposition": "vp"}, [GOOD]),
    ("duplicates_processed_twice", {"lead_ids": [1, 1], "value_proposition": "vp"}, [GOOD, {"subject": "S2", "body": "B2"}]),
    ("unicode", {"lead_ids": [4], "value_proposition": "Wir helfen", "tone": "professional", "extra_context": "Grüße"}, [{"subject": "Grüß dich", "body": "Hallo Müller"}]),
    ("no_leads", {"lead_ids": [], "value_proposition": "vp"}, []),
]
camp = []
import apps.api.services.leadgen.llm as LLMMOD  # noqa: E402
orig = REAL_LLM.extract_json
for name, body, script in CAMP_CASES:
    fake = FakeLLM(script)
    REAL_LLM.extract_json = fake.extract_json
    db = FakeDB(LEADS)
    req_model = CAMP.GenerateRequest(**body)
    res = asyncio.run(CAMP.generate_outreach(req_model, FakeCtx(db)))
    camp.append({"name": name, "request": body, "llm": [({"__raise__": str(s)} if isinstance(s, Exception) else s) for s in script],
                 "calls": fake.calls, "asked": db.asked, "closed": db.closed, "response": res.model_dump(mode="json")})
REAL_LLM.extract_json = orig
dump("campaigns.json", {"system": CAMP._EMAIL_SYSTEM_PROMPT, "leads": {str(k): (v if not isinstance(v, Exception) else {"__raise__": str(v)}) for k, v in LEADS.items()}, "cases": camp})

# ── provider accuracy ─────────────────────────────────────────────────
from apps.api.services.leadgen.enrichment.eval import scorer as S  # noqa: E402

fields = ["website", "domain", "founding_year", "founded_year", "linkedin_url", "twitter_url", "industry_tags", "description", "company", "email", "x_url"]
values = [None, "", "  ", "HTTPS://WWW.Acme.COM/About/", "www.acme.com", "acme.com/", "https://acme.com", 2020, "Founded in 1999 (est)", "abc", "AeroSpace", "aerospace & defense", "http://x.y/z/", "linkedin.com/company/a/", 0, "ÄÖÜ", "12", ["a"], True]
norm = [{"field": f, "value": v, "norm": S._normalize(f, v)} for f in fields for v in values]
vm_pairs = [("website", "https://www.a.com/x", "a.com"), ("domain", "A.com", "a.com/q"), ("industry_tags", "aerospace & defense", "aerospace"), ("industry_tags", "aero", "defense"), ("description", "Makes widgets", "widgets"),
            ("founding_year", "Est. 1999", 1999), ("company", "Acme", "acme"), ("company", "Acme Inc", "acme"), ("email", "", "a"), ("email", "a", None), ("linkedin_url", "https://www.linkedin.com/in/x/", "linkedin.com/in/x")]
vm = [{"field": f, "got": g, "expected": e, "match": S.values_match(f, g, e)} for f, g, e in vm_pairs]
today = date(2026, 10, 28)
fr = [{"asof": a, "fresh": S._freshness(a, today)} for a in [None, "", "2026-10-28", "2026-10-29", "2025-10-28", "2020-01-01", "1990-01-01", "bad", "2024-02-29", "2026-1-1", "2026-10-28T10:00:00"]]

golden = [{"id": "g1", "expected": {"website": "acme.com", "founding_year": "1999", "description": "widgets"}},
          {"id": "g2", "expected": {"website": "b.io", "industry_tags": "aerospace"}},
          {"id": "g3", "expected": {}}]
outs = {"g1": {"p1": {"fields": {"website": "https://www.acme.com/", "founding_year": "Est 1999", "description": "wrong"}, "asof": "2025-10-28"}, "p2": {"fields": {"website": "x.com"}}, "p3": {"fields": {}}},
        "g2": {"p1": {"fields": {"website": "B.io", "industry_tags": "aerospace & defense"}, "asof": "2020-01-01"}, "p2": {"fields": {"website": None, "industry_tags": ""}}, "p4": None},
        "gX": {"p1": {"fields": {"website": "a"}}},
        "g3": {"p1": {"fields": {"website": "a"}}}}
sc = S.score_outputs(golden, outs, today=today)
synthetic = {"golden": golden, "outputs": outs, "today": "2026-10-28", "ranking": [s.to_api() for s in S.rank_providers(sc)],
             "verdicts": {p: [{"case": v.case_id, "field": v.field, "correct": v.correct, "fresh": v.freshness} for v in s.verdicts] for p, s in sc.items()}}
empty = [s.to_api() for s in S.rank_providers(S.score_outputs([], {}, today=today))]
DATA = os.path.abspath(os.path.join(os.getcwd(), "..", "..", "data", "provider_eval"))  # the repo-root copy (legacy/python/data has none)
GP, RP = os.path.join(DATA, "golden.json"), os.path.join(DATA, "recorded_outputs.json")
real = {}
for d in (date(2026, 10, 28), date(2024, 1, 1), date(2030, 6, 15)):
    real[d.isoformat()] = [s.to_api() for s in S.run_eval(GP, RP, today=d)]
dump("accuracy.json", {"normalize": norm, "values_match": vm, "freshness": fr, "synthetic": synthetic, "empty": empty, "real": real,
                       "decay": S.FRESHNESS_DECAY_PER_YEAR, "weight": S.FRESHNESS_WEIGHT})

# persist semantics on an in-memory SQLite
from sqlalchemy import create_engine  # noqa: E402
from sqlalchemy.orm import sessionmaker  # noqa: E402
from apps.api.services.workbook.planner_models import ProviderStat  # noqa: E402
from apps.api.services.leadgen.enrichment.eval import persist as P  # noqa: E402

eng = create_engine("sqlite://")
ProviderStat.__table__.create(eng)
Sess = sessionmaker(bind=eng)
db = Sess()
db.add(ProviderStat(provider="apollo", field="email", attempts=3, hits=2))
db.add(ProviderStat(provider="apollo", field="phone", attempts=1, hits=0))
db.add(ProviderStat(provider="hunter", field="*", attempts=0, hits=0))
db.commit()
ranking = P.run_and_persist(db, GP, RP, today=today)
db.commit()
rows = [{"provider": r.provider, "field": r.field, "score": r.accuracy_score, "samples": r.accuracy_samples, "attempts": r.attempts, "hits": r.hits,
         "has_updated": r.accuracy_updated_at is not None} for r in db.query(ProviderStat).order_by(ProviderStat.provider, ProviderStat.field).all()]
before = [{"provider": "apollo", "field": "email", "attempts": 3, "hits": 2}, {"provider": "apollo", "field": "phone", "attempts": 1, "hits": 0}, {"provider": "hunter", "field": "*", "attempts": 0, "hits": 0}]
dump("persist.json", {"before": before, "today": "2026-10-28", "ranking": [s.to_api() for s in ranking], "rows": rows})
print("recorded", OUT)
