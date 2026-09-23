import pytest

from apps.api.services.workbook.cell_scope import restrict_work_items


def test_saved_dependency_values_keep_types_and_exclude_failed_or_deleted_cells():
    from types import SimpleNamespace
    from apps.api.services.workbook.cell_scope import row_execution_data
    row = SimpleNamespace(id=7, lead_id=None, data={"company": "Acme"}, enrichments={
        "zero": {"status": "complete", "value": 0},
        "flag": {"status": "complete", "value": False},
        "failed": {"status": "error", "value": "stale"},
        "deleted": {"status": "complete", "value": "stale"},
        "__row_id": {"status": "complete", "value": 999},
    })
    columns = [{"id": key, "name": key.title()} for key in ("zero", "flag", "failed", "__row_id")]
    data = row_execution_data(row, columns)
    assert data["zero"] == data["Zero"] == 0
    assert data["flag"] is data["Flag"] is False
    assert "failed" not in data and "deleted" not in data
    assert data["__row_id"] == 7


def test_exact_cells_preserve_dependency_order_and_row_identity():
    columns = [{"id": "first"}, {"id": "second"}, {"id": "third"}]
    rows = [({"__row_id": i, "id": 99}, columns) for i in (1, 2, 3)]
    result = restrict_work_items(rows, {"1": ["third", "first"], "2": ["second"]})
    assert [(row["__row_id"], [c["id"] for c in cols]) for row, cols in result] == [
        (1, ["first", "third"]), (2, ["second"])]
    assert len(columns) == 3


@pytest.mark.parametrize("scope", [{}, {"1": []}, {"2": ["first"]}, {"1": ["missing"]}])
def test_empty_or_missing_scope_never_expands(scope):
    assert restrict_work_items([({"__row_id": 1}, [{"id": "first"}])], scope) == []


@pytest.mark.parametrize("scope", [[], {"01": []}, {"-1": []}, {"1": "first"}, {"1": [None]}, {1: [], "1": []}])
def test_malformed_scope_is_rejected(scope):
    with pytest.raises(ValueError):
        restrict_work_items([], scope)


@pytest.mark.parametrize("status,value", [("error", "old"), ("running", "old"), ("pending", "old"), ("skipped", "old"), ("complete", None)])
def test_failed_snapshot_invalidates_materialized_row_data(status, value):
    from types import SimpleNamespace
    from apps.api.services.workbook.cell_scope import row_execution_data
    row = SimpleNamespace(id=7, lead_id=None,
        data={"company": "Fixture", "email": "stale@example.test", "Found email": "stale@example.test"},
        enrichments={"email": {"status": status, "value": value}})
    result = row_execution_data(row, [{"id": "email", "name": "Found email", "type": "enrichment"}])
    assert "email" not in result and "Found email" not in result
    assert result["company"] == "Fixture" and result["__row_id"] == 7
    assert row.data["email"] == "stale@example.test"  # Execution projection only.


def test_legacy_identity_is_not_a_workbook_row_identity():
    with pytest.raises(ValueError):
        restrict_work_items([({"id": 1}, [{"id": "first"}])], {"1": ["first"]})


def test_absent_scope_preserves_legacy_contract():
    items = [({"id": 1}, [{"id": "first"}])]
    assert restrict_work_items(items, None) is items


def test_selected_cell_count_does_not_invent_rectangular_exact_scope():
    from apps.api.services.workbook.cell_scope import selected_cell_count
    assert selected_cell_count([1, 2], ["a", "b"], {"1": ["a"], "2": ["b"]}) == 2
    assert selected_cell_count([1, 2], ["a", "b"]) == 4
    assert selected_cell_count([], []) == 0
    assert selected_cell_count(None, ["a"]) is None
    assert selected_cell_count([1], ["a"], {"1": ["missing"]}) is None
    assert selected_cell_count([1], ["a"], {}) is None


@pytest.mark.parametrize("column,allowed", [
    ({"type": "output"}, False), ({"type": "output", "run_once": False}, False),
    ({"type": "http", "http_method": "POST"}, False),
    ({"type": "http", "http_method": "PUT"}, False),
    ({"type": "http", "http_method": "DELETE"}, False),
    ({"type": "http", "http_method": "get"}, True),
    ({"type": "formula"}, True),
])
def test_automatic_retry_does_not_repeat_external_writes(column, allowed):
    from apps.api.services.workbook.cell_scope import allows_automatic_retry
    assert allows_automatic_retry(column) is allowed


def test_computed_alias_cannot_replace_execution_identity(monkeypatch):
    import asyncio
    from apps.api.services.workbook import enrichment as engine
    seen = []
    async def cell(workbook_id, row, *args, **kwargs):
        seen.append(dict(row))
        return {"success": True, "value": 999}
    monkeypatch.setattr(engine, "_run_one_cell", cell)
    identity = {"id": 7, "__row_id": 8, "__lead_id": 7}
    columns = [{"id": "__row_id", "name": "id"}, {"id": "next"}]
    asyncio.run(engine._run_one_row("wb", identity, columns, columns, None))
    assert seen == [identity, identity]


@pytest.mark.parametrize("crash", [False, True])
@pytest.mark.parametrize("external", [False, True])
def test_engine_batch_sync_and_retry_keep_exact_cells(tmp_path, monkeypatch, crash, external):
    import asyncio
    from sqlalchemy import create_engine
    from sqlalchemy.orm import sessionmaker
    from apps.api.database import Base
    from apps.api.services.workbook import enrichment as engine
    from apps.api.services.workbook.models import Workbook, WorkbookRow, WorkbookEnrichment

    connection = create_engine(f"sqlite:///{tmp_path / 'cells.db'}")
    Base.metadata.create_all(connection, tables=[Workbook.__table__, WorkbookRow.__table__, WorkbookEnrichment.__table__])
    sessions = sessionmaker(bind=connection)
    columns = [{"id": "a", "type": "formula", "formula": "1"},
               {"id": "b", "type": "formula", "formula": "2"}]
    if external:
        columns[0] = {"id": "a", "type": "output", "destination": "webhook"}
    with sessions() as db:
        workbook = Workbook(name="Exact cells", workspace_id="scope-test", columns_config=columns)
        db.add(workbook)
        db.flush()
        wid = workbook.id
        rows = [WorkbookRow(workbook_id=wid, workspace_id="scope-test", position=i, data={},
                            enrichments={cid: {"status": "error"} for cid in ("a", "b")}) for i in range(2)]
        db.add_all(rows)
        db.flush()
        first, second = [row.id for row in rows]
        # Historical errors include selected AND unselected cells.
        for rid in (first, second):
            for cid in ("a", "b"):
                db.add(WorkbookEnrichment(workbook_id=wid, workspace_id="scope-test", lead_id=rid, column_id=cid, status="error"))
        db.commit()

    monkeypatch.setattr(engine, "SessionLocal", sessions)
    monkeypatch.setattr(engine, "_make_redis", lambda: None)
    monkeypatch.setattr(engine, "flush_row_change_emits", lambda *args: None)
    observed, batches = [], []
    async def batch(wid, work, config, redis, **kwargs):
        batches.extend((row["__row_id"], col["id"]) for row, cols in work for col in cols)
        return set(), 0
    async def execute(wid, row, cols, config, redis, **kwargs):
        observed.extend((row["__row_id"], col["id"]) for col in cols)
        if crash:
            raise RuntimeError("Simulated row execution failure")
        return {"completed": 0, "errors": len(cols)}
    monkeypatch.setattr(engine, "_run_ai_batch_prepass", batch)
    monkeypatch.setattr(engine, "_run_one_row", execute)
    result = asyncio.run(engine.run_workbook_enrichment(
        wid, row_ids=[first, second], row_columns={str(first): ["a"], str(second): ["b"]},
        workspace_id="scope-test", retry_passes=1,
    ))
    assert batches == [(first, "a"), (second, "b")]
    assert observed == [(first, "a"), (second, "b")] + ([] if external else [(first, "a")]) + [(second, "b")]
    assert result["total"] == 2
    assert result["errors"] == 2
    with sessions() as db:
        assert db.get(Workbook, wid).status == "failed"
    connection.dispose()


@pytest.mark.parametrize("collision", [False, True])
def test_real_formula_execution_persists_only_selected_cells(tmp_path, monkeypatch, collision):
    import asyncio
    from sqlalchemy import create_engine
    from sqlalchemy.orm import sessionmaker
    from apps.api.database import Base
    from apps.api.services.workbook import enrichment as engine
    from apps.api.services.workbook.models import Workbook, WorkbookRow, WorkbookEnrichment

    connection = create_engine(f"sqlite:///{tmp_path / 'formulas.db'}")
    Base.metadata.create_all(connection, tables=[Workbook.__table__, WorkbookRow.__table__, WorkbookEnrichment.__table__])
    sessions = sessionmaker(bind=connection, autoflush=False)
    # Intentionally reversed: the real engine must order the dependency chain.
    columns = [{"id": "label", "name": "Label", "type": "formula", "formula": 'upper({domain})'},
               {"id": "domain", "name": "Domain", "type": "formula", "formula": '{email}.split("@")[1]'}]
    with sessions() as db:
        workbook = Workbook(name="Formula execution", workspace_id="scope-test", columns_config=columns)
        db.add(workbook)
        db.flush()
        wid = workbook.id
        rows = [WorkbookRow(workbook_id=wid, workspace_id="scope-test", position=i,
                            data={"email": f"person@company{i}.example"},
                            enrichments={"label": {"value": "KEEP", "status": "complete"}}) for i in range(2)]
        db.add_all(rows)
        db.flush()
        first, second = [row.id for row in rows]
        if collision:
            rows[1].lead_id = first
            # The legacy overlay says complete for the colliding numeric key,
            # but the selected v2 row's domain is actually missing.
            db.add(WorkbookEnrichment(workbook_id=wid, workspace_id="scope-test", lead_id=first,
                                      column_id="domain", status="complete", value="WRONG-LEGACY-VALUE"))
            rows[0].enrichments = {}  # selected chain genuinely needs filling
        db.commit()
    monkeypatch.setattr(engine, "SessionLocal", sessions)
    monkeypatch.setattr(engine, "_make_redis", lambda: None)
    monkeypatch.setattr(engine, "flush_row_change_emits", lambda *args: None)
    monkeypatch.setattr(engine, "BATCH_ENABLED", False)
    result = asyncio.run(engine.run_workbook_enrichment(
        wid, row_ids=[first, second], workspace_id="scope-test", retry_passes=0, fill_missing=collision,
        row_columns={str(first): ["label", "domain"], str(second): [] if collision else ["domain"]},
    ))
    assert result["total"] == result["completed"] == (2 if collision else 3)
    assert result["errors"] == 0
    with sessions() as db:
        assert db.get(Workbook, wid).status == "complete"
        first_row, second_row = db.get(WorkbookRow, first), db.get(WorkbookRow, second)
        assert first_row.enrichments["domain"]["value"] == "company0.example"
        assert first_row.enrichments["label"]["value"] == "COMPANY0.EXAMPLE"
        if collision:
            assert "domain" not in second_row.enrichments
        else:
            assert second_row.enrichments["domain"]["value"] == "company1.example"
        assert second_row.enrichments["label"] == {"value": "KEEP", "status": "complete"}
        records = db.query(WorkbookEnrichment).all()
        expected = {(first, "domain"), (first, "label")}
        if not collision:
            expected.add((second, "domain"))
        assert {(r.lead_id, r.column_id) for r in records} == expected
        assert all(r.status == "complete" for r in records)
    # Rerun just the downstream column: it must consume the persisted domain.
    result = asyncio.run(engine.run_workbook_enrichment(
        wid, row_ids=[first], column_ids=["label"], workspace_id="scope-test", retry_passes=0,
        row_columns={str(first): ["label"]},
    ))
    assert result["completed"] == result["total"] == 1
    assert result["errors"] == 0
    with sessions() as db:
        assert db.get(WorkbookRow, first).enrichments["label"]["value"] == "COMPANY0.EXAMPLE"
        workbook = db.get(Workbook, wid)
        workbook.columns_config = [{**column, "formula": "1 / 0"} if column["id"] == "domain" else column
                                   for column in columns]
        db.commit()
    result = asyncio.run(engine.run_workbook_enrichment(
        wid, row_ids=[first], workspace_id="scope-test", retry_passes=1,
        row_columns={str(first): ["domain", "label"]},
    ))
    assert result["completed"] == 0
    assert result["errors"] == 2
    with sessions() as db:
        cell = db.get(WorkbookRow, first).enrichments["label"]
        assert cell["status"] == "error"
        assert cell["value"] is None
        assert cell["error"] == "upstream_dependency_failed"
    connection.dispose()
