from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.services.leadgen import store as store_module
from apps.api.services.leadgen.store import PgLeadStore


def test_shared_llm_usage_aggregate_is_atomic_and_tenant_scoped(monkeypatch):
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine)
    sessions = sessionmaker(bind=engine, expire_on_commit=False)
    monkeypatch.setattr(store_module, "SessionLocal", sessions)

    first = PgLeadStore("workspace-a")
    second = PgLeadStore("workspace-b")
    first.record_llm_usage("anthropic", "claude", 10, 5, rate_limit=100, rate_remaining=99)
    first.record_llm_usage("anthropic", "claude", 7, 3)
    second.record_llm_usage("anthropic", "claude", 100, 50)

    first_rows = first.get_llm_usage()
    assert len(first_rows) == 1
    assert first_rows[0]["calls"] == 2
    assert first_rows[0]["prompt_tokens"] == 17
    assert first_rows[0]["completion_tokens"] == 8
    assert first_rows[0]["total_tokens"] == 25
    assert first_rows[0]["rate_limit"] == 100
    assert first_rows[0]["rate_remaining"] == 99
    assert first.get_llm_usage_total() == {"total_calls": 2, "total_tokens": 25}

    second_rows = second.get_llm_usage()
    assert len(second_rows) == 1
    assert second_rows[0]["calls"] == 1
    assert second.get_llm_usage_total() == {"total_calls": 1, "total_tokens": 150}
