"""The collection job ledger on PostgreSQL (PG-gated), plus flag unit tests.

    TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
        uv run pytest tests/test_multihost_ledger_pg.py

Proves: the PostgreSQL ledger behaves like ``LeadDB``'s job/stage API (the same
operation sequence yields the same observable state on both), racing workers on
separate connections claim each job exactly once, two "hosts" see one ledger,
forced RLS confines a connection to its workspace, and the backfill from real
SQLite ledger files is attributed correctly, idempotent and verifiable.
"""
import json
import sqlite3
import threading
import time
from pathlib import Path

import pytest
from sqlalchemy import create_engine, text

from apps.api.services.leadgen import ledger as ledger_mod
from apps.api.services.leadgen import ledger_backfill
from apps.api.services.workspace import pg_meta
from tests.multihost_support import TEST_DATABASE_URL, temp_database

needs_pg = pytest.mark.skipif(not TEST_DATABASE_URL, reason="TEST_DATABASE_URL not set")

# ── flag handling (no database) ─────────────────────────────────────────────


def test_default_ledger_is_the_sqlite_file(monkeypatch, tmp_path):
    from apps.api.core.config import settings
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.services.workspace import manager
    monkeypatch.setattr(settings, "COLLECTION_LEDGER_STORE", "sqlite")
    monkeypatch.setattr(manager, "workspace_leads_db_path", lambda slug: str(tmp_path / f"{slug}.db"))
    db = ledger_mod.open_job_ledger("ws-1", "acme")
    assert isinstance(db, LeadDB)
    db.close()


def test_postgres_ledger_requires_the_postgres_lead_store(monkeypatch):
    from apps.api.core.config import settings
    monkeypatch.setattr(settings, "COLLECTION_LEDGER_STORE", "postgres")
    with pytest.raises(RuntimeError, match="requires the PostgreSQL lead store"):
        ledger_mod.open_job_ledger("ws-1", "acme")   # the suite's database is SQLite


def test_unknown_ledger_backend_is_rejected(monkeypatch):
    from apps.api.core.config import settings
    monkeypatch.setattr(settings, "COLLECTION_LEDGER_STORE", "redis")
    with pytest.raises(ValueError):
        ledger_mod.backend()


def test_table_rewrite_only_touches_ledger_tables():
    rewrite = ledger_mod._TABLE_REWRITE
    sub = lambda s: rewrite.sub(lambda m: f"{m.group(1)}{m.group(2)}{ledger_mod._REWRITE[m.group(3).lower()]}", s)
    assert sub("SELECT status FROM jobs WHERE id = ?") == "SELECT status FROM collection_jobs WHERE id = ?"
    assert sub("UPDATE jobs SET status = 'failed'") == "UPDATE collection_jobs SET status = 'failed'"
    assert sub("SELECT * FROM job_stages WHERE job_id = ?") == "SELECT * FROM collection_job_stages WHERE job_id = ?"
    assert sub("SELECT 1 FROM collection_jobs") == "SELECT 1 FROM collection_jobs"
    assert sub("SELECT 1 FROM my_jobs") == "SELECT 1 FROM my_jobs"


# ── PostgreSQL ──────────────────────────────────────────────────────────────

@pytest.fixture(scope="module")
def pgdb():
    with temp_database() as d:
        yield d


@pytest.fixture
def pg(pgdb):
    owner = create_engine(pgdb.owner_url)
    with owner.begin() as c:
        c.execute(text("TRUNCATE collection_jobs, collection_job_stages, workspaces, workspace_members"))
    engine = create_engine(pgdb.app_url, pool_size=8, max_overflow=0)
    pg_meta.use_engine(engine)
    yield owner, engine
    pg_meta.use_engine(None)
    engine.dispose()
    owner.dispose()


def _norm_job(job):
    """Drop wall-clock fields so two backends can be compared."""
    job = dict(job)
    for k in ("created_at", "started_at", "completed_at"):
        job[k] = "T" if job.get(k) else ""
    job.pop("workspace_id", None)
    for st in job.get("stages", []):
        st.pop("id", None)
        for k in ("started_at", "completed_at"):
            st[k] = "T" if st.get(k) else ""
    return job


def _script(db, ids):
    """One operation sequence covering the whole job/stage surface."""
    out = []
    a, b, c = ids
    for jid, q in ((a, "plumbers in leeds"), (b, "dentists in york"), (c, "vets in hull")):
        db.create_job(jid, q, intent="market_search", intent_details=json.dumps({"k": 1}))
    db.create_job(a, "duplicate create is ignored")
    out.append(("jobs0", sorted((j["id"], j["query"], j["status"]) for j in db.get_jobs())))
    claimed = db.claim_job()
    out.append(("claimed", claimed["id"], claimed["status"] if "status" in claimed else None, claimed["attempts"]))
    s1 = db.create_stage(claimed["id"], "discover")
    s2 = db.create_stage(claimed["id"], "enrich")
    db.complete_stage(s1, input_count=5, output_count=4, rejected_count=1, details='{"a": 1}')
    db.complete_stage(s2, status="failed", details="{}")
    db.complete_job(claimed["id"], leads_found=4)
    out.append(("detail", _norm_job(db.get_job_detail(claimed["id"]))))
    db.fail_job(b, "boom 1")                                     # attempts 0 < 3: re-queued
    out.append(("after_fail1", db.get_job_detail(b)["status"], db.get_job_detail(b)["error"]))
    for _ in range(3):
        row = db.claim_job()
        if row:
            db.fail_job(row["id"], "boom again")
    out.append(("statuses", sorted((j["id"] == a, j["status"], j["attempts"]) for j in db.get_jobs())))
    db.cancel_job(c)
    out.append(("cancelled", db.get_job_detail(c)["status"], db.get_job_detail(c)["error"]))
    db.retry_job(c)
    out.append(("retried", db.get_job_detail(c)["status"], db.get_job_detail(c)["attempts"]))
    out.append(("filter", [j["id"] == c for j in db.get_jobs(status="pending")]))
    db.conn.execute("UPDATE jobs SET status = 'failed', error = ? WHERE id = ?", ("raw", a))
    db.conn.commit()
    out.append(("raw", db.conn.execute("SELECT status, error FROM jobs WHERE id = ?", (a,)).fetchone()[0:2]))
    out.append(("stages", [(s["stage"], s["status"], s["input_count"]) for s in db.get_job_stages(claimed["id"])]))
    db.delete_job(claimed["id"], keep_leads=True)
    out.append(("deleted", db.get_job_detail(claimed["id"]), db.get_job_stages(claimed["id"])))
    out.append(("missing", db.get_job_detail("nope")))
    return out


@needs_pg
def test_postgres_ledger_matches_the_sqlite_ledger_operation_for_operation(pg, tmp_path):
    from apps.api.services.leadgen.db import LeadDB
    ids = ("a" * 32, "b" * 32, "c" * 32)
    sqlite_db = LeadDB(str(tmp_path / "leads.db"))
    # created_at sort order must be deterministic for both: space the inserts
    expected = _script_slow(sqlite_db, ids)
    sqlite_db.close()
    ledger = ledger_mod.PgCollectionLedger("ws-parity")
    try:
        actual = _script_slow(ledger, ids)
    finally:
        ledger.close()
    assert actual == expected


def _script_slow(db, ids):
    # claim_job orders by created_at: make the three jobs' timestamps distinct.
    real_create = db.create_job

    def slow_create(*a, **k):
        r = real_create(*a, **k)
        time.sleep(0.01)
        return r

    db.create_job = slow_create
    return _script(db, ids)


@needs_pg
def test_racing_hosts_claim_each_job_exactly_once(pg, pgdb):
    ws = "ws-race"
    seed = ledger_mod.PgCollectionLedger(ws)
    ids = [f"job{i:03d}" for i in range(24)]
    for jid in ids:
        seed.create_job(jid, f"q {jid}")
    seed.close()
    claims, errors = [], []
    barrier = threading.Barrier(6)

    def host(n):
        engine = create_engine(pgdb.app_url, pool_size=2, max_overflow=0)   # its own pool: "another host"
        try:
            led = ledger_mod.PgCollectionLedger(ws, engine=engine)
            barrier.wait(timeout=20)
            while True:
                job = led.claim_job()
                if job is None:
                    break
                claims.append(job["id"])
            led.close()
        except Exception as exc:  # noqa: BLE001
            errors.append(repr(exc))
        finally:
            engine.dispose()

    threads = [threading.Thread(target=host, args=(i,)) for i in range(6)]
    [t.start() for t in threads]
    [t.join(60) for t in threads]
    assert not errors, errors
    assert sorted(claims) == sorted(ids), "a job was claimed twice or not at all"
    with pg[0].begin() as c:
        assert c.execute(text("SELECT count(*) FROM collection_jobs WHERE status = 'running' AND attempts = 1")).scalar() == 24


@needs_pg
def test_two_hosts_see_one_ledger(pg, pgdb):
    host_a = ledger_mod.PgCollectionLedger("ws-shared")
    host_a.create_job("shared-1", "from host a")
    host_b_engine = create_engine(pgdb.app_url, pool_size=2, max_overflow=0)
    try:
        host_b = ledger_mod.PgCollectionLedger("ws-shared", engine=host_b_engine)
        assert host_b.get_job_detail("shared-1")["query"] == "from host a"
        host_b.claim_job()
        host_b.complete_job("shared-1", leads_found=7)
        host_b.close()
    finally:
        host_b_engine.dispose()
    assert host_a.get_job_detail("shared-1")["status"] == "done"
    assert host_a.get_job_detail("shared-1")["leads_found"] == 7
    host_a.close()


@needs_pg
def test_rls_confines_a_connection_to_its_workspace(pg):
    owner, engine = pg
    a, b = ledger_mod.PgCollectionLedger("ws-a"), ledger_mod.PgCollectionLedger("ws-b")
    a.create_job("job-a", "a's query")
    b.create_job("job-b", "b's query")
    a.create_stage("job-a", "discover")
    assert [j["id"] for j in a.get_jobs()] == ["job-a"]
    assert [j["id"] for j in b.get_jobs()] == ["job-b"]
    assert b.get_job_detail("job-a") is None
    b.cancel_job("job-a")                                    # cannot touch another tenant's job
    assert a.get_job_detail("job-a")["status"] == "pending"
    b.conn.execute("DELETE FROM jobs WHERE id = ?", ("job-a",))
    b.conn.commit()
    assert a.get_job_detail("job-a") is not None
    assert b.get_job_stages("job-a") == []
    with pytest.raises(Exception, match="row-level security"):
        b.conn.execute("INSERT INTO collection_jobs (id, workspace_id, query) VALUES ('x', 'ws-a', 'forged')")
    b.conn.rollback()
    with engine.begin() as c:                                # unbound session: fail closed
        assert c.execute(text("SELECT count(*) FROM collection_jobs")).scalar() == 0
        assert c.execute(text("SELECT count(*) FROM collection_job_stages")).scalar() == 0
    with owner.begin() as c:
        flags = c.execute(text(
            "SELECT relname, relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = ANY(:t)"),
            {"t": ["collection_jobs", "collection_job_stages"]}).all()
    assert len(flags) == 2 and all(f[1] and f[2] for f in flags)
    a.close(); b.close()


@needs_pg
def test_ledger_connection_does_not_sit_idle_in_transaction(pg, pgdb):
    led = ledger_mod.PgCollectionLedger("ws-idle")
    led.create_job("idle-1", "q")
    led.conn.execute("SELECT status FROM jobs WHERE id = ?", ("idle-1",)).fetchone()   # a read, like _is_cancelled
    with pg[0].begin() as c:
        states = c.execute(text(
            "SELECT state FROM pg_stat_activity WHERE datname = :d AND usename = 'app_rls_test'"),
            {"d": pgdb.name}).scalars().all()
    assert "idle in transaction" not in states, states
    led.close()


@needs_pg
def test_job_routes_and_failure_reconciler_use_the_postgres_ledger(pg, monkeypatch):
    from apps.api.core.config import settings
    from apps.api.core.tenancy import WorkspaceCtx
    from apps.api.routers import leads as leads_router
    from apps.api.services.leadgen import job_runner, store
    monkeypatch.setattr(settings, "COLLECTION_LEDGER_STORE", "postgres")
    monkeypatch.setattr(store, "use_pg_store", lambda: True)

    class U:  # noqa: D401 - minimal user
        id = 1

    ctx = WorkspaceCtx(user=U(), workspace_id="ws-route", slug="route")
    seed = ledger_mod.open_job_ledger("ws-route", "route")
    seed.create_job("rt-1", "route query")
    seed.create_stage("rt-1", "discover")
    seed.close()

    detail = leads_router.get_job_detail("rt-1", ctx)
    assert detail["query"] == "route query" and detail["stages"][0]["stage"] == "discover"
    assert [j["id"] for j in leads_router.list_jobs(None, ctx)] == ["rt-1"]

    job_runner.reconcile_collect_job_failure(
        1, {"job_id": "rt-1", "slug": "route", "workspace_id": "ws-route"}, "worker died", will_retry=True)
    assert leads_router.get_job_detail("rt-1", ctx)["status"] == "pending"
    assert "Queue retry scheduled: worker died" in leads_router.get_job_detail("rt-1", ctx)["error"]
    job_runner.reconcile_collect_job_failure(
        1, {"job_id": "rt-1", "slug": "route", "workspace_id": "ws-route"}, "gave up", will_retry=False)
    assert leads_router.get_job_detail("rt-1", ctx)["status"] == "failed"
    leads_router.cancel_job("rt-1", ctx)                      # already failed: cancel is a no-op
    leads_router.delete_job("rt-1", keep_leads=True, ctx=ctx)
    with pytest.raises(Exception) as err:
        leads_router.get_job_detail("rt-1", ctx)
    assert "404" in str(err.value) or "not found" in str(err.value).lower()


# ── backfill ────────────────────────────────────────────────────────────────

@pytest.fixture
def ledger_source(tmp_path, monkeypatch):
    """Real single-host ledger files: data/leads.db (main) and per-workspace files."""
    from apps.api.core.config import settings
    from apps.api.services.leadgen.db import LeadDB
    from apps.api.services.workspace import manager
    monkeypatch.setattr(settings, "WORKSPACE_META_STORE", "sqlite")
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    main = manager.get_workspace_by_slug("main")
    a = manager.create_workspace("Alpha", owner_id=1)
    b = manager.create_workspace("Bravo", owner_id=1)
    data = Path(tmp_path) / "data"

    def fill(path, jobs):
        db = LeadDB(str(path))
        for jid, q, ws, stages in jobs:
            db.create_job(jid, q)
            if ws:
                db.conn.execute("UPDATE jobs SET workspace_id = ? WHERE id = ?", (ws, jid))
            db.conn.commit()
            for stage in stages:
                sid = db.create_stage(jid, stage)
                db.complete_stage(sid, input_count=3, output_count=2, details='{"x": 1}')
        db.conn.execute("PRAGMA foreign_keys=OFF")        # legacy files can hold stages of deleted jobs
        db.conn.execute("INSERT INTO job_stages (job_id, stage) VALUES ('orphan-job', 'ghost')")
        db.conn.commit()
        db.close()

    fill(data / "leads.db", [
        ("m-1", "main legacy blank stamp", "", ["discover"]),
        ("m-2", "main stamped main", main.id, ["discover", "enrich"]),
        ("m-3", "main stamped for alpha", a.id, ["discover"]),
        ("m-4", "main stamped unknown", "no-such-workspace", []),
    ])
    fill(data / "workspaces" / a.slug / "leads.db", [("a-1", "alpha job", a.id, ["discover"]),
                                                      ("a-2", "alpha stamped other", b.id, [])])
    fill(data / "workspaces" / b.slug / "leads.db", [("b-1", "bravo job", "", ["enrich", "discover"])])
    return data, {"main": main, "a": a, "b": b}


@needs_pg
def test_ledger_backfill_attributes_files_to_their_tenants(ledger_source, pg):
    data, ws = ledger_source
    owner, _ = pg
    res = ledger_backfill.backfill_ledger(str(data))
    assert res["collection_jobs"].inserted == 7 and res["collection_job_stages"].inserted == 7
    assert res["_attribution"].updated == 1                # m-3 followed its stamp to Alpha
    assert res["_attribution"].skipped_existing == 3       # one orphan stage per file
    with owner.begin() as c:
        owner_of = dict(c.execute(text("SELECT id, workspace_id FROM collection_jobs")).all())
    assert owner_of == {
        "m-1": ws["main"].id, "m-2": ws["main"].id, "m-3": ws["a"].id,
        "m-4": ws["main"].id,                              # unknown stamp falls back to the file's tenant
        "a-1": ws["a"].id, "a-2": ws["a"].id,              # a per-workspace file is its own tenant boundary
        "b-1": ws["b"].id}
    checks = ledger_backfill.verify_ledger(str(data))
    assert all(c.ok for c in checks), [(c.table, c.missing_keys, c.extra_keys, c.changed_keys) for c in checks]
    # stage order within a job survives (ORDER BY id)
    led = ledger_mod.PgCollectionLedger(ws["b"].id)
    assert [s["stage"] for s in led.get_job_stages("b-1")] == ["enrich", "discover"]
    led.close()


@needs_pg
def test_ledger_backfill_is_idempotent_and_never_clobbers(ledger_source, pg):
    data, ws = ledger_source
    first = ledger_backfill.backfill_ledger(str(data))
    again = ledger_backfill.backfill_ledger(str(data))
    for name in ("collection_jobs", "collection_job_stages"):
        assert again[name].inserted == 0 and again[name].skipped_existing == first[name].source_rows
    led = ledger_mod.PgCollectionLedger(ws["main"].id)
    led.cancel_job("m-1")                                  # a PostgreSQL-side change after the first run
    led.close()
    ledger_backfill.backfill_ledger(str(data))
    led = ledger_mod.PgCollectionLedger(ws["main"].id)
    assert led.get_job_detail("m-1")["status"] == "cancelled"
    led.close()
    bad = {c.table: c for c in ledger_backfill.verify_ledger(str(data))}
    assert not bad["collection_jobs"].ok and bad["collection_jobs"].changed_keys == ["m-1"]
    ledger_backfill.backfill_ledger(str(data), overwrite=True)
    assert all(c.ok for c in ledger_backfill.verify_ledger(str(data)))


@needs_pg
def test_ledger_backfill_resumes_after_a_crash_and_does_not_touch_sources(ledger_source, pg, monkeypatch):
    import hashlib
    data, _ = ledger_source
    files = sorted(data.rglob("leads.db"))
    before = [hashlib.sha256(f.read_bytes()).hexdigest() for f in files]
    real = pg_meta.PgMetaConnection.commit
    calls = {"n": 0}

    def flaky(self):
        calls["n"] += 1
        if calls["n"] == 3:
            raise RuntimeError("simulated crash")
        return real(self)

    monkeypatch.setattr(pg_meta.PgMetaConnection, "commit", flaky)
    with pytest.raises(RuntimeError, match="simulated crash"):
        ledger_backfill.backfill_ledger(str(data), batch=2)
    monkeypatch.setattr(pg_meta.PgMetaConnection, "commit", real)
    assert not all(c.ok for c in ledger_backfill.verify_ledger(str(data)))
    ledger_backfill.backfill_ledger(str(data), batch=2)
    assert all(c.ok for c in ledger_backfill.verify_ledger(str(data)))
    assert before == [hashlib.sha256(f.read_bytes()).hexdigest() for f in files]


@needs_pg
def test_cli_backfills_the_ledger_with_the_other_stores(ledger_source, pg, capsys):
    from apps.api.scripts import multihost_backfill as cli
    data, ws = ledger_source
    assert cli.main(["run", "--data-dir", str(data), "--stores", "meta,ledger"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["ok"] and {c["table"] for c in out["verify"]["tables"]} >= {
        "workspaces", "collection_jobs", "collection_job_stages"}
