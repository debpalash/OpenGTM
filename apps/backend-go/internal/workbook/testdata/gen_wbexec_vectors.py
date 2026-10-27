"""Record run_workbook residual-parity vectors (plan 15 WP-WBEXEC) from the REAL Python executor.

Run from legacy/python:
    TZ=UTC PYTHONPATH=. uv run python ../../apps/backend-go/internal/workbook/testdata/gen_wbexec_vectors.py
Writes wbexec_vectors.json next to this file. Never hand-edit the output: every expected value is what
enrichment._run_one_row / enrich_cell / output_attempts produced. Only the inputs are hand-written.

Seams (the Go tests replace the same seams): provider calls (run_provider), the email verify cascade,
the destination clients (CRM, Sheets, Airtable, Instantly, Smartlead, outreach store) and the webhook
HTTP transport; the database is in-memory SQLite with the real models. Clocks are fixed.
"""
import asyncio
import json
import pathlib
from datetime import datetime, timezone
from types import SimpleNamespace

import httpx
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.config import settings
from apps.api.core.tenancy import current_workspace_var
from apps.api.services.leadgen.enrichment import email_verify_cascade as cascade
from apps.api.services.leadgen.enrichment import licenses
from apps.api.services.workbook import enrichment as E
from apps.api.services.workbook import output as O
from apps.api.services.workbook import output_attempts as OA
from apps.api.services.workbook import vendor_catalog
from apps.api.services.workbook.cell_scope import row_execution_data
from apps.api.services.workbook.models import Workbook, WorkbookEnrichment, WorkbookRow
from apps.api.services.workbook.planner_models import ProviderStat

HERE = pathlib.Path(__file__).resolve().parent
NOW = datetime(2026, 10, 28, 12, 0, 0, tzinfo=timezone.utc)
NOW_ISO = NOW.isoformat()
WS, WB = "ws-wbexec", "wb-wbexec"

engine = create_engine("sqlite://", poolclass=StaticPool, connect_args={"check_same_thread": False})
for t in (Workbook, WorkbookRow, WorkbookEnrichment, ProviderStat):
    t.__table__.create(engine)
Session = sessionmaker(bind=engine)
E.SessionLocal = Session
current_workspace_var.set(WS)


class FixedDatetime(datetime):
    @classmethod
    def now(cls, tz=None):
        return NOW if tz else NOW.replace(tzinfo=None)


E.datetime = FixedDatetime
licenses._now_iso = lambda: NOW_ISO

# ── seams ─────────────────────────────────────────────────────────────────
STATE = {}


def reset_state(sc):
    STATE.clear()
    STATE.update(broadcasts=[], provider_calls=[], writebacks=[], verify_calls=[], output_calls=[], emitted=[],
                 scenario=sc, leads={})


async def fake_broadcast(_redis, workbook_id, msg):
    STATE["broadcasts"].append(msg)


E._broadcast = fake_broadcast


async def fake_run_provider(name, lead, timeout=10.0):
    STATE["provider_calls"].append(name)
    r = STATE["scenario"].get("providers", {}).get(name)
    if r is None:
        return None
    return {"provider": name, "success": r.get("success", True), "fields": dict(r.get("fields", {})),
            "confidence": r.get("confidence", 0.0), "error": r.get("error", ""), "license": "unknown"}


E.run_provider = fake_run_provider
E.get_provider = lambda name: SimpleNamespace(name=name, default_confidence=0.5) \
    if name in STATE["scenario"].get("providers", {}) else None


async def fake_verify(email, workspace_id=None):
    STATE["verify_calls"].append({"email": email, "workspace_id": workspace_id})
    v = STATE["scenario"].get("verify")
    if v == "raise":
        raise RuntimeError("verifier down")
    return cascade.VerifyResult(email=email, status=v["status"], source=v["source"], confidence=v["confidence"])


cascade.verify_email = fake_verify


class FakeLeadStore:
    def get_lead(self, lead_id):
        fp = STATE["scenario"].get("lead_field_provenance", {}).get(str(lead_id))
        return SimpleNamespace(field_provenance=fp) if fp is not None else None

    def update_lead_fields(self, lead_id, updates):
        STATE["writebacks"].append({"lead_id": lead_id, "updates": updates})

    def close(self):
        pass


import apps.api.services.leadgen.store as lead_store_mod  # noqa: E402
import apps.api.services.workspace.manager as ws_manager  # noqa: E402

lead_store_mod.get_lead_store = lambda ws, slug: FakeLeadStore()
ws_manager.workspace_slug = lambda ws: "slug"


def capture_events(workspace_id, workbook_id, changes, **_):
    STATE["emitted"].append({"workspace_id": workspace_id, "workbook_id": workbook_id, "changes": changes})
    return len(changes)


import apps.api.services.automations.events as auto_events  # noqa: E402

auto_events.on_rows_changed = capture_events


# Destination clients: record the call, answer from the scenario script.
def dest_answer(kind):
    return dict(STATE["scenario"]["dest"].get(kind, {"success": True}))


async def fake_push_record(fields, base_id, table, typecast=True, workspace_id=None):
    STATE["output_calls"].append({"kind": "airtable", "fields": fields, "base_id": base_id, "table": table,
                                  "typecast": typecast, "workspace_id": workspace_id})
    return dest_answer("airtable")


async def fake_append_row(spreadsheet_id, values, sheet_range="Sheet1", workspace_id=None):
    STATE["output_calls"].append({"kind": "sheets", "spreadsheet_id": spreadsheet_id, "values": values,
                                  "range": sheet_range, "workspace_id": workspace_id})
    return dest_answer("sheets")


async def fake_instantly(campaign_id, lead, skip_if_in_campaign=True, workspace_id=None):
    STATE["output_calls"].append({"kind": "instantly", "campaign_id": campaign_id, "lead": lead,
                                  "skip_if_in_campaign": skip_if_in_campaign, "workspace_id": workspace_id})
    return dest_answer("instantly")


async def fake_smartlead(campaign_id, lead, settings=None, workspace_id=None):
    STATE["output_calls"].append({"kind": "smartlead", "campaign_id": campaign_id, "lead": lead,
                                  "settings": settings, "workspace_id": workspace_id})
    return dest_answer("smartlead")


import apps.api.services.integrations.airtable as airtable  # noqa: E402
import apps.api.services.integrations.instantly as instantly  # noqa: E402
import apps.api.services.integrations.sheets as sheets  # noqa: E402
import apps.api.services.integrations.smartlead as smartlead  # noqa: E402

airtable.push_record = fake_push_record
sheets.append_row = fake_append_row
instantly.add_lead_to_campaign = fake_instantly
smartlead.add_lead_to_campaign = fake_smartlead


async def fake_push_crm(cfg, lead_data, workspace_id, workbook_id=None, lead_id=None):
    STATE["output_calls"].append({"kind": "crm", "cfg": cfg, "workspace_id": workspace_id,
                                  "workbook_id": workbook_id, "lead_id": lead_id,
                                  "lead_email": lead_data.get("email")})
    return dest_answer("crm")


O._push_crm = fake_push_crm


class FakeOutreachStore:
    def sequence_exists(self, seq_id):
        return STATE["scenario"]["dest"]["sequencer"].get("exists", True)

    def is_suppressed(self, email):
        return STATE["scenario"]["dest"]["sequencer"].get("suppressed", False)

    def enroll(self, seq_id, lead_id, email, consent_source=None):
        STATE["output_calls"].append({"kind": "sequencer", "sequence_id": seq_id, "lead_id": lead_id,
                                      "email": email, "consent_source": consent_source})
        return 7 if not STATE["scenario"]["dest"]["sequencer"].get("already", False) else None


import apps.api.services.outreach.store as outreach_store  # noqa: E402

outreach_store.get_outreach_store = lambda ws: FakeOutreachStore()

O._is_safe_public_url = lambda url: (True, "") if "blocked" not in url else (False, "host resolves to non-public address 10.0.0.1")
_RealAsyncClient = httpx.AsyncClient


def webhook_handler(request: httpx.Request):
    body = request.content.decode()
    STATE["output_calls"].append({"kind": "webhook", "method": request.method, "url": str(request.url),
                                  "headers": {k: v for k, v in request.headers.items()
                                              if k.lower() not in ("host", "accept", "accept-encoding", "connection",
                                                                   "user-agent", "content-length")},
                                  "body": body})
    a = STATE["scenario"]["dest"].get("webhook", {"status": 200, "text": "ok"})
    return httpx.Response(a["status"], text=a.get("text", ""))


O.httpx = SimpleNamespace(AsyncClient=lambda **kw: _RealAsyncClient(transport=httpx.MockTransport(webhook_handler), **kw))

for name in ("website_scraper", "gleif", "mailscout", "decision_maker", "acme_free"):
    assert not vendor_catalog.is_paid(name), name


# ── runner ────────────────────────────────────────────────────────────────

def seed(sc):
    with Session() as db:
        db.query(WorkbookEnrichment).delete()
        db.query(WorkbookRow).delete()
        db.query(Workbook).delete()
        db.add(Workbook(id=WB, name="wb", workspace_id=WS, status="running", columns_config=sc["columns"],
                        budget_max_usd=0.0, budget_spent_usd=0.0))
        r = sc["row"]
        db.add(WorkbookRow(id=r["id"], workbook_id=WB, workspace_id=WS, position=0, data=r["data"],
                           enrichments=r.get("enrichments", {}), lead_id=r.get("lead_id")))
        for p in sc.get("prior_enrichments", []):
            db.add(WorkbookEnrichment(workbook_id=WB, workspace_id=WS, **p))
        db.commit()


def snapshot():
    with Session() as db:
        row = db.query(WorkbookRow).one()
        cells = [{"lead_id": e.lead_id, "column_id": e.column_id, "value": e.value, "status": e.status,
                  "provider": e.provider, "error": e.error, "cell_metadata": e.cell_metadata}
                 for e in db.query(WorkbookEnrichment).order_by(WorkbookEnrichment.column_id).all()]
        return row.enrichments, row.data, cells


def run_scenario(sc):
    reset_state(sc)
    settings.PROVENANCE_TRACKING_ENABLED = sc.get("provenance", False)
    settings.AUTOMATIONS_ENABLED = sc.get("automations", False)
    seed(sc)
    with Session() as db:
        row = db.query(WorkbookRow).one()
        lead = row_execution_data(row, sc["columns"])
    cols = [c for c in sc["columns"] if c.get("type") in E.ENRICHMENT_COL_TYPES]
    from apps.api.services.workbook.column_deps import topo_sort_columns
    ordered = topo_sort_columns(cols)

    async def go():
        if sc.get("flush") == "gather":
            # The real run: rows run under asyncio.gather (one Task, one copied context, per row),
            # so the parent's flush sees nothing a row queued.
            res = (await asyncio.gather(E._run_one_row(WB, lead, ordered, sc["columns"], object())))[0]
        else:
            res = await E._run_one_row(WB, lead, ordered, sc["columns"], object(), force=sc.get("force", False))
        E.flush_row_change_emits(WS)
        return res

    E._pending_row_changes.set(None)
    res = asyncio.run(go())
    overlay, data, cells = snapshot()
    return {**sc, "expect": {"row_result": res, "overlay": overlay, "row_data": data, "enrichments": cells,
                             "broadcasts": STATE["broadcasts"], "provider_calls": STATE["provider_calls"],
                             "writebacks": STATE["writebacks"], "verify_calls": STATE["verify_calls"],
                             "output_calls": STATE["output_calls"], "emitted": STATE["emitted"]}}


def wf(cid, target, chain, **kw):
    return {"id": cid, "name": kw.pop("name", "Col " + cid), "type": "waterfall", "target_field": target,
            "waterfall": chain, **kw}


def out(cid, dest, cfg, **kw):
    return {"id": cid, "name": "Out " + cid, "type": "output", "destination": dest, "destination_config": cfg, **kw}


ROW = {"id": 11, "lead_id": 501, "data": {"company": "Acme", "website": "acme.example.com", "email": "jo@acme.example.com",
                                          "first_name": "Jo", "last_name": "Doe", "city": "Paris"}}
ROW_NOLEAD = {"id": 12, "lead_id": None, "data": {"company": "Beta", "website": "beta.example.com"}}
PHONE = {"website_scraper": {"fields": {"phone": "+33 1 23", "city": "Lyon", "company_size": "11-50"}, "confidence": 0.8}}

SCENARIOS = [
    # ── provenance_on ──
    {"name": "provenance_off_baseline", "columns": [wf("ph", "phone", ["website_scraper"])], "row": ROW, "providers": PHONE},
    {"name": "provenance_waterfall", "provenance": True, "columns": [wf("ph", "phone", ["website_scraper"])], "row": ROW,
     "providers": PHONE, "lead_field_provenance": {"501": '{"email": {"source": "csv", "license": "user-provided", "confidence": null, "fetched_at": "x"}, "phone": {"source": "old"}}'}},
    {"name": "provenance_zero_confidence_uses_default", "provenance": True, "columns": [wf("ph", "phone", ["gleif"])], "row": ROW,
     "providers": {"gleif": {"fields": {"phone": "+44"}, "confidence": 0.0}}},
    {"name": "provenance_no_lead_row", "provenance": True, "columns": [wf("ph", "phone", ["website_scraper"])], "row": ROW_NOLEAD,
     "providers": PHONE},
    {"name": "provenance_error_cell", "provenance": True, "columns": [wf("ph", "phone", ["website_scraper"])], "row": ROW,
     "providers": {"website_scraper": {"success": False, "fields": {}, "error": "nope"}}},
    {"name": "provenance_formula_column", "provenance": True,
     "columns": [{"id": "f", "name": "F", "type": "formula", "formula": "{company} + '!'"}], "row": ROW},
    {"name": "provenance_malformed_existing", "provenance": True, "columns": [wf("ph", "phone", ["website_scraper"])], "row": ROW,
     "providers": PHONE, "lead_field_provenance": {"501": "not json"}},
    # ── automations_on ──
    {"name": "automations_same_task_flush", "automations": True,
     "columns": [wf("ph", "phone", ["website_scraper"])], "row": {**ROW, "enrichments": {"ph": {"value": "+1 old", "status": "complete"}}},
     "providers": PHONE},
    {"name": "automations_gather_flush_python_loses_deltas", "automations": True, "flush": "gather",
     "columns": [wf("ph", "phone", ["website_scraper"])], "row": ROW, "providers": PHONE},
    {"name": "automations_unchanged_value_no_delta", "automations": True,
     "columns": [wf("ph", "phone", ["website_scraper"])], "row": {**ROW, "enrichments": {"ph": {"value": "+33 1 23", "status": "complete"}}},
     "providers": PHONE},
    # ── condition ──
    {"name": "condition_false_skips", "columns": [wf("ph", "phone", ["website_scraper"], condition='{email} == ""')], "row": ROW, "providers": PHONE},
    {"name": "condition_true_runs", "columns": [wf("ph", "phone", ["website_scraper"], condition='{company} != "" AND {city} == "Paris"')], "row": ROW, "providers": PHONE},
    {"name": "condition_by_display_name", "columns": [{"id": "c1", "name": "Company Name", "type": "lead_field", "lead_field": "company"},
                                                      wf("ph", "phone", ["website_scraper"], condition='{Company Name} contains acme')], "row": ROW, "providers": PHONE},
    {"name": "condition_unparseable_fails_closed", "columns": [wf("ph", "phone", ["website_scraper"], condition="garbage")], "row": ROW, "providers": PHONE},
    {"name": "condition_skip_blocks_dependent", "columns": [wf("ph", "phone", ["website_scraper"], condition="never"),
                                                            {"id": "f", "name": "F", "type": "formula", "formula": "{ph} + 'x'"}], "row": ROW, "providers": PHONE},
    # ── email_verify ──
    {"name": "email_verify_valid", "columns": [wf("em", "email", ["mailscout"])], "row": ROW_NOLEAD,
     "providers": {"mailscout": {"fields": {"email": "a@beta.example.com"}, "confidence": 0.9}},
     "verify": {"status": "valid", "source": "smtp", "confidence": 0.95}},
    {"name": "email_verify_raises_is_best_effort", "columns": [wf("em", "email", ["mailscout"])], "row": ROW_NOLEAD,
     "providers": {"mailscout": {"fields": {"email": "a@beta.example.com"}, "confidence": 0.9}}, "verify": "raise"},
    {"name": "email_verify_false_skips_cascade", "columns": [wf("em", "email", ["mailscout"], verify=False)], "row": ROW_NOLEAD,
     "providers": {"mailscout": {"fields": {"email": "a@beta.example.com"}, "confidence": 0.9}},
     "verify": {"status": "valid", "source": "smtp", "confidence": 0.95}},
    {"name": "email_verify_no_value_no_cascade", "columns": [wf("em", "email", ["mailscout"])], "row": ROW_NOLEAD,
     "providers": {"mailscout": {"success": False, "fields": {}}}, "verify": {"status": "valid", "source": "smtp", "confidence": 0.95}},
    {"name": "email_verify_id_only_target_not_verified", "columns": [{"id": "email", "name": "Email2", "type": "waterfall", "waterfall": ["mailscout"]}],
     "row": ROW_NOLEAD, "providers": {"mailscout": {"fields": {"email": "a@beta.example.com"}, "confidence": 0.9}},
     "verify": {"status": "valid", "source": "smtp", "confidence": 0.95}},
    {"name": "email_verify_with_provenance", "provenance": True, "columns": [wf("em", "email", ["mailscout"])], "row": ROW_NOLEAD,
     "providers": {"mailscout": {"fields": {"email": "a@beta.example.com"}, "confidence": 0.9}},
     "verify": {"status": "catch_all", "source": "reacher", "confidence": 0.6}},
    # ── structured_target ──
    {"name": "structured_target_summary_and_writeback", "columns": [wf("dm", "decision_makers", ["decision_maker"])], "row": ROW,
     "providers": {"decision_maker": {"fields": {"decision_makers": '[{"name": "Ann", "title": "CEO"}, {"name": "Bob"}]', "city": "Nice"}, "confidence": 0.7}}},
    {"name": "structured_target_no_name", "columns": [wf("dm", "decision_makers", ["decision_maker"])], "row": ROW,
     "providers": {"decision_maker": {"fields": {"decision_makers": '[{"title": "CEO"}]'}, "confidence": 0.7}}},
    # ── output columns ──
    {"name": "output_webhook_default_body", "columns": [out("o", "webhook", {"url": "https://hooks.example.com/{company}"})], "row": ROW,
     "dest": {"webhook": {"status": 200}}},
    {"name": "output_webhook_template_body", "columns": [out("o", "webhook", {"url": "https://hooks.example.com/x", "method": "put",
                                                                              "headers": {"X-Co": "{company}"},
                                                                              "body": '{"co": "{company}", "n": 1, "tags": ["{city}"]}'})],
     "row": ROW, "dest": {"webhook": {"status": 201}}},
    {"name": "output_webhook_text_body", "columns": [out("o", "webhook", {"url": "https://hooks.example.com/x", "body": "hello {company}"})],
     "row": ROW, "dest": {"webhook": {"status": 200}}},
    {"name": "output_webhook_get_no_body", "columns": [out("o", "webhook", {"url": "https://hooks.example.com/x", "method": "GET"})],
     "row": ROW, "dest": {"webhook": {"status": 204}}},
    {"name": "output_webhook_http_500", "columns": [out("o", "webhook", {"url": "https://hooks.example.com/x"})], "row": ROW,
     "dest": {"webhook": {"status": 500, "text": "boom " + "x" * 200}}},
    {"name": "output_webhook_blocked", "columns": [out("o", "webhook", {"url": "https://blocked.example.com/x"})], "row": ROW, "dest": {}},
    {"name": "output_webhook_no_url", "columns": [out("o", "webhook", {"url": "  "})], "row": ROW, "dest": {}},
    {"name": "output_webhook_unknown_reference", "columns": [out("o", "webhook", {"url": "https://hooks.example.com/{nope}"})], "row": ROW, "dest": {}},
    {"name": "output_crm_ok", "columns": [out("o", "crm", {"type": "hubspot"})], "row": ROW,
     "dest": {"crm": {"success": True, "value": "HubSpot: created 77", "error": None}}},
    {"name": "output_crm_fail", "columns": [out("o", "crm", {"type": "hubspot"})], "row": ROW,
     "dest": {"crm": {"success": False, "value": "", "error": "HubSpot not connected"}}},
    {"name": "output_sheets_defaults", "columns": [out("o", "sheets", {"spreadsheet_id": "sp1"})], "row": ROW,
     "dest": {"sheets": {"success": True, "range": "Sheet1!A2:E2"}}},
    {"name": "output_sheets_columns_fail", "columns": [out("o", "sheets", {"spreadsheet_id": "sp1", "columns": ["company", "missing"], "range": "Leads"})],
     "row": ROW, "dest": {"sheets": {"success": False, "error": "Google Sheets not connected"}}},
    {"name": "output_airtable_defaults", "columns": [out("o", "airtable", {"base_id": "b1", "table": "T"})], "row": ROW,
     "dest": {"airtable": {"success": True, "record_id": "rec9"}}},
    {"name": "output_airtable_fieldmap_no_record_id", "columns": [out("o", "airtable", {"base_id": "b1", "table": "T", "field_map": {"company": "Co", "missing": "M"}})],
     "row": ROW, "dest": {"airtable": {"success": True}}},
    {"name": "output_instantly_added", "columns": [out("o", "instantly", {"campaign_id": " c1 "})], "row": ROW,
     "dest": {"instantly": {"success": True}}},
    {"name": "output_instantly_duplicate_template_map", "columns": [out("o", "instantly", {"campaign": "c2", "skip_if_in_campaign": False,
                                                                                         "field_map": {"{first_name} {last_name}": "full_name", "email": "email", "nope": "x"}})],
     "row": ROW, "dest": {"instantly": {"success": True, "duplicate": True}}},
    {"name": "output_instantly_no_campaign", "columns": [out("o", "instantly", {})], "row": ROW, "dest": {}},
    {"name": "output_smartlead_settings", "columns": [out("o", "smartlead", {"campaign_id": "9", "settings": {"ignore_global_block_list": True}})], "row": ROW,
     "dest": {"smartlead": {"success": False, "error": "Smartlead rejected"}}},
    {"name": "output_sequencer_enrolled", "columns": [out("o", "sequencer", {"sequence_id": "seq1"})], "row": ROW, "dest": {"sequencer": {}}},
    {"name": "output_sequencer_suppressed", "columns": [out("o", "sequencer", {"sequence_id": "seq1"})], "row": ROW, "dest": {"sequencer": {"suppressed": True}}},
    {"name": "output_sequencer_already", "columns": [out("o", "sequencer", {"sequence_id": "seq1"})], "row": ROW, "dest": {"sequencer": {"already": True}}},
    {"name": "output_sequencer_missing_sequence", "columns": [out("o", "sequencer", {"sequence_id": "seq1"})], "row": ROW, "dest": {"sequencer": {"exists": False}}},
    {"name": "output_sequencer_no_email", "columns": [out("o", "sequencer", {"sequence_id": "seq1"})], "row": ROW_NOLEAD, "dest": {"sequencer": {}}},
    {"name": "output_unknown_destination", "columns": [out("o", "fax", {})], "row": ROW, "dest": {}},
    {"name": "output_run_once_complete_is_skipped", "columns": [out("o", "webhook", {"url": "https://hooks.example.com/x"})],
     "row": {**ROW, "enrichments": {"o": {"value": "POST 200", "status": "complete", "provider": "webhook"}}}, "dest": {}},
    {"name": "output_run_once_false_resends", "columns": [out("o", "webhook", {"url": "https://hooks.example.com/x"}, run_once=False)],
     "row": {**ROW, "enrichments": {"o": {"value": "POST 200", "status": "complete", "provider": "webhook"}}}, "dest": {"webhook": {"status": 200}}},
    {"name": "output_force_resends", "force": True, "columns": [out("o", "webhook", {"url": "https://hooks.example.com/x"})],
     "row": {**ROW, "enrichments": {"o": {"value": "POST 200", "status": "complete", "provider": "webhook"}}}, "dest": {"webhook": {"status": 200}}},
    {"name": "output_identity_requires_review", "columns": [out("o", "webhook", {"url": "https://hooks.example.com/x"})], "row": ROW,
     "prior_enrichments": [{"lead_id": 501, "column_id": "o", "value": "POST 200", "status": "complete", "provider": "webhook"}], "dest": {}},
    {"name": "output_after_enrichment_with_provenance", "provenance": True,
     "columns": [wf("ph", "phone", ["website_scraper"]), out("o", "webhook", {"url": "https://hooks.example.com/x", "body": {"p": "{ph}"}})],
     "row": ROW, "providers": PHONE, "dest": {"webhook": {"status": 200}}},
]


# ── output journal (queued runs) ──────────────────────────────────────────

def journal_vectors():
    from apps.api.services.workbook import batch_attempts as BA

    payloads = {}

    class FakeJob:
        def __init__(self):
            self.payload = {"workbook_id": WB}

    job = FakeJob()

    class Sess:
        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def commit(self):
            payloads["last"] = json.loads(json.dumps(job.payload))

    OA._owned_job = lambda db, **owner: job
    owner = {"job_id": 1, "workspace_id": WS, "workbook_id": WB, "worker_id": "w", "locked_at": NOW, "lease_token": 1}
    calls = []

    async def execute(**request):
        calls.append(request["col_config"]["id"])
        return {"success": True, "value": "POST 200", "error": None}

    async def execute_fail(**request):
        calls.append(request["col_config"]["id"])
        return {"success": False, "value": "POST 500", "error": "HTTP 500: x"}

    def req(col_id="o", row_id=11, lead_id=501, extra=None):
        return dict(col_config={"id": col_id, "type": "output", **(extra or {})}, lead_data={"__row_id": row_id, "company": "Acme"},
                    columns_config=[], workbook_id=WB, lead_id=lead_id, workspace_id=WS)

    steps = []

    def step(name, coro_fn, **kw):
        token = BA.batch_owner.set(owner)
        try:
            try:
                res = asyncio.run(OA.execute_output_with_journal(Sess, coro_fn, **kw))
                out = {"value": res}
            except ValueError as e:
                out = {"error": "ValueError", "message": str(e)}
        finally:
            BA.batch_owner.reset(token)
        steps.append({"name": name, "request": {"col_id": kw["col_config"]["id"], "row_id": kw["lead_data"].get("__row_id"),
                                                "lead_id": kw["lead_id"], "col_extra": {k: v for k, v in kw["col_config"].items() if k not in ("id", "type")}},
                      "result": out, "calls": list(calls), "attempts": json.loads(json.dumps(job.payload.get("output_attempts", {})))})

    step("first_dispatch_records_receipt", execute, **req())
    step("second_run_reuses_receipt", execute, **req())
    step("changed_contract_is_refused", execute, **req(extra={"destination": "crm"}))
    step("failed_send_records_failed_receipt", execute_fail, **req(col_id="o2"))
    step("failed_receipt_is_reused_not_resent", execute, **req(col_id="o2"))
    # A claim without a receipt (process died mid-send) is never resent.
    attempts = job.payload["output_attempts"]
    digest = attempts["row:11/o"]["contract_hash"]
    job.payload = {**job.payload, "output_attempts": {**attempts, "row:12/o": {"contract_hash": None, "state": "dispatch_claimed"}}}
    r12 = req(row_id=12)
    digest12 = OA.hashlib.sha256(json.dumps(r12, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()).hexdigest()
    job.payload["output_attempts"]["row:12/o"]["contract_hash"] = digest12
    step("dangling_claim_requires_review", execute, **r12)
    step("lead_keyed_subject_without_row_id", execute, **{**req(), "lead_data": {"company": "Acme"}})
    summary = OA.summarize_output_attempts(job.payload.get("output_attempts"))
    return {"steps": steps, "summary": summary, "row11_contract_hash": digest,
            "hash_input_example": {"request": req(), "sha256": digest}}


def main():
    vectors = {"now": NOW_ISO, "workspace_id": WS, "workbook_id": WB,
               "scenarios": [run_scenario(sc) for sc in SCENARIOS], "journal": journal_vectors(),
               "licenses": {"vocab": sorted(licenses.LICENSE_VOCAB), "map": licenses.PROVIDER_LICENSE,
                            "resolve": [{"provider": p, "declared": d, "license": licenses.resolve_license(p, d)}
                                        for p, d in [("gleif", None), ("cache:hunter_io", None), ("nope", None), (None, None), ("", "CC0-1.0"),
                                                     ("hunter_io", "unknown"), ("hunter_io", "bogus"), ("x", "scraped"), ("a:b:gleif", None), ("csv", None)]],
                            "provenance_for": [licenses.provenance_for(*a, **k) for a, k in [
                                (("gleif",), {"confidence": 0.123456}), ((None,), {}), (("x",), {"license": "CC-BY-4.0"}),
                                (("x",), {"license": "nonsense", "declared_license": "scraped"}), (("hunter_io",), {"confidence": 1})]],
                            "merge": [{"current": c, "updates": u, "result": licenses.merge_field_provenance(c, u)} for c, u in [
                                (None, {"a": {"source": "x"}}), ("", {"a": {"x": 1}}), ("[1]", {"a": {}}), ('{"b": 1, "a": 2}', {"a": {"é": "ü"}}),
                                ('{"a": 1.50, "z": [1e2, null, true]}', {"c": {"n": None}}), ("{bad", {"a": {}})]]}}
    out = HERE / "wbexec_vectors.json"
    out.write_text(json.dumps(vectors, indent=1, ensure_ascii=False, sort_keys=False, default=str) + "\n", encoding="utf-8")
    print(f"wrote {out}: {len(vectors['scenarios'])} scenarios, {len(vectors['journal']['steps'])} journal steps")


if __name__ == "__main__":
    main()
