from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.routers.audiences import list_audience_accounts
from apps.api.services.audiences.models import Audience, AudienceMember
from apps.api.services.leadgen.orm_models import SignalRow


def test_account_rollup_groups_domains_and_is_tenant_scoped():
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[Audience.__table__, AudienceMember.__table__, SignalRow.__table__])
    db = sessionmaker(bind=engine)()
    db.add(Audience(id="aud", workspace_id="ws", name="Target", filters={}))
    db.add_all([
        AudienceMember(workspace_id="ws", audience_id="aud", lead_id=1, snapshot={"company": "Acme", "website": "https://www.acme.test/a", "contact_person": "Ada", "contact_title": "VP Sales", "email": "ada@acme.test", "score": 90}),
        AudienceMember(workspace_id="ws", audience_id="aud", lead_id=2, snapshot={"company": "Acme Inc", "website": "acme.test", "contact_person": "Bob", "phone": "1", "score": 70}),
        SignalRow(id="s1", workspace_id="ws", lead_id=1, signal_type="hiring", weight=8, created_at=1),
        SignalRow(id="s2", workspace_id="other", lead_id=1, signal_type="funding", weight=100, created_at=1),
    ]); db.commit()
    ctx = type("Ctx", (), {"workspace_id": "ws"})()
    result = list_audience_accounts("aud", db=db, ctx=ctx)
    assert result["summary"] == {"account_count": 1, "contact_count": 2, "accounts_with_signals": 1, "accounts_with_decision_makers": 1}
    account = result["accounts"][0]
    assert account["key"] == "acme.test" and account["contacts"] == 2
    assert account["avg_score"] == 80 and account["email_coverage_pct"] == 50 and account["phone_coverage_pct"] == 50
    assert account["signal_count"] == 1 and account["signal_weight"] == 8
    db.close()
