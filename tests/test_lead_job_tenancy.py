"""Tenant isolation and durability invariants for legacy collection jobs."""

from inspect import unwrap
from types import SimpleNamespace

import pytest
from fastapi import HTTPException

from apps.api.core.tenancy import WorkspaceCtx
from apps.api.routers import leads
from apps.api.services.leadgen.db import LeadDB
from apps.api.services.queue_service import QueueService


def _ctx(workspace_id: str, slug: str) -> WorkspaceCtx:
    return WorkspaceCtx(
        user=SimpleNamespace(id=1), workspace_id=workspace_id, slug=slug
    )


def test_collection_jobs_use_authenticated_workspace_not_body(monkeypatch, tmp_path):
    paths = {
        "one": str(tmp_path / "one.db"),
        "two": str(tmp_path / "two.db"),
    }
    monkeypatch.setattr(
        "apps.api.services.workspace.manager.workspace_leads_db_path",
        lambda slug: paths[slug],
    )
    captured = {}

    def _enqueue(self, db, job_type, payload, priority=1, fire_key=None):
        captured.update(job_type=job_type, payload=payload, fire_key=fire_key)
        return SimpleNamespace(id=501)

    monkeypatch.setattr(QueueService, "add_job", _enqueue)
    from apps.api.services.leadgen.progress import ProgressBus
    monkeypatch.setattr(
        ProgressBus,
        "_publish_redis",
        lambda self, workspace_id, event: None,
    )

    ctx_one = _ctx("W1", "one")
    # A malicious legacy body tries to select W2. The authenticated context wins.
    result = unwrap(leads.start_collection)(
        None,
        leads.CollectRequest(query="staffing companies", workspace_id="W2"),
        ctx_one,
    )

    assert result["workspace_id"] == "W1"
    assert captured["payload"]["workspace_id"] == "W1"
    assert captured["payload"]["slug"] == "one"
    assert captured["fire_key"] == f"collect:W1:{result['job_id']}"

    one = LeadDB(paths["one"])
    row = one.get_job_detail(result["job_id"])
    one.close()
    assert row and row["workspace_id"] == "W1"

    two = LeadDB(paths["two"])
    assert two.get_job_detail(result["job_id"]) is None
    two.close()

    assert {j["id"] for j in leads.list_jobs(ctx=ctx_one)} == {result["job_id"]}
    assert leads.list_jobs(ctx=_ctx("W2", "two")) == []
    with pytest.raises(HTTPException) as exc:
        leads.get_job_detail(result["job_id"], ctx=_ctx("W2", "two"))
    assert exc.value.status_code == 404


def test_bare_domain_creates_neither_job_nor_queue_entry(monkeypatch, tmp_path):
    db_path = str(tmp_path / "one.db")
    monkeypatch.setattr(
        "apps.api.services.workspace.manager.workspace_leads_db_path",
        lambda slug: db_path,
    )

    def _unexpected_enqueue(*args, **kwargs):
        raise AssertionError("ambiguous domain reached the durable queue")

    monkeypatch.setattr(QueueService, "add_job", _unexpected_enqueue)
    result = unwrap(leads.start_collection)(
        None,
        leads.CollectRequest(query="stripe.com"),
        _ctx("W1", "one"),
    )

    assert result["ok"] is False
    assert result["clarification_required"] is True
    assert result["domain"] == "stripe.com"
    db = LeadDB(db_path)
    assert db.get_jobs() == []
    db.close()


def test_collection_failure_reconciliation_is_workspace_local(monkeypatch, tmp_path):
    from apps.api.services.leadgen.job_runner import reconcile_collect_job_failure

    paths = {
        "one": str(tmp_path / "one.db"),
        "two": str(tmp_path / "two.db"),
    }
    monkeypatch.setattr(
        "apps.api.services.workspace.manager.workspace_leads_db_path",
        lambda slug: paths[slug],
    )
    db = LeadDB(paths["one"])
    db.create_job("job-one", "query")
    db.close()
    other = LeadDB(paths["two"])
    other.create_job("job-two", "other query")
    other.close()

    payload = {
        "job_id": "job-one",
        "workspace_id": "W1",
        "slug": "one",
    }
    reconcile_collect_job_failure(10, payload, "timeout", True)
    db = LeadDB(paths["one"])
    assert db.get_job_detail("job-one")["status"] == "pending"
    db.close()

    reconcile_collect_job_failure(10, payload, "timeout", False)
    db = LeadDB(paths["one"])
    failed = db.get_job_detail("job-one")
    db.close()
    assert failed["status"] == "failed"
    assert "timeout" in failed["error"]

    other = LeadDB(paths["two"])
    assert other.get_job_detail("job-two")["status"] == "pending"
    other.close()


def test_sse_rejects_foreign_workspace(monkeypatch):
    from apps.api.services.workspace import manager as ws_manager

    monkeypatch.setattr(
        leads, "_authenticate_query_token", lambda token: SimpleNamespace(id=7)
    )
    monkeypatch.setattr(ws_manager, "is_member", lambda workspace_id, user_id: False)

    with pytest.raises(HTTPException) as exc:
        leads.sse_events(token="valid", workspace_id="foreign")
    assert exc.value.status_code == 403
