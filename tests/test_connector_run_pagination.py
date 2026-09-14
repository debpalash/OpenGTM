import asyncio
from datetime import datetime, timedelta, timezone

import pytest
from fastapi import HTTPException
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.routers.workbooks import list_connector_runs
from apps.api.services.workbook.models import ConnectorRun, Workbook


def test_connector_run_history_is_stable_bounded_and_tenant_safe():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[Workbook.__table__, ConnectorRun.__table__])
    Session = sessionmaker(bind=engine)
    created = datetime.now(timezone.utc) - timedelta(days=1)
    with Session() as db:
        db.add(Workbook(id="workbook", workspace_id="ws", name="Source"))
        db.add_all([
            ConnectorRun(
                id=f"connector-{index}", workspace_id="ws", workbook_id="workbook",
                connector="source", status="complete", created_at=created,
            )
            for index in range(5)
        ])
        db.add(ConnectorRun(
            id="foreign-connector", workspace_id="other", workbook_id="workbook",
            connector="source", status="complete", created_at=created + timedelta(days=2),
        ))
        db.commit()
        ctx = type("Ctx", (), {"workspace_id": "ws"})()

        first = asyncio.run(list_connector_runs(
            "workbook", db=db, ctx=ctx, limit=2, offset=0, cursor=None,
        ))
        assert first["has_more"] is True and len(first["runs"]) == 2
        assert "foreign-connector" not in {run["id"] for run in first["runs"]}

        db.add(ConnectorRun(
            id="new-connector", workspace_id="ws", workbook_id="workbook",
            connector="source", status="complete", created_at=created + timedelta(days=3),
        ))
        db.commit()
        second = asyncio.run(list_connector_runs(
            "workbook", db=db, ctx=ctx, limit=2, offset=0, cursor=first["next_cursor"],
        ))
        third = asyncio.run(list_connector_runs(
            "workbook", db=db, ctx=ctx, limit=2, offset=0, cursor=second["next_cursor"],
        ))
        traversed = first["runs"] + second["runs"] + third["runs"]
        assert len(traversed) == 5
        assert len({run["id"] for run in traversed}) == 5
        assert "new-connector" not in {run["id"] for run in traversed}

        with pytest.raises(HTTPException, match="Invalid connector run cursor"):
            asyncio.run(list_connector_runs(
                "workbook", db=db, ctx=ctx, limit=2, offset=0, cursor="bad",
            ))
