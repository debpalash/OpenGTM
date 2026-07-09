"""CRM import + bidirectional sync — offline tests (SQLite, httpx.MockTransport).

Covers the crm_import source kind (flag-gated by CRM_IMPORT_ENABLED) and the
update-by-external-id write-back in the output column:

  * HubSpot import: cursor paging, default + custom field mapping, the sync key
    (crm_external_id / crm_type / crm_object) stored on every row;
  * Salesforce import: SOQL query + nextRecordsUrl paging, WHERE filter,
    Account.Name -> company mapping;
  * re-run idempotency (dedup on the sync key), limit cap, workspace isolation;
  * flag OFF -> clean refusal, ZERO network calls;
  * missing token / 429 -> clean per-run errors with bounded Retry-After retry;
  * write-back: PATCH with only the mapped fields when the row carries a
    matching external id, 404 -> create fallback that stores the fresh id back
    onto the row, and the untouched legacy create path when there is no id.
"""
import asyncio
import json
import os
import sys

import httpx
import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

os.environ.setdefault("DATABASE_URL", "sqlite:///./data/_pytest.db")

PROJECT_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if PROJECT_ROOT not in sys.path:
    sys.path.insert(0, PROJECT_ROOT)

from apps.api.database import Base  # noqa: E402
from apps.api.core.config import settings  # noqa: E402
from apps.api.services.crm import hubspot, salesforce  # noqa: E402
from apps.api.services.workbook import crm_import as ci  # noqa: E402
from apps.api.services.workbook import source_engine as se  # noqa: E402
from apps.api.services.workbook import output as out  # noqa: E402
from apps.api.services.workbook.models import (  # noqa: E402
    Workbook, WorkbookEnrichment, WorkbookRow,
)

WS_A = "ws-crm-alpha"
WS_B = "ws-crm-beta"


def _run(coro):
    return asyncio.run(coro)


# ── fixtures ──────────────────────────────────────────────────────────────

@pytest.fixture()
def db(monkeypatch):
    """In-memory SQLite with the workbook tables; patch every SessionLocal
    the import/write-back paths touch."""
    eng = create_engine(
        "sqlite://", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(eng, tables=[
        Workbook.__table__, WorkbookRow.__table__, WorkbookEnrichment.__table__,
    ])
    SL = sessionmaker(bind=eng, autoflush=False)
    import apps.api.database as database
    monkeypatch.setattr(database, "SessionLocal", SL, raising=False)
    monkeypatch.setattr(ci, "SessionLocal", SL, raising=False)
    monkeypatch.setattr(se, "SessionLocal", SL, raising=False)
    monkeypatch.setattr(ci, "_make_redis", lambda: None)
    return SL


@pytest.fixture()
def flag_on(monkeypatch):
    monkeypatch.setattr(settings, "CRM_IMPORT_ENABLED", True, raising=False)


@pytest.fixture()
def hubspot_token(monkeypatch):
    monkeypatch.setattr(hubspot, "_get_token", lambda ws=None: "hs-tok")


@pytest.fixture()
def sf_creds(monkeypatch):
    monkeypatch.setattr(
        salesforce, "_creds", lambda ws=None: ("https://sf.example.com", "sf-tok"),
    )


def _patch_transport(monkeypatch, handler):
    """Route every httpx.AsyncClient through a MockTransport (test_http_column
    pattern)."""
    orig = httpx.AsyncClient.__init__

    def init(self, *a, **kw):
        kw["transport"] = httpx.MockTransport(handler)
        orig(self, *a, **kw)

    monkeypatch.setattr(httpx.AsyncClient, "__init__", init)


def _mk_workbook(SL, wb_id="wb1", ws=WS_A, source=None):
    col = {"id": "src1", "name": "CRM", "type": "source"}
    if source:
        col["source"] = source
    with SL() as s:
        s.add(Workbook(id=wb_id, name="W", workspace_id=ws, source_type="empty",
                       columns_config=[col], status="draft"))
        s.commit()
    return "src1"


def _rows(SL, wb_id="wb1"):
    with SL() as s:
        return [
            {"id": r.id, "ws": r.workspace_id, "position": r.position,
             "lead_id": r.lead_id, **(r.data or {})}
            for r in s.query(WorkbookRow).filter(
                WorkbookRow.workbook_id == wb_id
            ).order_by(WorkbookRow.position).all()
        ]


# ── HubSpot pages ─────────────────────────────────────────────────────────

def _hs_contact(cid, email, first="", last="", company="", jobtitle=""):
    return {"id": str(cid), "properties": {
        "email": email, "firstname": first, "lastname": last,
        "company": company, "phone": "", "website": "", "jobtitle": jobtitle,
    }}


def _hubspot_paged_handler(calls):
    """Two pages of contacts: [1,2] then [3] via the `after` cursor."""
    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        assert request.url.host == "api.hubapi.com"
        assert request.url.path == "/crm/v3/objects/contacts"
        assert request.headers["Authorization"] == "Bearer hs-tok"
        if request.url.params.get("after") == "pg2":
            return httpx.Response(200, json={"results": [
                _hs_contact(3, "carol@x.com", "Carol", "Cruz", "Initech", "CTO"),
            ]})
        return httpx.Response(200, json={
            "results": [
                _hs_contact(1, "alice@x.com", "Alice", "Ang", "Acme", "CEO"),
                _hs_contact(2, "bob@x.com", "Bob", "", "Globex", ""),
            ],
            "paging": {"next": {"after": "pg2"}},
        })
    return handler


# ── import: flag gate ─────────────────────────────────────────────────────

def test_flag_off_refuses_cleanly_and_makes_no_calls(db, monkeypatch, hubspot_token):
    monkeypatch.setattr(settings, "CRM_IMPORT_ENABLED", False, raising=False)
    calls = []
    _patch_transport(monkeypatch, _hubspot_paged_handler(calls))
    col_id = _mk_workbook(db, source={"kind": "crm_import", "crm": "hubspot"})

    res = _run(se.materialize_source("wb1", col_id, WS_A))
    assert res["error"] == "crm_import_disabled"
    assert res["added"] == 0
    assert calls == []          # zero network
    assert _rows(db) == []      # zero rows


# ── import: hubspot ───────────────────────────────────────────────────────

def test_hubspot_import_paging_mapping_and_sync_key(db, monkeypatch, flag_on, hubspot_token):
    calls = []
    _patch_transport(monkeypatch, _hubspot_paged_handler(calls))
    col_id = _mk_workbook(db, source={"kind": "crm_import", "crm": "hubspot"})

    res = _run(se.materialize_source("wb1", col_id, WS_A))
    assert res == {"found": 3, "added": 3, "skipped": 0, "crm": "hubspot"}
    assert len(calls) == 2  # two pages

    rows = _rows(db)
    assert [r["crm_external_id"] for r in rows] == ["1", "2", "3"]
    assert all(r["crm_type"] == "hubspot" and r["crm_object"] == "contact" for r in rows)
    assert rows[0]["email"] == "alice@x.com"
    assert rows[0]["contact_person"] == "Alice Ang"      # first+last merged
    assert rows[0]["company"] == "Acme"
    assert rows[0]["contact_title"] == "CEO"             # jobtitle mapped
    assert rows[1]["contact_person"] == "Bob"            # lastname empty
    assert all(r["lead_id"] is None for r in rows)

    with db() as s:
        wb = s.get(Workbook, "wb1")
        assert wb.status == "draft"
        assert wb.total_rows == 3


def test_hubspot_import_rerun_dedups_on_external_id(db, monkeypatch, flag_on, hubspot_token):
    _patch_transport(monkeypatch, _hubspot_paged_handler([]))
    col_id = _mk_workbook(db, source={"kind": "crm_import", "crm": "hubspot"})

    first = _run(se.materialize_source("wb1", col_id, WS_A))
    again = _run(se.materialize_source("wb1", col_id, WS_A))
    assert first["added"] == 3
    assert again == {"found": 3, "added": 0, "skipped": 3, "crm": "hubspot"}
    assert len(_rows(db)) == 3


def test_hubspot_import_custom_field_map_and_limit(db, monkeypatch, flag_on, hubspot_token):
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["props"] = request.url.params.get("properties")
        seen["limit"] = request.url.params.get("limit")
        return httpx.Response(200, json={
            "results": [{"id": "9", "properties": {"email": "z@x.com", "hs_tier": "gold"}}],
            "paging": {"next": {"after": "more"}},  # limit must stop the loop
        })

    _patch_transport(monkeypatch, handler)
    col_id = _mk_workbook(db, source={
        "kind": "crm_import", "crm": "hubspot", "limit": 1,
        "field_map": {"email": "email", "hs_tier": "tier"},
    })
    res = _run(se.materialize_source("wb1", col_id, WS_A))
    assert res["added"] == 1
    assert seen["props"] == "email,hs_tier"   # properties come from field_map
    assert seen["limit"] == "1"
    row = _rows(db)[0]
    assert row["tier"] == "gold"              # custom lead field kept
    assert row["crm_external_id"] == "9"


def test_limit_cap():
    assert ci.effective_limit({}) == 500
    assert ci.effective_limit({"limit": None}) == 500
    assert ci.effective_limit({"limit": 7000}) == 5000
    assert ci.effective_limit({"limit": "25"}) == 25
    assert ci.effective_limit({"limit": -3}) == 1


def test_missing_token_is_clean_per_run_error(db, monkeypatch, flag_on):
    monkeypatch.setattr(hubspot, "_get_token", lambda ws=None: "")
    _patch_transport(monkeypatch, lambda r: httpx.Response(500))
    col_id = _mk_workbook(db, source={"kind": "crm_import", "crm": "hubspot"})

    res = _run(se.materialize_source("wb1", col_id, WS_A))
    assert "No HubSpot token" in res["error"]
    assert res["added"] == 0
    with db() as s:
        assert s.get(Workbook, "wb1").status == "draft"  # never stuck running


def test_expired_token_is_clean_per_run_error(db, monkeypatch, flag_on, hubspot_token):
    _patch_transport(monkeypatch, lambda r: httpx.Response(401, text="expired"))
    col_id = _mk_workbook(db, source={"kind": "crm_import", "crm": "hubspot"})
    res = _run(se.materialize_source("wb1", col_id, WS_A))
    assert "invalid or expired" in res["error"]
    assert res["added"] == 0


def test_429_respects_retry_after_bounded(db, monkeypatch, flag_on, hubspot_token):
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        if len(calls) == 1:
            return httpx.Response(429, headers={"Retry-After": "0"})
        return httpx.Response(200, json={"results": [
            _hs_contact(1, "a@x.com", "A", "B", "Acme", ""),
        ]})

    _patch_transport(monkeypatch, handler)
    col_id = _mk_workbook(db, source={"kind": "crm_import", "crm": "hubspot"})
    res = _run(se.materialize_source("wb1", col_id, WS_A))
    assert res["added"] == 1
    assert len(calls) == 2  # one retry, then success


def test_429_gives_up_after_bounded_retries(db, monkeypatch, flag_on, hubspot_token):
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(429, headers={"Retry-After": "0"}, text="slow down")

    _patch_transport(monkeypatch, handler)
    col_id = _mk_workbook(db, source={"kind": "crm_import", "crm": "hubspot"})
    res = _run(se.materialize_source("wb1", col_id, WS_A))
    assert "429" in res["error"]
    assert len(calls) == 4  # initial + 3 bounded retries, then a clean error


# ── import: salesforce ────────────────────────────────────────────────────

def _sf_record(sid, email, first, last, account, title):
    return {"Id": sid, "Email": email, "FirstName": first, "LastName": last,
            "Account": {"Name": account} if account else None,
            "Phone": "", "Title": title}


def test_salesforce_import_paging_filter_and_mapping(db, monkeypatch, flag_on, sf_creds):
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.host == "sf.example.com"
        assert request.headers["Authorization"] == "Bearer sf-tok"
        if request.url.path.endswith("/query") and "q" in request.url.params:
            seen["soql"] = request.url.params["q"]
            return httpx.Response(200, json={
                "done": False,
                "nextRecordsUrl": "/services/data/v59.0/query/next-1",
                "records": [_sf_record("003A", "ann@x.com", "Ann", "Ash", "Acme", "VP")],
            })
        assert request.url.path == "/services/data/v59.0/query/next-1"
        return httpx.Response(200, json={
            "done": True,
            "records": [_sf_record("003B", "bo@x.com", "Bo", "", None, "")],
        })

    _patch_transport(monkeypatch, handler)
    col_id = _mk_workbook(db, source={
        "kind": "crm_import", "crm": "salesforce", "filter": "Email != null",
    })
    res = _run(se.materialize_source("wb1", col_id, WS_A))
    assert res == {"found": 2, "added": 2, "skipped": 0, "crm": "salesforce"}
    assert seen["soql"].startswith(
        "SELECT Id, Email, FirstName, LastName, Account.Name, Phone, Title FROM Contact"
    )
    assert "WHERE Email != null" in seen["soql"]

    rows = _rows(db)
    assert [r["crm_external_id"] for r in rows] == ["003A", "003B"]
    assert all(r["crm_type"] == "salesforce" and r["crm_object"] == "contact" for r in rows)
    assert rows[0]["company"] == "Acme"                # Account.Name dotted path
    assert rows[0]["contact_person"] == "Ann Ash"
    assert rows[0]["contact_title"] == "VP"
    assert "company" not in rows[1]                    # null Account dropped


def test_unsupported_crm_and_object_are_clean_errors(db, flag_on, monkeypatch):
    _patch_transport(monkeypatch, lambda r: httpx.Response(500))
    col_id = _mk_workbook(db, source={"kind": "crm_import", "crm": "pipedrive"})
    res = _run(se.materialize_source("wb1", col_id, WS_A))
    assert "unsupported crm" in res["error"]

    col2 = _mk_workbook(db, wb_id="wb2",
                        source={"kind": "crm_import", "crm": "hubspot", "object": "deal"})
    res2 = _run(se.materialize_source("wb2", col2, WS_A))
    assert "unsupported crm object" in res2["error"]


# ── import: workspace isolation ───────────────────────────────────────────

def test_workspace_isolation(db, monkeypatch, flag_on, hubspot_token):
    _patch_transport(monkeypatch, _hubspot_paged_handler([]))
    src = {"kind": "crm_import", "crm": "hubspot"}
    col_a = _mk_workbook(db, wb_id="wbA", ws=WS_A, source=src)
    col_b = _mk_workbook(db, wb_id="wbB", ws=WS_B, source=src)

    res_a = _run(se.materialize_source("wbA", col_a, WS_A))
    assert res_a["added"] == 3
    # Tenant A's rows are stamped with A; B's workbook untouched.
    assert all(r["ws"] == WS_A for r in _rows(db, "wbA"))
    assert _rows(db, "wbB") == []

    # The same contacts import independently into tenant B (dedup is
    # per-workbook, never cross-tenant).
    res_b = _run(se.materialize_source("wbB", col_b, WS_B))
    assert res_b["added"] == 3
    assert all(r["ws"] == WS_B for r in _rows(db, "wbB"))
    assert len(_rows(db, "wbA")) == 3


# ── write-back: update by external id ────────────────────────────────────

def test_writeback_hubspot_update_patches_mapped_fields_only(monkeypatch, hubspot_token):
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["method"] = request.method
        captured["path"] = request.url.path
        captured["body"] = json.loads(request.content)
        return httpx.Response(200, json={"id": "42"})

    _patch_transport(monkeypatch, handler)
    lead_data = {
        "id": 7, "crm_external_id": "42", "crm_type": "hubspot",
        "email": "new@x.com", "phone": "+1555", "company": "Acme",
        "some_enrichment_col": "should never be sent",
    }
    cfg = {"type": "hubspot", "field_map": {"email": "email", "phone": "phone"}}
    res = _run(out._push_crm(cfg, lead_data, WS_A, workbook_id="wb1", lead_id=7))

    assert res["success"] is True
    assert res["value"] == "HubSpot: updated 42"
    assert captured["method"] == "PATCH"
    assert captured["path"] == "/crm/v3/objects/contacts/42"
    # ONLY the mapped fields — nothing else from the row leaks to the CRM.
    assert captured["body"] == {"properties": {"email": "new@x.com", "phone": "+1555"}}


def test_writeback_salesforce_update_patches_contact(monkeypatch, sf_creds):
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["method"] = request.method
        captured["path"] = request.url.path
        captured["body"] = json.loads(request.content)
        return httpx.Response(204)

    _patch_transport(monkeypatch, handler)
    lead_data = {
        "id": 3, "crm_external_id": "003ABC", "crm_type": "salesforce",
        "crm_object": "contact", "email": "s@x.com", "phone": "+2",
        "contact_title": "CTO", "company": "Globex",
    }
    res = _run(out._push_crm({"type": "salesforce"}, lead_data, WS_A,
                             workbook_id="wb1", lead_id=3))
    assert res["success"] is True
    assert res["value"] == "Salesforce: updated 003ABC"
    assert captured["method"] == "PATCH"
    assert captured["path"] == "/services/data/v59.0/sobjects/Contact/003ABC"
    # Default Contact update map: Email/Phone/Title — Company is NOT a writable
    # Contact field and must not be sent.
    assert captured["body"] == {"Email": "s@x.com", "Phone": "+2", "Title": "CTO"}


def test_writeback_404_falls_back_to_create_and_stores_new_id(db, monkeypatch, hubspot_token):
    # Seed a crm_import row whose HubSpot contact was deleted upstream.
    with db() as s:
        s.add(Workbook(id="wb1", name="W", workspace_id=WS_A, source_type="empty",
                       columns_config=[], status="draft"))
        row = WorkbookRow(
            workbook_id="wb1", workspace_id=WS_A, position=1, lead_id=None,
            data={"email": "gone@x.com", "company": "Acme",
                  "crm_external_id": "42", "crm_type": "hubspot",
                  "crm_object": "contact"},
            enrichments={},
        )
        s.add(row)
        s.commit()
        row_id = row.id

    order = []

    def handler(request: httpx.Request) -> httpx.Response:
        order.append((request.method, request.url.path))
        if request.method == "PATCH":
            return httpx.Response(404, json={"status": "error"})
        if request.url.path == "/crm/v3/objects/contacts/search":
            return httpx.Response(200, json={"results": []})
        assert request.url.path == "/crm/v3/objects/contacts"
        return httpx.Response(201, json={"id": "777"})

    _patch_transport(monkeypatch, handler)
    lead_data = {"id": row_id, "email": "gone@x.com", "company": "Acme",
                 "crm_external_id": "42", "crm_type": "hubspot",
                 "crm_object": "contact"}
    res = _run(out._push_crm({"type": "hubspot"}, lead_data, WS_A,
                             workbook_id="wb1", lead_id=row_id))

    assert res["success"] is True
    assert "created 777" in res["value"]
    assert order[0] == ("PATCH", "/crm/v3/objects/contacts/42")
    assert ("POST", "/crm/v3/objects/contacts") in order

    # The fresh id is stored back onto the row — later runs UPDATE again.
    with db() as s:
        data = s.get(WorkbookRow, row_id).data
        assert data["crm_external_id"] == "777"
        assert data["crm_type"] == "hubspot"
        assert data["crm_object"] == "contact"


def test_writeback_no_external_id_uses_legacy_create_path(monkeypatch, hubspot_token):
    order = []

    def handler(request: httpx.Request) -> httpx.Response:
        order.append((request.method, request.url.path))
        if request.url.path == "/crm/v3/objects/contacts/search":
            return httpx.Response(200, json={"results": []})
        return httpx.Response(201, json={"id": "55"})

    _patch_transport(monkeypatch, handler)
    lead_data = {"id": 1, "email": "a@x.com", "company": "Acme"}
    res = _run(out._push_crm({"type": "hubspot"}, lead_data, WS_A,
                             workbook_id="wb1", lead_id=1))
    assert res["success"] is True
    # No PATCH-by-id anywhere: search-then-create, exactly the pre-existing path.
    assert order[0] == ("POST", "/crm/v3/objects/contacts/search")
    assert all(m != "PATCH" for m, _ in order)


def test_writeback_crm_type_mismatch_uses_create_path(monkeypatch, hubspot_token):
    """A Salesforce-imported row pushed to a HubSpot column must not PATCH a
    HubSpot record with a Salesforce id."""
    order = []

    def handler(request: httpx.Request) -> httpx.Response:
        order.append((request.method, request.url.path))
        if request.url.path == "/crm/v3/objects/contacts/search":
            return httpx.Response(200, json={"results": []})
        return httpx.Response(201, json={"id": "88"})

    _patch_transport(monkeypatch, handler)
    lead_data = {"id": 1, "email": "a@x.com", "crm_external_id": "003ZZZ",
                 "crm_type": "salesforce"}
    res = _run(out._push_crm({"type": "hubspot"}, lead_data, WS_A,
                             workbook_id="wb1", lead_id=1))
    assert res["success"] is True
    assert all(m != "PATCH" for m, _ in order)


def test_writeback_update_error_is_reported_not_created(monkeypatch, hubspot_token):
    """A non-404 update failure surfaces as the cell error — no silent
    duplicate-creating fallback."""
    order = []

    def handler(request: httpx.Request) -> httpx.Response:
        order.append(request.method)
        return httpx.Response(500, text="boom")

    _patch_transport(monkeypatch, handler)
    lead_data = {"id": 1, "email": "a@x.com", "crm_external_id": "42",
                 "crm_type": "hubspot"}
    res = _run(out._push_crm({"type": "hubspot"}, lead_data, WS_A,
                             workbook_id="wb1", lead_id=1))
    assert res["success"] is False
    assert "HTTP 500" in res["error"]
    assert order == ["PATCH"]  # never fell through to create
