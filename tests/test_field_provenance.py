"""Per-fact provenance — unit + write-path tests (SQLite, no network).

Covers docs/specs/research-per-fact-provenance-spec.md acceptance criteria:
  - license resolves per-provider (declared attr) + central fallback map; unknown
    providers → "unknown", never crash (AC5).
  - provenance_for builds {source, license, confidence, fetched_at} (AC1).
  - Lead.to_dict/from_dict round-trips field_provenance (storage).
  - EnrichmentOverlay/Providence schema serializes provenance (AC4).
  - _set_enrichment: flag-ON writes provenance into the WorkbookRow cell dict +
    WorkbookEnrichment.cell_metadata mirror (AC1); flag-OFF cell dict is
    byte-identical to the legacy shape (AC7).
  - leadgen-core apply_results + email waterfall + workbook _write_back_to_lead
    populate lead field_provenance + refresh last_enriched_at (AC2, AC3).
"""
import uuid

import pytest

from apps.api.core import config as app_config
from apps.api.services.leadgen.enrichment import licenses as L
from apps.api.services.leadgen.models import Lead


@pytest.fixture
def prov_on(monkeypatch):
    monkeypatch.setattr(app_config.settings, "PROVENANCE_TRACKING_ENABLED", True)
    yield


@pytest.fixture
def prov_off(monkeypatch):
    monkeypatch.setattr(app_config.settings, "PROVENANCE_TRACKING_ENABLED", False)
    yield


# ── licenses: vocabulary + resolution ────────────────────────────────────────

def test_resolve_license_central_map():
    assert L.resolve_license("hunter_io") == "proprietary-api"
    assert L.resolve_license("gleif") == "CC0-1.0"
    assert L.resolve_license("wikidata") == "CC0-1.0"
    assert L.resolve_license("mca_registry") == "public-record"
    assert L.resolve_license("website_scraper") == "scraped"


def test_resolve_license_unknown_never_crashes():
    assert L.resolve_license("totally_made_up_provider") == "unknown"
    assert L.resolve_license(None) == "unknown"
    assert L.resolve_license("") == "unknown"


def test_resolve_license_declared_wins_over_map():
    # A provider declaring a real token beats the name-keyed fallback.
    assert L.resolve_license("hunter_io", declared="CC0-1.0") == "CC0-1.0"
    # The "unknown" default never wins; falls back to the map.
    assert L.resolve_license("hunter_io", declared="unknown") == "proprietary-api"
    # A garbage declared token is ignored.
    assert L.resolve_license("hunter_io", declared="not-a-license") == "proprietary-api"


def test_resolve_license_strips_cache_prefix():
    assert L.resolve_license("cache:gleif") == "CC0-1.0"


def test_every_mapped_license_is_in_vocab():
    for lic in L.PROVIDER_LICENSE.values():
        assert lic in L.LICENSE_VOCAB


# ── provenance_for shape ──────────────────────────────────────────────────────

def test_provenance_for_shape():
    p = L.provenance_for("hunter_io", confidence=0.83)
    assert set(p.keys()) == {"source", "license", "confidence", "fetched_at"}
    assert p["source"] == "hunter_io"
    assert p["license"] == "proprietary-api"
    assert p["confidence"] == 0.83
    assert p["fetched_at"]  # ISO timestamp present


def test_provenance_for_confidence_optional():
    p = L.provenance_for("ai")  # no confidence (e.g. AI column)
    assert p["confidence"] is None
    assert p["license"] == "unknown"


def test_merge_field_provenance_roundtrip_and_tolerant():
    cur = L.merge_field_provenance("", {"email": L.provenance_for("hunter_io")})
    merged = L.merge_field_provenance(cur, {"phone": L.provenance_for("apollo_io")})
    import json
    d = json.loads(merged)
    assert set(d.keys()) == {"email", "phone"}
    assert d["email"]["license"] == "proprietary-api"
    # Malformed current is treated as empty, not fatal.
    out = L.merge_field_provenance("not json", {"x": {"source": "s"}})
    assert json.loads(out) == {"x": {"source": "s"}}


# ── Lead dataclass round-trip ─────────────────────────────────────────────────

def test_lead_roundtrips_field_provenance():
    fp = L.merge_field_provenance("", {"email": L.provenance_for("gleif", confidence=0.9)})
    lead = Lead(company="Acme", field_provenance=fp)
    d = lead.to_dict()
    assert "field_provenance" in d
    back = Lead.from_dict(d)
    assert back.field_provenance == fp
    # default is empty string (legacy / un-enriched)
    assert Lead(company="X").field_provenance == ""


# ── API schema serialization ─────────────────────────────────────────────────

def test_enrichment_overlay_serializes_provenance():
    from apps.api.services.workbook.schemas import EnrichmentOverlay
    overlay = {
        "value": "a@b.com", "status": "complete", "provider": "hunter_io",
        "error": None, "verify_status": "valid",
        "provenance": {"source": "hunter_io", "license": "proprietary-api",
                       "confidence": 0.8, "fetched_at": "2026-06-26T00:00:00+00:00"},
    }
    m = EnrichmentOverlay(**overlay)
    assert m.provenance is not None
    assert m.provenance.license == "proprietary-api"
    assert m.provenance.confidence == 0.8
    # absent provenance → None (legacy cells)
    legacy = EnrichmentOverlay(value="x", status="complete")
    assert legacy.provenance is None


# ── leadgen-core apply_results ────────────────────────────────────────────────

def test_apply_results_writes_field_provenance(prov_on):
    from apps.api.services.leadgen.enrichment.provider import WaterfallEnricher, WaterfallLog
    wf = WaterfallEnricher(cache_enabled=False)
    lead = Lead(company="Acme")
    logs = [WaterfallLog(field_name="email", winner="hunter_io",
                         final_value="a@acme.com", final_confidence=0.77)]
    wf.apply_results(lead, {"email": "a@acme.com"}, logs)
    import json
    fp = json.loads(lead.field_provenance)
    assert fp["email"]["source"] == "hunter_io"
    assert fp["email"]["license"] == "proprietary-api"
    assert fp["email"]["confidence"] == 0.77
    assert fp["email"]["fetched_at"]


def test_apply_results_no_provenance_when_flag_off(prov_off):
    from apps.api.services.leadgen.enrichment.provider import WaterfallEnricher, WaterfallLog
    wf = WaterfallEnricher(cache_enabled=False)
    lead = Lead(company="Acme")
    logs = [WaterfallLog(field_name="email", winner="hunter_io",
                         final_value="a@acme.com", final_confidence=0.77)]
    wf.apply_results(lead, {"email": "a@acme.com"}, logs)
    assert lead.field_provenance == ""  # byte-identical: untouched


# ── email waterfall ───────────────────────────────────────────────────────────

def test_email_waterfall_records_provenance(prov_on):
    from apps.api.services.leadgen.enrichment.email_waterfall import (
        _record_provenance, WaterfallOutcome,
    )
    lead = Lead(company="Acme")
    outcome = WaterfallOutcome(email="x@acme.com", provider="hunter_io")
    _record_provenance(lead, outcome)
    import json
    fp = json.loads(lead.field_provenance)
    assert fp["email"]["source"] == "hunter_io"
    assert fp["email"]["license"] == "proprietary-api"


def test_email_waterfall_no_provenance_when_off(prov_off):
    from apps.api.services.leadgen.enrichment.email_waterfall import (
        _record_provenance, WaterfallOutcome,
    )
    lead = Lead(company="Acme")
    _record_provenance(lead, WaterfallOutcome(email="x@acme.com", provider="hunter_io"))
    assert lead.field_provenance == ""


# ── _set_enrichment cell dict (DB-backed, SQLite) ─────────────────────────────

@pytest.fixture
def wb_session():
    """A SQLite-backed session with workbook tables + one seeded row."""
    from apps.api.database import Base, engine, SessionLocal
    from apps.api.services.workbook.models import Workbook, WorkbookRow
    from apps.api.core import tenancy

    Base.metadata.create_all(bind=engine)
    ws = "ws_prov_test"
    tenancy.current_workspace_var.set(ws)
    wid = str(uuid.uuid4())
    s = SessionLocal()
    wb = Workbook(id=wid, name="WB", workspace_id=ws, columns_config=[])
    row = WorkbookRow(workspace_id=ws, workbook_id=wid, position=0,
                      data={"company": "Acme"}, enrichments={}, lead_id=4242)
    s.add_all([wb, row])
    s.commit()
    yield s, wid, row.lead_id
    s.rollback()
    s.query(WorkbookRow).filter(WorkbookRow.workbook_id == wid).delete()
    s.query(Workbook).filter(Workbook.id == wid).delete()
    s.commit()
    s.close()


def test_set_enrichment_byte_identical_without_provenance(wb_session):
    from apps.api.services.workbook import enrichment as E
    from apps.api.services.workbook.models import WorkbookRow, WorkbookEnrichment
    s, wid, lead_id = wb_session
    E._set_enrichment(s, wid, lead_id, "col_email", "a@acme.com", "complete",
                      provider="hunter_io")
    s.commit()
    row = s.query(WorkbookRow).filter(WorkbookRow.workbook_id == wid).first()
    cell = row.enrichments["col_email"]
    # Legacy shape exactly — no provenance key leaks when not supplied.
    assert cell == {"value": "a@acme.com", "status": "complete",
                    "provider": "hunter_io", "error": None}
    we = s.query(WorkbookEnrichment).filter(
        WorkbookEnrichment.workbook_id == wid,
        WorkbookEnrichment.column_id == "col_email").first()
    assert we.cell_metadata is None


def test_set_enrichment_writes_provenance(wb_session):
    from apps.api.services.workbook import enrichment as E
    from apps.api.services.workbook.models import WorkbookRow, WorkbookEnrichment
    s, wid, lead_id = wb_session
    prov = L.provenance_for("hunter_io", confidence=0.8)
    E._set_enrichment(s, wid, lead_id, "col_email", "a@acme.com", "complete",
                      provider="hunter_io", provenance=prov)
    s.commit()
    row = s.query(WorkbookRow).filter(WorkbookRow.workbook_id == wid).first()
    cell = row.enrichments["col_email"]
    assert cell["provenance"] == prov
    assert cell["provenance"]["confidence"] == 0.8  # AC2: confidence persisted
    we = s.query(WorkbookEnrichment).filter(
        WorkbookEnrichment.workbook_id == wid,
        WorkbookEnrichment.column_id == "col_email").first()
    assert we.cell_metadata["provenance"] == prov


# ── workbook _write_back_to_lead (legacy SQLite store path) ───────────────────

def test_write_back_to_lead_records_provenance(prov_on, monkeypatch, tmp_path):
    from apps.api.services.workbook import enrichment as E
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.core import tenancy

    db_file = str(tmp_path / "leads.db")
    # Force the unscoped legacy path (no workspace) → LeadDB(default). Patch the
    # class so it targets our temp file instead of the real default DB.
    tenancy.current_workspace_var.set("")
    monkeypatch.setattr(E, "LeadDB", lambda: LeadDB(db_file))

    seed = LeadDB(db_file)
    lid = seed.upsert_lead(Lead(company="Acme", city="NYC"))
    seed.close()

    E._write_back_to_lead(lid, "phone", "+1-555", provider="apollo_io")

    check = LeadDB(db_file)
    lead = check.get_lead(lid)
    check.close()
    import json
    fp = json.loads(lead.field_provenance)
    assert fp["phone"]["source"] == "apollo_io"
    assert fp["phone"]["license"] == "proprietary-api"
    assert lead.last_enriched_at  # refreshed (AC3)


def test_write_back_to_lead_no_provenance_when_off(prov_off, monkeypatch, tmp_path):
    from apps.api.services.workbook import enrichment as E
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.core import tenancy

    db_file = str(tmp_path / "leads2.db")
    tenancy.current_workspace_var.set("")
    monkeypatch.setattr(E, "LeadDB", lambda: LeadDB(db_file))
    seed = LeadDB(db_file)
    lid = seed.upsert_lead(Lead(company="Acme", city="NYC"))
    seed.close()

    E._write_back_to_lead(lid, "phone", "+1-555", provider="apollo_io")

    check = LeadDB(db_file)
    lead = check.get_lead(lid)
    check.close()
    # Untouched when the flag is off (field stays default "").
    assert (lead.field_provenance or "") == ""
