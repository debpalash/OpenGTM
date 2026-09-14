"""Offline contract tests for PostgreSQL workbook JSON expression indexes."""

import importlib


MIGRATION = "migrations.versions.e2f3a4b5c6d7_workbook_json_expression_indexes"


class _Bind:
    class dialect:
        name = "postgresql"


def test_upgrade_creates_search_and_stable_sort_indexes(monkeypatch):
    migration = importlib.import_module(MIGRATION)
    statements = []
    monkeypatch.setattr(migration.op, "get_bind", lambda: _Bind())
    monkeypatch.setattr(migration.op, "execute", lambda statement: statements.append(str(statement)))

    migration.upgrade()

    assert statements[0] == "CREATE EXTENSION IF NOT EXISTS pg_trgm"
    assert any("ix_workbook_rows_data_trgm" in sql and "gin_trgm_ops" in sql for sql in statements)
    assert any("ix_workbook_rows_enrichments_trgm" in sql and "gin_trgm_ops" in sql for sql in statements)
    for field in migration.SORT_FIELDS:
        sql = next(item for item in statements if f'ix_workbook_rows_sort_{field}' in item)
        assert "workbook_id" in sql
        assert f"data ->> '{field}'" in sql
        assert "position, id" in sql


def test_non_postgres_upgrade_is_a_noop(monkeypatch):
    migration = importlib.import_module(MIGRATION)

    class _SQLiteBind:
        class dialect:
            name = "sqlite"

    monkeypatch.setattr(migration.op, "get_bind", lambda: _SQLiteBind())
    monkeypatch.setattr(
        migration.op,
        "execute",
        lambda statement: (_ for _ in ()).throw(AssertionError(f"unexpected SQL: {statement}")),
    )

    migration.upgrade()
    migration.downgrade()
