from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.models import Job
from apps.api.services.workbook.batch_attempts import claim_batch_attempt, acknowledge_batch_attempt


@pytest.fixture
def batch_job(tmp_path):
    engine = create_engine(f"sqlite:///{tmp_path}/batch.db")
    Job.__table__.create(engine)
    sessions = sessionmaker(bind=engine)
    now = datetime.now(timezone.utc)
    owner = dict(job_id=1, workspace_id="one", workbook_id="book", worker_id="worker", locked_at=now)
    with sessions() as db:
        db.add(Job(id=1, type="run_workbook", workspace_id="one", status="processing",
                   worker_id="worker", locked_at=now, payload={"workbook_id": "book", "keep": True}))
        db.commit()
    yield sessions, owner
    engine.dispose()


def test_competing_batch_claims_authorize_only_one_dispatch(batch_job):
    sessions, owner = batch_job
    def claim(_):
        return claim_batch_attempt(sessions, contract={"requests": ["row:1"]}, **owner)
    with ThreadPoolExecutor(max_workers=2) as pool:
        results = list(pool.map(claim, range(2)))
    assert sorted(item["action"] for item in results) == ["dispatch", "reconcile"]
    assert claim(0)["action"] == "reconcile"
    with sessions() as db:
        assert db.get(Job, 1).payload["keep"] is True


def test_output_claim_prevents_ambiguous_replay_and_reuses_recorded_result(batch_job):
    from apps.api.services.workbook.output_attempts import claim_output_attempt, record_output_result
    sessions, owner = batch_job
    def claim(_):
        return claim_output_attempt(sessions, cell_key="row:1/push", contract={"destination": "fixture", "input": "same"}, **owner)
    with ThreadPoolExecutor(max_workers=2) as pool:
        claims = list(pool.map(claim, range(2)))
    assert sorted(c["action"] for c in claims) == ["dispatch", "reconcile"]
    receipt = {"success": True, "value": "receipt-1", "error": None}
    args = {**owner, "cell_key": "row:1/push", "contract_hash": claims[0]["contract_hash"]}
    record_output_result(sessions, result=receipt, **args)
    assert claim(0)["action"] == "reuse"
    assert claim(0)["result"] == receipt
    with pytest.raises(ValueError, match="conflicts"):
        record_output_result(sessions, result={**receipt, "value": "different"}, **args)
    with pytest.raises(ValueError, match="contract changed"):
        claim_output_attempt(sessions, cell_key="row:1/push", contract={"input": "changed"}, **owner)
    with pytest.raises(ValueError, match="lease"):
        claim_output_attempt(sessions, cell_key="row:2/push", contract={"input": "same"}, **{**owner, "worker_id": "stale"})
    with sessions() as db:
        assert db.get(Job, 1).payload["keep"] is True
        assert "destination" not in db.get(Job, 1).payload["output_attempts"]["row:1/push"]


@pytest.mark.parametrize("lost_ack", [False, True])
def test_output_dispatch_replay_never_resends(batch_job, lost_ack):
    import asyncio
    from apps.api.services.workbook.batch_attempts import batch_owner
    from apps.api.services.workbook.output_attempts import execute_output_with_journal
    sessions, owner = batch_job
    sends = []
    request = dict(workbook_id="book", workspace_id="one", lead_id=1,
                   lead_data={"__row_id": 1, "email": "fixture@example.test"},
                   col_config={"id": "push", "run_once": False}, columns_config=[])

    async def destination(**kwargs):
        # An independent writer proves the claim transaction ended before I/O.
        with sessions() as db:
            db.get(Job, 1).error = "destination entered"
            db.commit()
        sends.append(kwargs)
        if lost_ack:
            raise TimeoutError("Destination may have accepted the send")
        return {"success": True, "value": "delivery-1"}

    async def run():
        token = batch_owner.set(owner)
        try:
            if lost_ack:
                with pytest.raises(TimeoutError):
                    await execute_output_with_journal(sessions, destination, **request)
            else:
                assert (await execute_output_with_journal(sessions, destination, **request))["success"]
            replay = await execute_output_with_journal(sessions, destination, **request)
            assert replay == ({"success": False, "value": None, "error": "output_delivery_requires_review"}
                              if lost_ack else {"success": True, "value": "delivery-1", "error": None})
            with pytest.raises(ValueError, match="contract changed"):
                await execute_output_with_journal(sessions, destination, **{**request, "lead_data": {"__row_id": 1, "email": "changed@example.test"}})
        finally:
            batch_owner.reset(token)
    asyncio.run(run())
    assert len(sends) == 1


def test_run_receipt_serializes_before_read_and_preserves_output_claims(batch_job, monkeypatch):
    from sqlalchemy import event
    from apps.api.services.workbook import run_receipts
    from apps.api.services.workbook.output_attempts import claim_output_attempt
    sessions, owner = batch_job
    monkeypatch.setattr(run_receipts, "SessionLocal", sessions)
    payload = {"workbook_id": "book", "workspace_id": "one", "__queue_lease": {
        "worker_id": "worker", "locked_at": owner["locked_at"].isoformat()}}
    result = {"completed": 1, "errors": 0, "total": 1, "rows": 1, "stopped": False}
    statements = []
    engine = sessions.kw["bind"]
    def capture(conn, cursor, statement, parameters, context, executemany):
        statements.append(statement.lstrip().split()[0].upper())
    event.listen(engine, "before_cursor_execute", capture)
    try:
        assert run_receipts.persist_run_result(1, payload, result)
    finally:
        event.remove(engine, "before_cursor_execute", capture)
    assert statements[0] == "UPDATE", "Must acquire writer ownership before reading JSON"
    def write(index):
        if index % 2:
            return run_receipts.persist_run_result(1, payload, result)
        return claim_output_attempt(sessions, cell_key=f"row:{index}/push", contract={"input": index}, **owner)
    with ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(write, range(20)))
    with sessions() as db:
        saved = db.get(Job, 1).payload
        assert saved["keep"] is True
        assert saved["execution_result"]["completed"] == 1
        assert set(saved["output_attempts"]) == {f"row:{i}/push" for i in range(0, 20, 2)}


def test_acknowledged_batch_retrieves_and_rejects_changed_identity(batch_job):
    sessions, owner = batch_job
    receipt = claim_batch_attempt(sessions, contract={"requests": [1]}, **owner)
    for _ in range(2):
        acknowledge_batch_attempt(sessions, contract_hash=receipt["contract_hash"], vendor_batch_id="vendor-1", **owner)
    assert claim_batch_attempt(sessions, contract={"requests": [1]}, **owner)["action"] == "retrieve"
    with pytest.raises(ValueError, match="different vendor"):
        acknowledge_batch_attempt(sessions, contract_hash=receipt["contract_hash"], vendor_batch_id="vendor-2", **owner)
    with pytest.raises(ValueError, match="contract changed"):
        claim_batch_attempt(sessions, contract={"requests": [2]}, **owner)


def test_incremental_results_merge_without_overwriting_committed_answers(batch_job):
    from apps.api.services.workbook.batch_attempts import checkpoint_batch_results
    sessions, owner = batch_job
    receipt = claim_batch_attempt(sessions, contract={"requests": [1, 2]}, **owner)
    args = {**owner, "contract_hash": receipt["contract_hash"]}
    acknowledge_batch_attempt(sessions, vendor_batch_id="vendor-1", **args)
    for result in ({"1": "First"}, {"2": "Second"}, {"1": "First"}):
        checkpoint_batch_results(sessions, results=result, **args)
    with pytest.raises(ValueError, match="conflicts"):
        checkpoint_batch_results(sessions, results={"1": "Changed"}, **args)
    with sessions() as db:
        assert db.get(Job, 1).payload["ai_batch_attempt"]["results"] == {"1": "First", "2": "Second"}


@pytest.mark.parametrize("change", [{"workspace_id": "other"}, {"workbook_id": "other"}, {"worker_id": "stale"}])
def test_foreign_or_stale_batch_owner_cannot_claim(batch_job, change):
    sessions, owner = batch_job
    with pytest.raises(ValueError):
        claim_batch_attempt(sessions, contract={"requests": [1]}, **{**owner, **change})
    with sessions() as db:
        assert "ai_batch_attempt" not in db.get(Job, 1).payload


def test_reclaimed_job_retains_claim_and_rejects_old_acknowledgement(batch_job):
    from datetime import timedelta
    sessions, owner = batch_job
    receipt = claim_batch_attempt(sessions, contract={"requests": [1]}, **owner)
    next_owner = {**owner, "worker_id": "replacement", "locked_at": owner["locked_at"] + timedelta(seconds=60)}
    with sessions() as db:
        job = db.get(Job, 1)
        job.worker_id, job.locked_at = next_owner["worker_id"], next_owner["locked_at"]
        db.commit()
    with pytest.raises(ValueError, match="lease"):
        acknowledge_batch_attempt(sessions, contract_hash=receipt["contract_hash"], vendor_batch_id="late-ack", **owner)
    assert claim_batch_attempt(sessions, contract={"requests": [1]}, **next_owner)["action"] == "reconcile"


@pytest.mark.parametrize("failure", ["lost_ack", "read_error", "partial", "stale_writer", "cell_write", "partial_stream"])
def test_queued_prepass_reuses_claim_after_uncertain_outcome(batch_job, monkeypatch, failure):
    import asyncio
    from apps.api.services.workbook import enrichment as enr
    from apps.api.services.workbook.execution_identity import execution_scope
    from apps.api.services.workbook.batch_attempts import batch_lease_scope, BatchRecoveryRequired
    sessions, owner = batch_job
    monkeypatch.setattr(enr, "SessionLocal", sessions)
    monkeypatch.setattr(enr, "BATCH_ENABLED", True)
    monkeypatch.setattr(enr, "BATCH_MIN_ROWS", 1)
    monkeypatch.setattr(enr.llm, "anthropic_provider", lambda: {"id": "fixture", "model": "fixture-model"})
    writes, calls, creates = [], [], []
    failed_write = False
    def write(*args, **kwargs):
        nonlocal failed_write
        if failure == "cell_write" and not failed_write:
            failed_write = True
            raise RuntimeError("Fixture cell storage unavailable")
        writes.append(args)
    monkeypatch.setattr(enr, "_set_enrichment", write)
    async def batch(requests, **options):
        calls.append(options["batch_id"])
        assert options["strict_results"] is True
        if failure == "partial_stream" and options["batch_id"] is not None:
            assert options["known_result_ids"] == ["1::ai"]
        if options["batch_id"] is None:
            creates.append("vendor-1")
            if failure == "lost_ack":
                raise TimeoutError("Acknowledgement lost")
            options["on_submitted"]("vendor-1")
            if failure == "partial_stream":
                options["on_result"]("1::ai", "Evidence-backed result")
                raise TimeoutError("Stream interrupted after first answer")
            if failure == "cell_write":
                return {request["custom_id"]: "Checkpointed result" for request in requests}
            if failure == "stale_writer":
                with sessions() as db:
                    db.get(Job, 1).worker_id = "replacement"
                    db.commit()
                return {request["custom_id"]: "Stale result" for request in requests}
            if failure == "read_error":
                raise TimeoutError("Results unavailable")
            return {}
        return {request["custom_id"]: "Evidence-backed result" for request in requests}
    monkeypatch.setattr(enr.llm, "batch_complete_anthropic", batch)
    column = {"id": "ai", "type": "ai_formula", "prompt": "Describe {company}"}
    payload = {"workspace_id": "one", "workbook_id": "book", "__queue_lease": {
        "worker_id": "worker", "locked_at": owner["locked_at"].isoformat()}}
    async def run():
        with execution_scope("one", "book", 1), batch_lease_scope(1, payload):
            work = [({"id": 1, "company": "Example"}, [column])]
            if failure == "partial_stream":
                work.append(({"id": 2, "company": "Second"}, [column]))
            return await enr._run_ai_batch_prepass("book", work, [column], None)
    with pytest.raises(RuntimeError):
        asyncio.run(run())
    assert not writes
    if failure == "partial_stream":
        with sessions() as db:
            assert db.get(Job, 1).payload["ai_batch_attempt"]["results"] == {"1::ai": "Evidence-backed result"}
    if failure == "stale_writer":
        assert calls == [None]
        with sessions() as db:
            assert db.get(Job, 1).worker_id == "replacement"
            assert db.get(Job, 1).payload["ai_batch_attempt"]["vendor_batch_id"] == "vendor-1"
    elif failure == "lost_ack":
        with pytest.raises(BatchRecoveryRequired, match="outcome unknown"):
            asyncio.run(run())
        assert calls == [None]
    else:
        count = 2 if failure == "partial_stream" else 1
        assert asyncio.run(run()) == ({(i, "ai") for i in range(1, count + 1)}, count)
        assert calls == ([None] if failure == "cell_write" else [None, "vendor-1"])
        assert len(writes) == count
        with sessions() as db:
            assert "1::ai" in db.get(Job, 1).payload["ai_batch_attempt"]["results"]
    assert creates == ["vendor-1"]


@pytest.mark.parametrize("change", ["disabled", "provider", "threshold", "columns", "empty_scope"])
def test_existing_batch_cannot_be_bypassed_by_changed_eligibility(batch_job, monkeypatch, change):
    import asyncio
    from apps.api.services.workbook import enrichment as enr
    from apps.api.services.workbook.models import Workbook
    from apps.api.services.workbook.execution_identity import execution_scope
    from apps.api.services.workbook.batch_attempts import batch_lease_scope, BatchRecoveryRequired
    sessions, owner = batch_job
    claim_batch_attempt(sessions, contract={"original": "accepted input"}, **owner)
    monkeypatch.setattr(enr, "SessionLocal", sessions)
    monkeypatch.setattr(enr, "BATCH_ENABLED", change != "disabled")
    monkeypatch.setattr(enr, "BATCH_MIN_ROWS", 10 if change == "threshold" else 1)
    monkeypatch.setattr(enr.llm, "anthropic_provider", lambda: None if change == "provider" else {"id": "fixture", "model": "fixture"})
    async def forbidden(*args, **kwargs):
        pytest.fail("Existing batch must not fall through to another execution path")
    monkeypatch.setattr(enr.llm, "batch_complete_anthropic", forbidden)
    monkeypatch.setattr(enr, "_run_one_row", forbidden)
    column = {"id": "ai", "type": "formula" if change == "columns" else "ai_formula", "prompt": "Describe {company}"}
    if change == "empty_scope":
        with sessions() as db:
            Workbook.__table__.create(db.get_bind())
            db.add(Workbook(id="book", workspace_id="one", name="Empty", columns_config=[], status="running"))
            db.commit()
        monkeypatch.setattr(enr, "_load_workbook_leads", lambda *args: [])
    payload = {"workspace_id": "one", "workbook_id": "book", "__queue_lease": {
        "worker_id": "worker", "locked_at": owner["locked_at"].isoformat()}}
    async def run():
        with execution_scope("one", "book", 1), batch_lease_scope(1, payload):
            if change == "empty_scope":
                return await enr._run_workbook_enrichment_impl("book")
            return await enr._run_ai_batch_prepass("book", [({"id": 1, "company": "Example"}, [column])], [column], None)
    with pytest.raises(BatchRecoveryRequired):
        asyncio.run(run())
    with sessions() as db:
        assert db.get(Job, 1).payload["ai_batch_attempt"]["state"] == "dispatch_claimed"
        if change == "empty_scope":
            assert db.get(Workbook, "book").status == "running"  # Not falsely completed.


@pytest.mark.parametrize("loss", ["worker", "timestamp", "cancelled"])
def test_stale_runner_cannot_overwrite_workbook_progress_or_status(batch_job, monkeypatch, loss):
    import asyncio
    from datetime import timedelta
    from apps.api.services.workbook import enrichment as enr
    from apps.api.services.workbook.models import Workbook
    from apps.api.services.workbook.batch_attempts import batch_lease_scope
    sessions, owner = batch_job
    with sessions() as db:
        Workbook.__table__.create(db.get_bind())
        db.add(Workbook(id="book", workspace_id="one", name="Workbook", status="running",
                       columns_config=[{"id": "email", "type": "waterfall", "waterfall": []}]))
        db.commit()
    monkeypatch.setattr(enr, "SessionLocal", sessions)
    monkeypatch.setattr(enr, "_load_workbook_leads", lambda *args: [{"id": 1}])
    broadcasts = []
    class RedisFixture:
        closed = False
        async def aclose(self): self.closed = True
    redis = RedisFixture()
    monkeypatch.setattr(enr, "_make_redis", lambda: redis)
    async def broadcast(*args): broadcasts.append(args)
    monkeypatch.setattr(enr, "_broadcast", broadcast)
    async def prepass(*args, **kwargs): return set(), 0
    monkeypatch.setattr(enr, "_run_ai_batch_prepass", prepass)
    async def row(*args, **kwargs):
        with sessions() as db:
            job = db.get(Job, 1)
            if loss == "worker": job.worker_id = "replacement"
            elif loss == "timestamp": job.locked_at = owner["locked_at"] + timedelta(seconds=60)
            else: job.status = "cancelled"
            workbook = db.get(Workbook, "book")
            workbook.status = "paused" if loss == "cancelled" else "running"
            workbook.completed_rows, workbook.total_rows = 7, 9
            db.commit()
        return {"completed": 1, "errors": 0}
    monkeypatch.setattr(enr, "_run_one_row", row)
    payload = {"workspace_id": "one", "workbook_id": "book", "__queue_lease": {
        "worker_id": "worker", "locked_at": owner["locked_at"].isoformat()}}
    async def run():
        with batch_lease_scope(1, payload):
            await enr._run_workbook_enrichment_impl("book", job_id=1)
    with pytest.raises(ValueError, match="lease"):
        asyncio.run(run())
    with sessions() as db:
        workbook = db.get(Workbook, "book")
        assert (workbook.completed_rows, workbook.total_rows) == (7, 9)
        assert workbook.status == ("paused" if loss == "cancelled" else "running")
    assert redis.closed
    assert not broadcasts


@pytest.mark.parametrize("state", ["active", "replaced", "cancelled"])
def test_cell_entry_checks_owner_and_releases_lock_before_execution(batch_job, monkeypatch, state):
    import asyncio
    from apps.api.services.workbook import enrichment as enr
    from apps.api.services.workbook.batch_attempts import batch_lease_scope
    sessions, owner = batch_job
    monkeypatch.setattr(enr, "SessionLocal", sessions)
    if state != "active":
        with sessions() as db:
            job = db.get(Job, 1)
            if state == "replaced": job.worker_id = "replacement"
            else: job.status = "cancelled"
            db.commit()
    calls = []
    async def cell(**kwargs):
        calls.append(kwargs["col_id"])
        # Independent writer during execution proves the entry-check transaction
        # has ended; provider I/O must not hold SQLite's writer lock.
        with sessions() as db:
            db.get(Job, 1).error = "Execution entered"
            db.commit()
        return {"success": True, "value": "result"}
    monkeypatch.setattr(enr, "enrich_cell", cell)
    payload = {"workspace_id": "one", "workbook_id": "book", "__queue_lease": {
        "worker_id": "worker", "locked_at": owner["locked_at"].isoformat()}}
    async def run():
        with batch_lease_scope(1, payload):
            return await enr._run_one_cell("book", {"id": 1}, {"id": "email"}, [], None)
    result = asyncio.run(run())
    if state == "active":
        assert result["success"] is True
        assert calls == ["email"]
    else:
        assert result["success"] is False
        assert "lease" in result["error"]
        assert not calls


@pytest.mark.parametrize("recorded", [False, True])
def test_queue_retry_preserves_output_attempt(batch_job, monkeypatch, recorded):
    import asyncio
    from apps.api.services import queue_service, job_process_runner
    from apps.api.services.workbook.batch_attempts import batch_lease_scope
    from apps.api.services.workbook.output_attempts import execute_output_with_journal
    sessions, owner = batch_job
    monkeypatch.setattr(queue_service, "SessionLocal", sessions)
    queue = queue_service.QueueService()
    queue.worker_id = "worker"
    queue.register_handler("run_workbook", lambda *_: None)
    with sessions() as db:
        db.get(Job, 1).max_retries = 1
        db.get(Job, 1).payload = {"workbook_id": "book", "workspace_id": "one"}
        db.commit()
    sends, results = [], []
    request = dict(workbook_id="book", workspace_id="one", lead_id=1,
                   lead_data={"__row_id": 1}, col_config={"id": "push"}, columns_config=[])
    async def destination(**kwargs):
        sends.append(kwargs)
        if not recorded:
            raise TimeoutError("Lost delivery acknowledgement")
        return {"success": True, "value": "receipt-1"}
    async def child(job_id, job_type, payload, **kwargs):
        with batch_lease_scope(job_id, payload):
            results.append(await execute_output_with_journal(sessions, destination, **request))
        if len(results) == 1 and recorded:
            raise RuntimeError("Worker died after recording delivery")
    monkeypatch.setattr(job_process_runner, "run_job_subprocess", child)
    asyncio.run(queue._process_job(1, "run_workbook", {"workbook_id": "book", "workspace_id": "one"}, locked_at=owner["locked_at"]))
    with sessions() as db:
        job = db.get(Job, 1)
        assert job.status == "pending"
        assert job.payload["output_attempts"]["row:1/push"]["state"] == ("recorded" if recorded else "dispatch_claimed")
        job.next_run_at = None
        db.commit()
    retry = queue.claim_next_job()
    assert retry["id"] == 1
    asyncio.run(queue._process_job(1, "run_workbook", retry["payload"], locked_at=retry["locked_at"]))
    assert len(sends) == 1
    assert results[-1] == ({"success": True, "value": "receipt-1", "error": None} if recorded else
                           {"success": False, "value": None, "error": "output_delivery_requires_review"})


def test_lost_output_lease_cannot_record_or_resend(batch_job):
    import asyncio
    from apps.api.services.workbook.batch_attempts import batch_owner
    from apps.api.services.workbook.output_attempts import execute_output_with_journal
    sessions, owner = batch_job
    sends = []
    request = dict(workbook_id="book", workspace_id="one", lead_id=1,
                   lead_data={"__row_id": 1}, col_config={"id": "push"}, columns_config=[])
    async def destination(**kwargs):
        sends.append(kwargs)
        with sessions() as db:
            db.get(Job, 1).worker_id = "replacement"
            db.commit()
        return {"success": True, "value": "receipt-1"}
    async def run():
        token = batch_owner.set(owner)
        try:
            with pytest.raises(ValueError, match="lease"):
                await execute_output_with_journal(sessions, destination, **request)
        finally:
            batch_owner.reset(token)
        token = batch_owner.set({**owner, "worker_id": "replacement"})
        try:
            result = await execute_output_with_journal(sessions, destination, **request)
            assert result["error"] == "output_delivery_requires_review"
        finally:
            batch_owner.reset(token)
    asyncio.run(run())
    assert len(sends) == 1
    with sessions() as db:
        assert db.get(Job, 1).payload["output_attempts"]["row:1/push"]["state"] == "dispatch_claimed"


@pytest.mark.parametrize("acknowledged", [False, True])
def test_queue_retry_preserves_batch_claim(batch_job, monkeypatch, acknowledged):
    import asyncio
    from apps.api.services import queue_service, job_process_runner
    from apps.api.services.workbook.batch_attempts import BatchRecoveryRequired
    sessions, owner = batch_job
    monkeypatch.setattr(queue_service, "SessionLocal", sessions)
    queue = queue_service.QueueService()
    queue.worker_id = "worker"
    queue.register_handler("run_workbook", lambda *_: None)
    with sessions() as db:
        db.get(Job, 1).max_retries = 1
        db.commit()
    creates, actions = [], []
    async def child(job_id, job_type, payload, **kwargs):
        lease = payload["__queue_lease"]
        claim = claim_batch_attempt(sessions, contract={"requests": [1]}, job_id=job_id,
            workspace_id="one", workbook_id="book", worker_id=lease["worker_id"],
            locked_at=datetime.fromisoformat(lease["locked_at"]))
        actions.append(claim["action"])
        if claim["action"] == "dispatch":
            creates.append("vendor accepted but acknowledgement lost")
            if acknowledged:
                acknowledge_batch_attempt(sessions, contract_hash=claim["contract_hash"], vendor_batch_id="vendor-1",
                    job_id=job_id, workspace_id="one", workbook_id="book", worker_id=lease["worker_id"],
                    locked_at=datetime.fromisoformat(lease["locked_at"]))
            raise TimeoutError("Lost batch acknowledgement")
        if claim["action"] == "retrieve":
            assert claim["vendor_batch_id"] == "vendor-1"
            return
        raise BatchRecoveryRequired("Reconcile existing batch")
    monkeypatch.setattr(job_process_runner, "run_job_subprocess", child)
    asyncio.run(queue._process_job(1, "run_workbook", {"workbook_id": "book"}, locked_at=owner["locked_at"]))
    with sessions() as db:
        job = db.get(Job, 1)
        assert job.status == "pending"
        assert job.payload["ai_batch_attempt"]["state"] == ("accepted" if acknowledged else "dispatch_claimed")
        job.next_run_at = None
        db.commit()
    retry = queue.claim_next_job()
    assert retry["id"] == 1
    asyncio.run(queue._process_job(1, "run_workbook", retry["payload"], locked_at=retry["locked_at"]))
    assert actions == ["dispatch", "retrieve" if acknowledged else "reconcile"]
    assert len(creates) == 1
    with sessions() as db:
        job = db.get(Job, 1)
        assert job.status == ("completed" if acknowledged else "failed")
        assert job.payload["ai_batch_attempt"]["state"] == ("accepted" if acknowledged else "dispatch_claimed")
