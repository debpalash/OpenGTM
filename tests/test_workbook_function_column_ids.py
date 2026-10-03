"""Saved function chains use the same column IDs as workbook definitions."""

import sqlite3

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.services.workbook import functions
from apps.api.services.workbook.models import Workbook


@pytest.fixture
def native_stores(tmp_path, monkeypatch):
    functions_path = tmp_path / 'functions.db'

    def connect():
        db = sqlite3.connect(functions_path)
        db.row_factory = sqlite3.Row
        db.execute('''CREATE TABLE IF NOT EXISTS functions (
            id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT,
            category TEXT, columns_chain TEXT, version INTEGER,
            usage_count INTEGER, created_at REAL, updated_at REAL
        )''')
        return db

    monkeypatch.setattr(functions, '_get_db', connect)
    engine = create_engine(f'sqlite:///{tmp_path / "workbooks.db"}')
    Workbook.__table__.create(engine)
    session = sessionmaker(bind=engine)()
    yield session
    session.close()
    engine.dispose()


def apply_chain(session, existing, chain):
    workbook = Workbook(id='workbook-test', workspace_id='workspace-test',
                        name='Function fixture', columns_config=existing)
    session.add(workbook)
    session.commit()
    saved = functions.create_function('Reusable chain', '', chain)
    result = functions.apply_function(saved['id'], workbook.id, session)
    session.expire_all()
    return result, session.get(Workbook, workbook.id).columns_config


def test_function_adds_id_based_columns_to_nonempty_workbook(native_stores):
    existing = [{'id': 'company', 'name': 'Company', 'type': 'lead_field'}]
    chain = [{'id': 'email', 'name': 'Email', 'type': 'input'}]
    result, columns = apply_chain(native_stores, existing, chain)
    assert result['columns_added'] == 1
    assert columns == existing + chain


def test_function_keeps_existing_id_column_and_adds_only_new_id(native_stores):
    existing = [{'id': 'email', 'name': 'Keep existing', 'type': 'input'}]
    chain = [{'id': 'email', 'name': 'Do not overwrite', 'type': 'input'},
             {'id': 'score', 'name': 'Score', 'type': 'formula', 'formula': '1'}]
    result, columns = apply_chain(native_stores, existing, chain)
    assert result['columns_added'] == 1
    assert columns == [existing[0], chain[1]]


def test_function_preserves_legacy_key_duplicate_behavior(native_stores):
    existing = [{'key': 'existing', 'name': 'Existing'}]
    chain = [{'key': 'existing', 'name': 'Keep original'}, {'key': 'new', 'name': 'New'}]
    result, columns = apply_chain(native_stores, existing, chain)
    assert result['columns_added'] == 1
    assert columns == [existing[0], chain[1]]


def test_function_deduplicates_repeated_ids_inside_chain(native_stores):
    first = {'id': 'email', 'name': 'First', 'type': 'input'}
    result, columns = apply_chain(native_stores, [], [first, {**first, 'name': 'Second'}])
    assert result['columns_added'] == 1
    assert columns == [first]
