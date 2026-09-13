import time

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.routers.signals import signal_analytics
from apps.api.services.leadgen.orm_models import SignalRow


def test_signal_analytics_is_tenant_scoped_and_weighted():
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[SignalRow.__table__])
    db = sessionmaker(bind=engine)()
    now = time.time()
    db.add_all([
        SignalRow(id="1", workspace_id="ws", lead_id=1, company="Acme", signal_type="hiring", source="jobs", weight=8, created_at=now),
        SignalRow(id="2", workspace_id="ws", lead_id=1, company="Acme", signal_type="funding", source="news", weight=10, created_at=now),
        SignalRow(id="3", workspace_id="other", lead_id=2, company="Other", signal_type="funding", source="news", weight=100, created_at=now),
        SignalRow(id="4", workspace_id="ws", lead_id=3, company="Old", signal_type="news", source="news", weight=5, created_at=now - 100 * 86400),
    ]); db.commit()
    ctx = type("Ctx", (), {"workspace_id": "ws"})()
    result = signal_analytics(days=30, db=db, ctx=ctx)
    assert result["summary"] == {"total": 2, "weighted_score": 18, "active_accounts": 1, "momentum_pct": None}
    assert result["top_accounts"][0] == {"lead_id": 1, "company": "Acme", "count": 2, "weight": 18}
    assert {row["signal_type"] for row in result["by_type"]} == {"hiring", "funding"}
    assert len(result["trend"]) == 30 and result["trend"][-1]["count"] == 2
    db.close()
