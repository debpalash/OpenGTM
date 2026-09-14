import time

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.services.leadgen.orm_models import SignalRow
from apps.api.services.signals.store import get_signal_store


@pytest.fixture()
def signal_store(monkeypatch):
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[SignalRow.__table__])
    factory = sessionmaker(bind=engine)
    monkeypatch.setattr("apps.api.services.signals.store.SessionLocal", factory)
    now = time.time()
    with factory() as db:
        db.add_all([
            SignalRow(id=f"s-{index:02d}", workspace_id="ws", lead_id=index % 3,
                      company=f"Company {index % 3}", signal_type="hiring" if index % 2 else "funding",
                      created_at=now - (index // 3))
            for index in range(12)
        ])
        db.add(SignalRow(id="foreign", workspace_id="other", signal_type="hiring", created_at=now + 1))
        db.commit()
    return get_signal_store("ws"), factory, now


def test_signal_cursor_traversal_is_stable_and_tenant_safe(signal_store):
    store, factory, now = signal_store
    first = store.get_signals_page(limit=5)
    assert first["has_more"] is True
    assert len(first["signals"]) == 5
    assert "foreign" not in {row["id"] for row in first["signals"]}

    # A newer concurrent insert must not shift or duplicate the next keyset page.
    with factory() as db:
        db.add(SignalRow(id="new", workspace_id="ws", signal_type="hiring", created_at=now + 10))
        db.commit()
    second = store.get_signals_page(limit=5, cursor=first["next_cursor"])
    assert not ({row["id"] for row in first["signals"]} & {row["id"] for row in second["signals"]})
    assert "new" not in {row["id"] for row in second["signals"]}

    third = store.get_signals_page(limit=5, cursor=second["next_cursor"])
    traversed = first["signals"] + second["signals"] + third["signals"]
    assert len(traversed) == 12
    assert len({row["id"] for row in traversed}) == 12
    assert third["has_more"] is False


def test_signal_page_combines_types_and_account_identity_before_limit(signal_store):
    store, _, _ = signal_store
    page = store.get_signals_page(
        signal_types=["hiring", "funding"],
        lead_ids=[1],
        companies=["company 2"],
        limit=3,
    )
    assert len(page["signals"]) == 3
    assert all(row["lead_id"] == 1 or row["company"] == "Company 2" for row in page["signals"])
    assert page["has_more"] is True


def test_signal_cursor_rejects_malformed_values(signal_store):
    store, _, _ = signal_store
    with pytest.raises(ValueError, match="invalid signal cursor"):
        store.get_signals_page(cursor="not-a-cursor")
