import csv
import io
from datetime import datetime, timedelta, timezone

from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.tenancy import WorkspaceCtx
from apps.api.database import Base, get_db
from apps.api.routers.governance import require_admin, router
from apps.api.services.governance.models import GovernanceAuditEvent


class User:
    id = 1


def test_audit_event_cursor_is_stable_complete_and_tenant_safe():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[GovernanceAuditEvent.__table__])
    Session = sessionmaker(bind=engine)
    now = datetime.now(timezone.utc)
    with Session() as db:
        db.add_all([
            GovernanceAuditEvent(
                id=f"event-{index}", workspace_id="ws", actor_role="admin",
                method="POST", route="/same", resource_path="/same",
                response_status=200, outcome="success", request_id=f"req-{index}",
                metadata_json={}, created_at=now,
            )
            for index in range(5)
        ])
        db.add(GovernanceAuditEvent(
            id="foreign", workspace_id="other", actor_role="admin",
            method="POST", route="/secret", resource_path="/secret",
            response_status=200, outcome="success", request_id="foreign",
            metadata_json={}, created_at=now,
        ))
        db.commit()

    app = FastAPI(); app.include_router(router)

    def override_db():
        with Session() as db:
            yield db

    app.dependency_overrides[get_db] = override_db
    app.dependency_overrides[require_admin] = lambda: WorkspaceCtx(User(), "ws", "ws")
    client = TestClient(app)

    first = client.get("/api/governance/audit-events?limit=2&method=POST").json()
    assert first["has_more"] is True and len(first["events"]) == 2
    assert "foreign" not in {event["id"] for event in first["events"]}
    with Session() as db:
        db.add(GovernanceAuditEvent(
            id="new-event", workspace_id="ws", actor_role="admin",
            method="POST", route="/new", resource_path="/new",
            response_status=200, outcome="success", request_id="new",
            metadata_json={}, created_at=now + timedelta(seconds=1),
        ))
        db.commit()
    second = client.get("/api/governance/audit-events", params={
        "limit": 2, "method": "POST", "cursor": first["next_cursor"],
    }).json()
    third = client.get("/api/governance/audit-events", params={
        "limit": 2, "method": "POST", "cursor": second["next_cursor"],
    }).json()
    traversed = first["events"] + second["events"] + third["events"]
    assert len(traversed) == 5
    assert len({event["id"] for event in traversed}) == 5
    assert "new-event" not in {event["id"] for event in traversed}
    assert client.get("/api/governance/audit-events?cursor=bad").status_code == 422


def test_audit_export_is_tenant_scoped_filtered_and_includes_safe_metadata():
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[GovernanceAuditEvent.__table__])
    Session = sessionmaker(bind=engine)
    now = datetime.now(timezone.utc)
    with Session() as db:
        db.add_all([
            GovernanceAuditEvent(workspace_id="ws", actor_user_id=1, actor_role="admin", method="DELETE", route="/rows", resource_path="/rows", response_status=200, outcome="success", request_id="req-1", metadata_json={"deleted_count": 3}, created_at=now),
            GovernanceAuditEvent(workspace_id="ws", actor_user_id=2, actor_role="admin", method="POST", route="/other", resource_path="/other", response_status=200, outcome="success", request_id="req-2", metadata_json={}, created_at=now),
            GovernanceAuditEvent(workspace_id="other", actor_user_id=1, actor_role="admin", method="DELETE", route="/secret", resource_path="/secret", response_status=200, outcome="success", request_id="req-3", metadata_json={"hidden": True}, created_at=now),
        ])
        db.commit()

    app = FastAPI(); app.include_router(router)
    def override_db():
        with Session() as db:
            yield db
    app.dependency_overrides[get_db] = override_db
    app.dependency_overrides[require_admin] = lambda: WorkspaceCtx(User(), "ws", "ws")

    response = TestClient(app).get("/api/governance/audit-events/export.csv?method=DELETE&actor_user_id=1")

    assert response.status_code == 200, response.text
    rows = list(csv.reader(io.StringIO(response.content.decode("utf-8-sig"))))
    assert rows[0][-1] == "metadata_json"
    assert len(rows) == 2
    assert rows[1][9] == "req-1"
    assert rows[1][10] == '{"deleted_count":3}'
    assert "secret" not in response.text
