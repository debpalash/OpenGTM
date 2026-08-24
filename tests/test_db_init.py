import pytest

from apps.api import db_init


def test_alembic_failure_is_not_masked_by_create_all(monkeypatch):
    monkeypatch.setenv("YUPCHA_DB_INIT", "alembic")
    monkeypatch.setattr(
        db_init, "_alembic_upgrade_head", lambda: (_ for _ in ()).throw(RuntimeError("bad migration"))
    )
    create_calls = []
    monkeypatch.setattr(db_init, "_create_all", lambda: create_calls.append(True))

    with pytest.raises(RuntimeError, match="bad migration"):
        db_init.init_db()
    assert create_calls == []


def test_create_all_is_explicit_and_dev_only(monkeypatch):
    monkeypatch.setenv("YUPCHA_DB_INIT", "create_all")
    monkeypatch.setenv("APP_ENV", "production")
    monkeypatch.setattr(db_init, "_create_all", lambda: pytest.fail("must not run"))

    with pytest.raises(RuntimeError, match="forbidden"):
        db_init.init_db()


def test_invalid_init_mode_fails(monkeypatch):
    monkeypatch.setenv("YUPCHA_DB_INIT", "best_effort")
    with pytest.raises(ValueError, match="YUPCHA_DB_INIT"):
        db_init.init_db()
