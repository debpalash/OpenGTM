"""Records the copilot tools ported to Go by plan 15 WP-COPILOT-CORE (result JSON + database side effects).

Run from legacy/python against a PRIVATE, migrated Postgres database (it deletes the recorder workspace's rows).
DATABASE_URL is a non-superuser RLS role (the lead store refuses a superuser); ADMIN_DATABASE_URL (owner) seeds and reads back:
    DATABASE_URL=postgresql+psycopg://app_rls_test:...@.../db ADMIN_DATABASE_URL=postgresql+psycopg://postgres:...@.../db PG_LEAD_STORE=true SQLITE_STORES_BACKEND=postgres SECRET_KEY=x APP_ENV=test \
        TZ=UTC RUN_INLINE_WORKER=0 .venv/bin/python <this file> <outdir>
Writes native_tools.json. Dynamic values are normalized by norm() (Go applies the same rules): uuids, 32-hex ids,
8-hex column suffixes, timestamps and integer queue ids. The AmbitionBox gateway is replaced by a recording fake.
"""
import asyncio
import json
import os
import re
import sys

sys.path.insert(0, os.getcwd())
from sqlalchemy import create_engine, text  # noqa: E402
from sqlalchemy.orm import sessionmaker  # noqa: E402

AdminSession = sessionmaker(bind=create_engine(os.environ["ADMIN_DATABASE_URL"]))

OUT = sys.argv[1]
WS, SLUG = "ws_rec_tools", "rec"
L = 9100000  # explicit lead ids so Go can seed the same rows


def sql(q, **kw):
    with AdminSession() as s:
        r = s.execute(text(q), kw)
        s.commit()
        try:
            return [dict(m) for m in r.mappings().all()]
        except Exception:
            return []


def reset():
    for t in ("workbook_rows", "workbooks", "collection_job_stages", "collection_jobs", "jobs", "leads"):
        sql(f"DELETE FROM {t} WHERE workspace_id = :ws", ws=WS)
    sql("DELETE FROM jobs WHERE payload::text LIKE :like", like=f"%{WS}%")
    sql("DELETE FROM workspaces WHERE id = :ws", ws=WS)
    sql("INSERT INTO workspaces (id, name, slug) VALUES (:ws, :nm, :slug)", ws=WS, nm=WS, slug=SLUG)
    sql("INSERT INTO app_settings (key, value) VALUES ('__collection_ledger_backend', 'postgres') "
        "ON CONFLICT (key) DO UPDATE SET value = 'postgres'")


LEADS = [
    # id, company, city, spec, email, phone, linkedin, contact, website, description, size, score, tier, status, dm, founded, revenue
    (1, "Acme Staffing", "Pune", "IT Staffing", "hr@acme.io", "+91 1", "https://linkedin.com/company/acme", "Ann", "acme.io", "Staffing firm", "51-200", 90, "hot", "new", "[{\"name\": \"Ann\"}]", "2001", "$5M"),
    (2, "Beta Talent", "Pune", "IT Staffing", "", "+91 2", "", "", "beta.io", "", "1-50", 70, "warm", "contacted", "", "", ""),
    (3, "Gamma HR", "Mumbai", "Payroll", "N/A", "", "nan", "Raj", "", "Payroll svc", "", 55, "warm", "new", "", "1999", ""),
    (4, "Delta Works", "Delhi", "Recruitment", "noatsign", "nan", "https://linkedin.com/company/delta", "N/A", "N/A", "", "201-500", 20, "unqualified", "new", "", "", ""),
    (5, "Epsilon", "Mumbai", "IT Staffing", "e@eps.io", "+91 5", "https://linkedin.com/company/eps", "Eve", "eps.io", "d", "500+", 35, "cold", "dead", "[]", "", "$1M"),
    (6, "Zeta Lone", "Goa", "Niche Robotics", None, None, None, None, None, None, None, 40, "cold", "new", None, None, None),
]


def seed_leads():
    for (i, co, city, spec, em, ph, li, cp, web, desc, size, score, tier, st, dm, fy, rev) in LEADS:
        sql("""INSERT INTO leads (id, workspace_id, company, city, specialization, email, phone, linkedin_url, contact_person,
               website, description, company_size, score, score_tier, status, decision_makers, founded_year, revenue_range)
               VALUES (:id, :ws, :co, :city, :spec, :em, :ph, :li, :cp, :web, :desc, :size, :score, :tier, :st, :dm, :fy, :rev)""",
            id=L + i, ws=WS, co=co, city=city, spec=spec, em=em, ph=ph, li=li, cp=cp, web=web, desc=desc, size=size,
            score=score, tier=tier, st=st, dm=dm, fy=fy, rev=rev)


def seed_workbook(wid, refresh=None):
    sql("""INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config, filter_criteria,
           columns_config, total_rows, completed_rows, sync_to_leads, budget_max_usd, budget_spent_usd, refresh_policy)
           VALUES (:id, 'Seed WB', '', 'draft', :ws, 'empty', '{}', '{}',
           '[{"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company"}]', 0, 0, true, 0, 0, :rp)""",
        id=wid, ws=WS, rp=json.dumps(refresh) if refresh is not None else None)


_RULES = [
    (re.compile(r"\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b"), "$UUID"),
    (re.compile(r"\b[0-9a-f]{32}\b"), "$HEX"),
    (re.compile(r"\b(src|agent)_[0-9a-f]{8}\b"), r"\1_$8"),
    (re.compile(r"\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(\+00:00|Z)?"), "$TS"),
    (re.compile(r'"(queue_job_id|source_job_id|job_id)": \d+'), r'"\1": "$ID"'),
    (re.compile(r'("refresh_workbook:[^"]*:policy:)\d+'), r"\1$EPOCH"),
]


def norm(s):
    for rx, rep in _RULES:
        s = rx.sub(rep, s)
    return s


def snapshot(before_job):
    jobs = sql("""SELECT type, workspace_id, payload::text AS payload, fire_key, executor, status, priority, max_retries,
                  next_run_at IS NOT NULL AND next_run_at > now() AT TIME ZONE 'utc' + interval '1 minute' AS delayed
                  FROM jobs WHERE id > :b AND (workspace_id = :ws OR payload::text LIKE :like) ORDER BY id""",
               ws=WS, b=before_job, like=f"%{WS}%")
    wbs = sql("""SELECT name, description, status, source_type, action_idempotency_key, source_config::text AS source_config,
                 filter_criteria::text AS filter_criteria, columns_config::text AS columns_config, total_rows, completed_rows,
                 sync_to_leads, budget_max_usd, budget_spent_usd, refresh_policy::text AS refresh_policy
                 FROM workbooks WHERE workspace_id = :ws ORDER BY name, id""", ws=WS)
    cjs = sql("""SELECT query, intent, intent_details, status FROM collection_jobs WHERE workspace_id = :ws ORDER BY created_at, id""",
              ws=WS)
    return deep_norm({"jobs": jobs, "workbooks": wbs, "collection_jobs": cjs})


def deep_norm(v):
    """norm() on every string value (database text columns hold JSON of their own)."""
    if isinstance(v, str):
        return norm(v)
    if isinstance(v, dict):
        return {k: deep_norm(x) for k, x in v.items()}
    if isinstance(v, list):
        return [deep_norm(x) for x in v]
    return v


# ── AmbitionBox fake (the Go client is parity-tested separately; the tool layer only maps arguments) ──
from apps.api.services.leadgen import ambitionbox as AB  # noqa: E402

AB_CALLS = []


class _Rec:
    def __init__(self, i, d):
        self._d = d

    def as_dict(self):
        return dict(self._d)


class _Coll:
    def __init__(self, n):
        self.records = [_Rec(i, {"id": i, "name": f"Co {i}"}) for i in range(n)]

    def summary(self, include_records=False):
        return {"provider": "ambitionbox", "requested_count": len(self.records), "returned_count": len(self.records),
                "status": "complete"}


async def _search(**kw):
    AB_CALLS.append({"fn": "search_companies", "kw": kw})
    if kw.get("rating") == "boom":
        raise RuntimeError("AmbitionBox API 503: upstream down")
    return {"companies": [{"id": 1, "name": "Co 1"}], "total": 1, "page": kw.get("page"), "source_total": 9,
            "total_pages": 1, "has_more": False}


async def _collect(**kw):
    AB_CALLS.append({"fn": "collect_companies", "kw": kw})
    return _Coll(min(3, kw["requested_count"]))


async def _jobs(**kw):
    AB_CALLS.append({"fn": "get_company_jobs", "kw": kw})
    return {"jobs": [], "total": 0, "page": kw.get("page"), "total_pages": 0}


AB.ambitionbox.search_companies = _search
AB.ambitionbox.collect_companies = _collect
AB.ambitionbox.get_company_jobs = _jobs

from apps.api.core.tenancy import workspace_scope  # noqa: E402
from apps.api.services.copilot import tools as T  # noqa: E402
from apps.api.services.leadgen.store import get_lead_store  # noqa: E402

CASES = [
    ("get_enrichment_gaps", {}),
    ("get_enrichment_gaps", {"missing_field": "email", "min_score": 0}),
    ("get_enrichment_gaps", {"missing_field": "phone", "min_score": 50, "limit": 1}),
    ("get_enrichment_gaps", {"missing_field": "linkedin"}),
    ("get_enrichment_gaps", {"missing_field": "decision_makers", "min_score": 10}),
    ("get_enrichment_gaps", {"missing_field": "website"}),
    ("get_enrichment_gaps", {"min_score": 35.5, "limit": 2}),
    ("compare_leads", {"lead_ids": [L + 1, L + 2]}),
    ("compare_leads", {"lead_ids": [L + 3, 1, L + 4, L + 6]}),
    ("compare_leads", {}),
    ("find_similar_leads", {"lead_id": L + 1}),
    ("find_similar_leads", {"lead_id": L + 2, "limit": 1}),
    ("find_similar_leads", {"lead_id": L + 6}),
    ("find_similar_leads", {"lead_id": 1}),
    ("set_workbook_refresh", {"workbook_id": "wb-rec-1", "interval": "daily"}),
    ("set_workbook_refresh", {"workbook_id": "wb-rec-1", "interval": "weekly"}),
    ("set_workbook_refresh", {"workbook_id": "wb-rec-1", "interval": "fortnightly"}),
    ("set_workbook_refresh", {"workbook_id": "nope", "interval": "daily"}),
    ("add_agent_column", {"workbook_id": "wb-rec-2", "column_name": "CEO", "goal": "Find the CEO", "target_field": "ceo"}),
    ("add_agent_column", {"workbook_id": "wb-rec-2", "column_name": "Fund", "goal": "Funding", "target_field": "funding", "max_cost_usd": 0.5}),
    ("add_agent_column", {"workbook_id": "nope", "column_name": "x", "goal": "y", "target_field": "z"}),
    ("add_signal_trigger", {"workbook_id": "wb-rec-3", "signals": ["funding", "hiring"]}),
    ("add_signal_trigger", {"workbook_id": "nope", "signals": ["funding"]}),
    ("start_collection", {"query": "IT staffing companies in Pune"}),
    ("start_collection", {"query": "Stripe partnerships team"}),
    ("start_collection", {"query": "fintech companies in Berlin", "intent": "market_search"}),
    ("create_source_workbook", {"icp_description": "  SaaS   companies in Berlin  ", "target_rows": 50}),
    ("create_source_workbook", {"icp_description": "SaaS companies in Berlin", "target_rows": 50}),
    ("create_source_workbook", {"icp_description": "Fintech in Paris", "target_rows": 10, "auto_run": False, "name": "Paris"}),
    ("create_source_workbook", {"icp_description": "find 25 fintech companies in Berlin that use Stripe", "account_discovery": True,
                                "target_rows": 3, "auto_enrich": True, "idempotency_key": "chat-accounts:c1:k"}),
    ("create_source_workbook", {"icp_description": "find 25 fintech companies in Berlin that use Stripe", "account_discovery": True,
                                "idempotency_key": "chat-accounts:c1:k"}),
    ("create_source_workbook", {"icp_description": "find 9 robotics companies", "account_discovery": True,
                                "idempotency_key": "chat-accounts:c1:k"}),
    ("create_source_workbook", {"icp_description": "companies in Pune", "account_discovery": True}),
    ("create_source_workbook", {"icp_description": "   "}),
    ("create_source_workbook", {"icp_description": "x", "idempotency_key": "k" * 256}),
    ("create_source_workbook", {"icp_description": "Fintech in Paris", "target_rows": 10, "auto_run": True, "name": "Paris"}),
    ("create_source_workbook", {"icp_description": "Huge", "target_rows": 9000}),
    ("import_ambitionbox_to_workbook", {"industry": "IT Services", "limit": 30}),
    ("import_ambitionbox_to_workbook", {}),
    ("ambitionbox_search", {}),
    ("ambitionbox_search", {"industry": "HR", "limit": 5, "sort_by": "rating", "rating": "4.0", "page": 2}),
    ("ambitionbox_search", {"limit": 45, "industry": "SaaS"}),
    ("ambitionbox_search", {"limit": 500}),
    ("ambitionbox_search", {"limit": 0, "rating": "boom"}),
    ("ambitionbox_jobs", {"company_id": 42}),
    ("ambitionbox_jobs", {"company_id": 42, "page": 3}),
]


async def run_case(name, args):
    with workspace_scope(WS):
        store = get_lead_store(WS, SLUG)
        return await T.execute_tool(name, args, store=store, workspace_id=WS, slug=SLUG, user_id=None)


def main():
    reset()
    seed_leads()
    seed_workbook("wb-rec-1")
    seed_workbook("wb-rec-2")
    seed_workbook("wb-rec-3", {"enabled": False, "interval": "daily"})
    out = []
    for name, args in CASES:
        before = (sql("SELECT coalesce(max(id), 0) AS m FROM jobs")[0]["m"])
        AB_CALLS.clear()
        res = asyncio.run(run_case(name, json.loads(json.dumps(args))))
        out.append({"name": name, "args": args, "result": norm(res), "ambitionbox_calls": list(AB_CALLS),
                    "after": snapshot(before)})
    reset()
    sql("DELETE FROM workspaces WHERE id = :ws", ws=WS)
    with open(os.path.join(OUT, "native_tools.json"), "w") as f:
        json.dump({"workspace_id": WS, "slug": SLUG, "lead_id_base": L, "leads": LEADS, "cases": out}, f, indent=1,
                  ensure_ascii=False)
        f.write("\n")
    print("ok", len(out))


main()
