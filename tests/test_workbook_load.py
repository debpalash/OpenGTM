import pytest

from apps.api.database import SessionLocal
from apps.api.services.workbook.models import Workbook
from apps.api.services.workbook_load import CONFIRMATION, run_workbook_load_test


def test_workbook_load_harness_validates_distant_selection_and_cleans_up():
    report = run_workbook_load_test(
        rows=500,
        page_size=25,
        confirmation=CONFIRMATION,
        max_page_ms=10_000,
        max_search_ms=10_000,
        max_selection_ms=10_000,
        allow_sqlite=True,
    )
    assert report["ok"] and report["selection_exact"]
    assert report["search_matches"] == 1
    assert report["selected_positions"][0] == 0
    assert report["selected_positions"][-1] == 499
    with SessionLocal() as db:
        assert not db.query(Workbook).filter(Workbook.id.like("workbook-load-%")).count()


def test_workbook_load_harness_fails_closed():
    with pytest.raises(ValueError, match="confirmation"):
        run_workbook_load_test(rows=500, confirmation="", allow_sqlite=True)
    with pytest.raises(ValueError, match="safe bounds"):
        run_workbook_load_test(rows=50, confirmation=CONFIRMATION, allow_sqlite=True)
