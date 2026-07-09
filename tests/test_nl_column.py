"""
NL → column generator (nl_column.py + POST /api/v2/workbooks/{id}/generate-column).

Fully offline — the LLM client is mocked (same pattern as
test_research_prompt_injection's _ScriptedLLM). Covers:

  * formula path: expression is validated against the safe evaluator and returned;
  * invalid formula: ONE retry with the error fed back, then ai_formula fallback;
  * ai_formula and http paths pass the add-column ColumnConfig schema;
  * prompt content: DSL whitelist + existing columns are enumerated;
  * endpoint: response contract, workspace scoping (404 cross-tenant), 502 on
    unusable LLM output.
"""

import asyncio

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.database import Base, get_db
from apps.api.services.workbook import nl_column as NL
from apps.api.services.workbook.formula_column import evaluate_formula
from apps.api.services.workbook.models import Workbook, WorkbookRow

WS = "ws_nl"

COLS = [
    {"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company"},
    {"id": "website", "name": "Website", "type": "lead_field", "lead_field": "website"},
    {"id": "email", "name": "Email", "type": "lead_field", "lead_field": "email"},
]


class _ScriptedLLM:
    """Fake llm.* — returns scripted extract_json answers and records prompts."""

    def __init__(self, script):
        self._script = list(script)
        self.calls = []  # (prompt, system)

    async def extract_json(self, prompt, system="", max_tokens=1024):
        self.calls.append((prompt, system))
        return self._script.pop(0) if self._script else {}


def _gen(monkeypatch, script, instruction, columns=COLS):
    fake = _ScriptedLLM(script)
    monkeypatch.setattr(NL, "llm", fake)
    result = asyncio.run(NL.generate_column(instruction, columns))
    return result, fake


# ── formula path ──────────────────────────────────────────────────────────

def test_formula_path_validates_and_returns(monkeypatch):
    res, fake = _gen(monkeypatch, [{
        "kind": "formula",
        "name": "Email Domain",
        "formula": '{Email}.split("@")[1]',
        "explanation": "Splits on @.",
    }], "extract the domain from the email")

    assert res["kind"] == "formula"
    assert res["explanation"] == "Splits on @."
    col = res["column"]
    assert col["type"] == "formula"
    assert col["name"] == "Email Domain"
    assert col["id"] == "email_domain"
    assert col["formula"] == '{Email}.split("@")[1]'
    assert len(fake.calls) == 1
    # the returned expression actually evaluates against the safe evaluator
    assert evaluate_formula(col["formula"], {"Email": "a@b.com"}) == "b.com"


def test_prompt_enumerates_dsl_and_columns(monkeypatch):
    _, fake = _gen(monkeypatch, [{
        "kind": "formula", "name": "X", "formula": '{Email}', "explanation": "",
    }], "whatever")
    prompt, system = fake.calls[0]
    # instruction + existing columns
    assert "whatever" in prompt
    assert "{Company}" in prompt and "{Website}" in prompt and "{Email}" in prompt
    # DSL capabilities derived from formula_column whitelists
    for fn in ("default", "upper", "lower", "concat", "replace", "round"):
        assert fn in prompt
    for meth in ("split", "startswith", "zfill"):
        assert meth in prompt
    # kind-selection guidance lives in the system prompt
    assert "ai_formula" in system and "formula" in system and "http" in system


def test_invalid_formula_retry_succeeds(monkeypatch):
    res, fake = _gen(monkeypatch, [
        {"kind": "formula", "name": "Bad", "formula": 'import os', "explanation": ""},
        {"kind": "formula", "name": "Fixed", "formula": 'upper({Company})', "explanation": "fixed"},
    ], "uppercase the company")

    assert len(fake.calls) == 2
    # the retry prompt feeds the validation error back
    assert "FAILED validation" in fake.calls[1][0]
    assert res["kind"] == "formula"
    assert res["column"]["formula"] == "upper({Company})"


def test_invalid_formula_twice_falls_back_to_ai_formula(monkeypatch):
    res, fake = _gen(monkeypatch, [
        {"kind": "formula", "name": "Bad", "formula": '__import__("os")', "explanation": ""},
        {"kind": "formula", "name": "Bad2", "formula": 'regex_match({Email})', "explanation": ""},
    ], "extract the TLD from the email")

    assert len(fake.calls) == 2
    assert res["kind"] == "ai_formula"
    col = res["column"]
    assert col["type"] == "ai_formula"
    assert "extract the TLD from the email" in col["prompt"]
    # fallback prompt references existing columns as placeholders
    assert "{Company}" in col["prompt"]
    assert "couldn't be validated" in res["explanation"]


def test_retry_with_garbage_answer_still_falls_back(monkeypatch):
    res, fake = _gen(monkeypatch, [
        {"kind": "formula", "name": "Bad", "formula": "(((", "explanation": ""},
        {},  # extract_json total failure on retry
    ], "slugify the company name")
    assert len(fake.calls) == 2
    assert res["kind"] == "ai_formula"
    assert res["column"]["type"] == "ai_formula"


# ── ai_formula path ───────────────────────────────────────────────────────

def test_ai_formula_path(monkeypatch):
    res, fake = _gen(monkeypatch, [{
        "kind": "ai_formula",
        "name": "B2B or B2C",
        "prompt": "Classify {Company} as B2B or B2C. Reply with only the label.",
        "explanation": "Needs semantic judgment.",
    }], "classify each company as b2b or b2c")

    assert res["kind"] == "ai_formula"
    col = res["column"]
    assert col["type"] == "ai_formula"
    assert col["id"] == "b2b_or_b2c"
    assert col["prompt"].startswith("Classify {Company}")
    assert len(fake.calls) == 1


def test_ai_formula_missing_prompt_raises(monkeypatch):
    fake = _ScriptedLLM([{"kind": "ai_formula", "name": "X", "explanation": ""}])
    monkeypatch.setattr(NL, "llm", fake)
    with pytest.raises(NL.NLColumnError):
        asyncio.run(NL.generate_column("do something", COLS))


# ── http path ─────────────────────────────────────────────────────────────

def test_http_path(monkeypatch):
    res, _ = _gen(monkeypatch, [{
        "kind": "http",
        "name": "Logo URL",
        "http_url": "https://logo.clearbit.com/{Website}",
        "http_method": "get",
        "http_extract": "$.url",
        "explanation": "Clearbit logo per row.",
    }], "get the company logo from clearbit")

    assert res["kind"] == "http"
    col = res["column"]
    assert col["type"] == "http"
    assert col["http_url"] == "https://logo.clearbit.com/{Website}"
    assert col["http_method"] == "GET"  # normalized
    assert col["http_extract"] == "$.url"
    assert "http_body" not in col  # GET never carries a body


def test_http_missing_url_raises(monkeypatch):
    fake = _ScriptedLLM([{"kind": "http", "name": "X"}])
    monkeypatch.setattr(NL, "llm", fake)
    with pytest.raises(NL.NLColumnError):
        asyncio.run(NL.generate_column("call some api", COLS))


# ── misc unit behaviour ───────────────────────────────────────────────────

def test_unknown_kind_raises(monkeypatch):
    fake = _ScriptedLLM([{"kind": "research", "name": "X"}])
    monkeypatch.setattr(NL, "llm", fake)
    with pytest.raises(NL.NLColumnError):
        asyncio.run(NL.generate_column("browse the web", COLS))


def test_generated_id_avoids_collisions(monkeypatch):
    cols = COLS + [{"id": "email_domain", "name": "Email Domain", "type": "formula"}]
    res, _ = _gen(monkeypatch, [{
        "kind": "formula", "name": "Email Domain",
        "formula": '{Email}.split("@")[1]', "explanation": "",
    }], "domain again", columns=cols)
    assert res["column"]["id"] == "email_domain_2"


def test_empty_instruction_raises():
    with pytest.raises(NL.NLColumnError):
        asyncio.run(NL.generate_column("   ", COLS))


# ── endpoint: contract + workspace scoping ────────────────────────────────

class _User:
    id = "user-1"


@pytest.fixture()
def client():
    from apps.api.routers.workbooks import router_v2

    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[Workbook.__table__, WorkbookRow.__table__])
    Session = sessionmaker(bind=engine)

    app = FastAPI()
    app.include_router(router_v2)

    def _override_db():
        s = Session()
        try:
            yield s
        finally:
            s.close()

    app.dependency_overrides[get_db] = _override_db
    app.dependency_overrides[current_workspace] = lambda: WorkspaceCtx(
        user=_User(), workspace_id=WS, slug="nl"
    )
    return TestClient(app), Session


def _mk_workbook(Session, workspace_id=WS, columns=COLS):
    s = Session()
    wb = Workbook(name="WB", workspace_id=workspace_id, columns_config=columns)
    s.add(wb)
    s.commit()
    wid = wb.id
    s.close()
    return wid


def test_endpoint_returns_ready_to_add_column(client, monkeypatch):
    tc, Session = client
    wid = _mk_workbook(Session)
    monkeypatch.setattr(NL, "llm", _ScriptedLLM([{
        "kind": "formula",
        "name": "Website Domain",
        "formula": '{Website}.replace("https://", "").replace("http://", "").split("/")[0]',
        "explanation": "Strips the scheme and path.",
    }]))

    r = tc.post(f"/api/v2/workbooks/{wid}/generate-column",
                json={"instruction": "extract the domain from the website URL"})
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["kind"] == "formula"
    assert body["explanation"] == "Strips the scheme and path."
    col = body["column"]
    # shape matches the add-column API's ColumnConfig
    assert col["id"] and col["name"] == "Website Domain" and col["type"] == "formula"
    assert col["formula"].startswith("{Website}")


def test_endpoint_scoped_to_workspace(client, monkeypatch):
    tc, Session = client
    other = _mk_workbook(Session, workspace_id="ws_other")
    called = _ScriptedLLM([{"kind": "formula", "name": "X", "formula": "{Email}"}])
    monkeypatch.setattr(NL, "llm", called)

    r = tc.post(f"/api/v2/workbooks/{other}/generate-column",
                json={"instruction": "extract the domain"})
    assert r.status_code == 404  # cross-tenant → 404, and the LLM is never called
    assert called.calls == []


def test_endpoint_502_on_unusable_llm_output(client, monkeypatch):
    tc, Session = client
    wid = _mk_workbook(Session)
    monkeypatch.setattr(NL, "llm", _ScriptedLLM([{}]))  # extract_json failure

    r = tc.post(f"/api/v2/workbooks/{wid}/generate-column",
                json={"instruction": "extract the domain"})
    assert r.status_code == 502
    assert "Column generation failed" in r.json()["detail"]


def test_endpoint_validates_instruction(client):
    tc, Session = client
    wid = _mk_workbook(Session)
    r = tc.post(f"/api/v2/workbooks/{wid}/generate-column", json={"instruction": ""})
    assert r.status_code == 422
