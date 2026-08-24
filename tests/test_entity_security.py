from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.database import Base, get_db
from apps.api.routers import entities as entity_router
from apps.api.services.entities.models import (
    CompanyEntity,
    EntityBlockingKey,
    EntityMergeLog,
    EntityReviewPair,
)
from apps.api.services.workbook.models import Workbook, WorkbookRow


class User:
    id = 1


def _ctx(workspace_id="ws-1"):
    return WorkspaceCtx(user=User(), workspace_id=workspace_id, slug=workspace_id)


def _client():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(
        engine,
        tables=[
            CompanyEntity.__table__,
            EntityBlockingKey.__table__,
            EntityMergeLog.__table__,
            EntityReviewPair.__table__,
            Workbook.__table__,
            WorkbookRow.__table__,
        ],
    )
    Session = sessionmaker(bind=engine)
    app = FastAPI()
    app.include_router(entity_router.router)

    def db_override():
        with Session() as db:
            yield db

    app.dependency_overrides[get_db] = db_override
    app.dependency_overrides[current_workspace] = lambda: _ctx()
    app.dependency_overrides[entity_router.require_editor] = lambda: _ctx()
    return TestClient(app), Session


def test_entity_reads_are_forced_to_current_workspace():
    client, Session = _client()
    with Session() as db:
        mine = CompanyEntity(workspace_id="ws-1", canonical_name="Mine")
        foreign = CompanyEntity(workspace_id="ws-2", canonical_name="Foreign")
        db.add_all([mine, foreign])
        db.commit()
        mine_id, foreign_id = mine.id, foreign.id

    listing = client.get("/api/entities/company")
    assert listing.status_code == 200
    assert [row["id"] for row in listing.json()["entities"]] == [mine_id]
    assert client.get(f"/api/entities/company/{foreign_id}").status_code == 404


def test_cross_workspace_merge_is_rejected():
    client, Session = _client()
    with Session() as db:
        mine = CompanyEntity(workspace_id="ws-1", canonical_name="Mine")
        foreign = CompanyEntity(workspace_id="ws-2", canonical_name="Foreign")
        db.add_all([mine, foreign])
        db.commit()
        body = {"kept_id": mine.id, "merged_id": foreign.id}

    response = client.post("/api/entities/merge", json=body)
    assert response.status_code == 400
    assert response.json()["detail"] == "cross_workspace_merge_forbidden"
