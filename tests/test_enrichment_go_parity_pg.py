"""Python/Go parity for the connector enrichment job (PG-gated, one command).

    TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
        uv run pytest tests/test_enrichment_go_parity_pg.py

TEST_DATABASE_URL only has to name a server on which the role may create
databases and roles; the harness never touches the database it names. It needs
``go`` on PATH.

The harness

1. migrates a template database with Alembic once and clones it into two fresh
   databases (one for Python, one for Go);
2. seeds both with the identical dataset of tests/enrichment_parity/fixtures.py
   (manifest v1 connectors, workbooks in two workspaces, a benched provider, a
   capped budget, rows that make a provider fail, time out, rate limit, return
   no data, return a JSON blob, or break on a non-string value);
3. starts one local HTTPS provider simulator and runs the same job rows, as
   NOSUPERUSER NOBYPASSRLS roles under forced RLS: the unchanged Python handler
   (``handle_run_workbook``, via tests/enrichment_parity/python_runner.py) on
   one database, the Go handler (internal/jobs/enrich, via ``go test``) on the
   other, resetting the simulator in between;
4. requires identical workbooks, rows (data and the enrichment overlay),
   ``workbook_enrichments``, ``workbook_spend_attempts``, ``provider_stats`` and
   job receipts, an identical multiset of requests at the simulator, and the
   same outcome from every step.

Set KEEP_PARITY_DBS=1 to keep the databases of a failing run for inspection.
"""
from __future__ import annotations

import difflib
import json
import os
import shutil
import subprocess
import sys
import uuid
from pathlib import Path

import pytest

from tests.enrichment_parity import fixtures
from tests.enrichment_parity.simulator import Simulator

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")
REPO = Path(__file__).resolve().parents[1]
SERVER = REPO / "apps" / "server"
APP_ROLE, APP_PASSWORD = "opengtm_core_app_test", "core_app_test_only"
SETUP_LOCK = 0x6F70656E67746D  # shared with the Go dbtest helpers

needs_go = pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not on PATH")
needs_pg = pytest.mark.skipif(not TEST_DATABASE_URL, reason="TEST_DATABASE_URL not set (enrichment parity tests skipped)")


def _urls(database: str, user: str | None = None, password: str | None = None) -> tuple[str, str]:
    from sqlalchemy.engine import make_url

    url = make_url(TEST_DATABASE_URL).set(database=database, drivername="postgresql+psycopg")
    if user:
        url = url.set(username=user, password=password)
    sa = url.render_as_string(hide_password=False)
    return sa, sa.replace("postgresql+psycopg://", "postgresql://", 1)


def _admin():
    import psycopg

    return psycopg.connect(_urls("postgres")[1], autocommit=True)


def _drop(name: str) -> None:
    if os.getenv("KEEP_PARITY_DBS"):
        return
    with _admin() as admin:
        admin.execute(f'DROP DATABASE IF EXISTS "{name}" WITH (FORCE)')


def ensure_app_role() -> None:
    """The NOSUPERUSER NOBYPASSRLS runtime login both executors connect as."""
    with _admin() as admin:
        admin.execute("SELECT pg_advisory_lock(%s)", (SETUP_LOCK,))
        try:
            exists = admin.execute("SELECT 1 FROM pg_roles WHERE rolname = %s", (APP_ROLE,)).fetchone()
            if not exists:
                admin.execute(f"CREATE ROLE {APP_ROLE} LOGIN NOSUPERUSER NOBYPASSRLS")
            admin.execute(f"ALTER ROLE {APP_ROLE} LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '{APP_PASSWORD}'")
            admin.execute(f"GRANT USAGE ON SCHEMA public TO {APP_ROLE}")
            admin.execute(f"GRANT yupcha_app TO {APP_ROLE}")
        finally:
            admin.execute("SELECT pg_advisory_unlock(%s)", (SETUP_LOCK,))


@pytest.fixture(scope="module")
def template_db():
    name = f"opengtm_enrpar_tpl_{uuid.uuid4().hex[:8]}"
    with _admin() as admin:
        admin.execute(f'CREATE DATABASE "{name}"')
    try:
        res = subprocess.run(
            ["uv", "run", "--frozen", "alembic", "upgrade", "head"],
            cwd=REPO, env=dict(os.environ, DATABASE_URL=_urls(name)[0]), capture_output=True, text=True)
        assert res.returncode == 0, f"alembic upgrade failed:\n{res.stdout}\n{res.stderr}"
        ensure_app_role()
        yield name
    finally:
        _drop(name)


@pytest.fixture(scope="module")
def simulator(tmp_path_factory):
    sim = Simulator(tmp_path_factory.mktemp("sim"), expected_key=fixtures.SECRET)
    yield sim
    sim.close()


def clone(template: str) -> str:
    name = f"opengtm_enrpar_{uuid.uuid4().hex[:8]}"
    with _admin() as admin:
        admin.execute(f'CREATE DATABASE "{name}" TEMPLATE "{template}"')
    return name


def seed(db: str) -> dict:
    """Seed the shared dataset; returns {workbook id: [row ids]} and the job specs."""
    import psycopg

    row_ids: dict[str, list[int]] = {}
    with psycopg.connect(_urls(db)[1]) as conn:
        for wb in fixtures.workbooks():
            conn.execute(
                """INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config,
                       filter_criteria, columns_config, total_rows, completed_rows, sync_to_leads, budget_max_usd,
                       budget_spent_usd, refresh_policy)
                   VALUES (%s, %s, '', %s, %s, 'csv', '{}', '{}', %s, 0, 0, true, %s, %s, '{}')""",
                (wb["id"], wb["id"], wb.get("status", "draft"), wb["ws"], json.dumps(wb["columns"]), wb["budget"],
                 wb.get("spent", 0.0)))
            ids = []
            for pos, row in enumerate(wb["rows"]):
                row = dict(row)
                enrichments = row.pop("enrichments", {})
                (rid,) = conn.execute(
                    """INSERT INTO workbook_rows (workbook_id, workspace_id, position, data, enrichments,
                           corroboration_count) VALUES (%s, %s, %s, %s, %s, 1) RETURNING id""",
                    (wb["id"], wb["ws"], pos, json.dumps(row), json.dumps(enrichments))).fetchone()
                ids.append(rid)
            row_ids[wb["id"]] = ids
        for st in fixtures.provider_stats():
            conn.execute(
                """INSERT INTO provider_stats (provider, field, attempts, hits, total_confidence, total_latency_ms,
                       total_cost_usd, cooldown_until, accuracy_samples)
                   VALUES (%s, %s, %s, %s, 0, 0, 0,
                           CASE WHEN %s::float8 IS NULL THEN NULL
                                ELSE (now() AT TIME ZONE 'UTC') + make_interval(hours => %s::int) END, 0)""",
                (st["provider"], st["field"], st["attempts"], st["hits"], st["cooldown_hours"], st["cooldown_hours"]))
        workbooks = {w["id"]: w for w in fixtures.workbooks()}
        jobs = []
        for step in fixtures.steps():
            wb = workbooks.get(step["workbook"])
            ids = row_ids.get(step["workbook"], [])
            payload = {
                "workbook_id": step["workbook"],
                "workspace_id": wb["ws"] if wb else fixtures.WS1,
                "run_id": f"parity-run-{len(jobs) + 1}",
                "column_ids": [c["id"] for c in wb["columns"] if c["type"] in ("enrichment", "waterfall")] if wb else [],
                "row_columns": None,
                "row_ids": step.get("row_ids", ids),
                "lead_ids": None,
                "concurrency": step["concurrency"],
                "max_providers": step.get("max_providers", 0),
                "retry_passes": step["retry_passes"],
                "provider_workers": step["provider_workers"],
                "provider_timeout": step["provider_timeout"],
                "fill_missing": bool(step.get("fill_missing", False)),
                "force": False,
            }
            if step.get("scope"):
                payload["column_ids"] = ["email", "phone"]
                payload["row_columns"] = {str(ids[0]): ["email"], str(ids[1]): ["phone"], str(ids[2]): ["email", "phone"]}
                payload["row_ids"] = ids[:3]
            (job_id,) = conn.execute(
                """INSERT INTO jobs (type, payload, workspace_id, status, priority, created_at, started_at,
                       last_heartbeat, retry_count, max_retries, worker_id, locked_at, next_run_at)
                   VALUES ('run_workbook_connector', %s, %s, 'processing', 1, LOCALTIMESTAMP, LOCALTIMESTAMP,
                           LOCALTIMESTAMP, 0, 3, 'parity-worker:1:00000000', LOCALTIMESTAMP, LOCALTIMESTAMP)
                   RETURNING id""", (json.dumps(payload), payload["workspace_id"])).fetchone()
            jobs.append({"job": job_id, "name": step["name"]})
        conn.commit()
    return {"rows": row_ids, "jobs": jobs}


def _env(sim: Simulator) -> dict:
    return {**os.environ, "SSL_CERT_FILE": str(sim.cert), "PAR_KEY": fixtures.SECRET}


def run_python(db: str, sim: Simulator, connectors: Path, steps: Path, out: Path) -> None:
    res = subprocess.run(
        [sys.executable, "-m", "tests.enrichment_parity.python_runner",
         "--app-url", _urls(db, APP_ROLE, APP_PASSWORD)[0], "--owner-url", _urls(db)[1],
         "--connectors", str(connectors), "--sim-base", sim.base, "--steps", str(steps), "--out", str(out)],
        cwd=REPO, env=_env(sim), capture_output=True, text=True)
    assert res.returncode == 0, f"python runner failed:\n{res.stdout[-3000:]}\n{res.stderr[-6000:]}"


def run_go(db: str, sim: Simulator, connectors: Path, steps: Path, out: Path) -> None:
    res = subprocess.run(
        ["go", "test", "-count=1", "-run", "^TestParityDriver$", "-v", "./internal/jobs/enrich/"],
        cwd=SERVER,
        env={**_env(sim), "OPENGTM_PARITY_DATABASE_URL": _urls(db)[1], "OPENGTM_PARITY_STEPS": str(steps),
             "OPENGTM_PARITY_CONNECTORS": str(connectors), "OPENGTM_PARITY_OUT": str(out)},
        capture_output=True, text=True)
    assert res.returncode == 0, f"go test failed:\n{res.stdout[-6000:]}\n{res.stderr[-3000:]}"
    assert "SKIP" not in res.stdout and "no tests to run" not in res.stdout, res.stdout


def dump(db: str) -> dict:
    import psycopg

    d: dict = {}
    with psycopg.connect(_urls(db)[1], autocommit=True) as conn:
        def rows(sql):
            return [list(r) for r in conn.execute(sql).fetchall()]

        d["workbooks"] = rows("""SELECT id, status, total_rows, completed_rows, budget_spent_usd, budget_max_usd
                                 FROM workbooks ORDER BY id""")
        d["rows"] = rows("""SELECT workbook_id, id, position, lead_id, data::jsonb, enrichments::jsonb
                            FROM workbook_rows ORDER BY 1, 2""")
        d["enrichments"] = rows("""SELECT workbook_id, workspace_id, lead_id, column_id, value, status, provider, error,
                                          cell_metadata IS NULL, cell_metadata::jsonb
                                   FROM workbook_enrichments ORDER BY 1, 3, 4""")
        d["spend"] = rows("""SELECT workspace_id, workbook_id, run_id, row_identity, column_id, provider, attempt_key,
                                    contract_hash, status, reserved_microusd, settled_microusd, cost_basis::jsonb,
                                    result::jsonb
                             FROM workbook_spend_attempts ORDER BY 1, 2, 4, 5, 6""")
        d["stats"] = rows("""SELECT provider, field, attempts, hits, round(total_confidence::numeric, 6),
                                    round(total_cost_usd::numeric, 6), cooldown_until IS NOT NULL
                             FROM provider_stats ORDER BY 1, 2""")
        d["jobs"] = rows("""SELECT id, type, status, workspace_id, payload::jsonb FROM jobs ORDER BY id""")
    for job in d["jobs"]:
        receipt = (job[4] or {}).get("execution_result")
        if isinstance(receipt, dict):
            assert isinstance(receipt.pop("recorded_at", None), str)
    # The Go executor adds cell_metadata.evidence (additive); the rest must match.
    evidence = {}
    for row in d["enrichments"]:
        meta = row[9]
        if isinstance(meta, dict) and "evidence" in meta:
            evidence[(row[0], row[2], row[3])] = meta.pop("evidence")
            if not meta:
                row[9] = None
    d["_evidence"] = {f"{k[0]}/{k[1]}/{k[2]}": v for k, v in evidence.items()}
    return d


def normalized_requests(sim: Simulator) -> list:
    out = []
    for r in sim.requests():
        body = r["body"]
        try:
            body = json.dumps(json.loads(body), sort_keys=True) if body else ""
        except ValueError:
            pass
        out.append((r["method"], r["path"], tuple(sorted(r["query"].items())), body,
                    tuple(sorted(r["headers"].items()))))
    return sorted(out)


def _diff(name: str, py, go) -> list[str]:
    if py == go:
        return []
    if isinstance(py, dict) and isinstance(go, dict):
        out = []
        for key in sorted(set(py) | set(go)):
            out += _diff(f"{name}.{key}", py.get(key), go.get(key))
        return out
    a = json.dumps(py, indent=1, default=str, sort_keys=True).splitlines()
    b = json.dumps(go, indent=1, default=str, sort_keys=True).splitlines()
    delta = list(difflib.unified_diff(a, b, "python", "go", lineterm="", n=2))
    return [f"{name} differs:\n" + "\n".join(delta[:120])]


def execute_both(template_db, simulator, tmp_path):
    connectors = tmp_path / "connectors"
    connectors.mkdir()
    for name, text in fixtures.connectors(simulator.base).items():
        (connectors / f"{name}.yaml").write_text(text)
    py_db, go_db = clone(template_db), clone(template_db)
    try:
        seeded_py, seeded_go = seed(py_db), seed(go_db)
        assert seeded_py == seeded_go, "the two databases were not seeded identically"
        steps = tmp_path / "steps.json"
        steps.write_text(json.dumps(seeded_py["jobs"]))
        py_out, go_out = tmp_path / "py.json", tmp_path / "go.json"

        simulator.reset()
        run_python(py_db, simulator, connectors, steps, py_out)
        py_requests = normalized_requests(simulator)
        simulator.reset()
        run_go(go_db, simulator, connectors, steps, go_out)
        go_requests = normalized_requests(simulator)
        return (seeded_py, py_db, go_db, json.loads(py_out.read_text()), json.loads(go_out.read_text()),
                py_requests, go_requests)
    except BaseException:
        if os.getenv("KEEP_PARITY_DBS"):
            print(f"kept databases {py_db} (python) and {go_db} (go)")
        raise


@needs_pg
@needs_go
@pytest.mark.postgres
def test_python_and_go_leave_identical_state(template_db, simulator, tmp_path):
    seeded, py_db, go_db, py_steps, go_steps, py_requests, go_requests = execute_both(template_db, simulator, tmp_path)
    try:
        assert len(py_steps) == len(go_steps) == len(seeded["jobs"])
        problems = [f"{j['name']}: python={p['error']!r} go={g['error']!r}"
                    for j, p, g in zip(seeded["jobs"], py_steps, go_steps) if p["error"] != g["error"]]
        assert not problems, "step outcomes differ:\n" + "\n".join(problems)

        py_dump, go_dump = dump(py_db), dump(go_db)
        go_evidence = go_dump.pop("_evidence")
        assert not py_dump.pop("_evidence"), "the Python path writes no evidence key"
        diffs = _diff("state", py_dump, go_dump)
        assert not diffs, "\n\n".join(diffs)
        assert py_requests == go_requests, "\n".join(_diff("requests", py_requests, go_requests))

        _assert_scenarios_were_exercised(py_dump, py_requests)
        _assert_evidence(go_evidence, go_dump)
    finally:
        _drop(py_db)
        _drop(go_db)


@needs_pg
@needs_go
@pytest.mark.postgres
@pytest.mark.parametrize("first, second", [("python", "go"), ("go", "python")])
def test_a_job_started_by_one_executor_is_finished_by_the_other(template_db, simulator, tmp_path, first, second):
    """Rollback and cutover mid-job: the spend ledger is shared state.

    Executor A runs the paid workbooks (settled, uncertain and over-budget attempts). A worker
    then 'dies' between settling and committing its cells (the cells are wiped, the attempts are
    kept) and the queue retries the same job on executor B. B must reuse A's settled receipts
    without calling the vendor, refuse A's uncertain attempts, never charge twice, and end with
    exactly the cells A produced. That only holds if both compute the same attempt key and
    contract digest, so it is the cross-language proof of the ledger protocol.
    """
    import psycopg

    connectors = tmp_path / "connectors"
    connectors.mkdir()
    for name, text in fixtures.connectors(simulator.base).items():
        (connectors / f"{name}.yaml").write_text(text)
    db = clone(template_db)
    runners = {"python": run_python, "go": run_go}
    try:
        seeded = seed(db)
        wanted = {"paid, capped budget", "paid, unlimited budget"}
        steps = tmp_path / "steps.json"
        steps.write_text(json.dumps([j for j in seeded["jobs"] if j["name"] in wanted]))

        simulator.reset()
        runners[first](db, simulator, connectors, steps, tmp_path / "a.json")
        paid_requests_a = [r for r in normalized_requests(simulator) if r[1] == "/paid/lookup"]
        after_a = dump(db)
        assert paid_requests_a, "executor A never called the paid provider"
        statuses = {s[8] for s in after_a["spend"]}
        assert {"settled", "uncertain"} <= statuses, statuses
        spent_a = {w[0]: w[4] for w in after_a["workbooks"]}

        with psycopg.connect(_urls(db)[1], autocommit=True) as conn:
            conn.execute("DELETE FROM workbook_enrichments WHERE workbook_id IN ('wb-paid', 'wb-paid-capped')")
            conn.execute("""UPDATE workbook_rows SET enrichments = '{}'::json
                            WHERE workbook_id IN ('wb-paid', 'wb-paid-capped')""")
            conn.execute("UPDATE workbooks SET status = 'running' WHERE id IN ('wb-paid', 'wb-paid-capped')")
            conn.execute("UPDATE provider_stats SET cooldown_until = NULL WHERE provider = 'par_paid'")

        simulator.reset()
        runners[second](db, simulator, connectors, steps, tmp_path / "b.json")
        assert not simulator.requests(), "the vendor was called again after a worker died mid-job"
        after_b = dump(db)
        after_a.pop("_evidence"), after_b.pop("_evidence")

        # The ledger is untouched (nothing charged twice) and the cells are exactly A's.
        assert after_b["spend"] == after_a["spend"]
        assert {w[0]: w[4] for w in after_b["workbooks"]} == spent_a
        # (cell_metadata and the row mirror's skipped_providers record which providers the planner
        # benched at the time; the cooldown was cleared between the runs, so they are not compared.)
        def cells(d):
            return [r[:8] for r in d["enrichments"] if r[0] in ("wb-paid", "wb-paid-capped")]

        def mirror(d):
            out = []
            for r in d["rows"]:
                if r[0] in ("wb-paid", "wb-paid-capped"):
                    cell = {k: {f: v for f, v in c.items() if f != "skipped_providers"} for k, c in r[5].items()}
                    out.append(r[:5] + [cell])
            return out

        for key, view in (("enrichments", cells), ("rows", mirror)):
            diffs = _diff(key, view(after_a), view(after_b))
            assert not diffs, "\n".join(diffs)
        assert {r[5] for r in after_b["enrichments"] if r[0] == "wb-paid"} >= {"complete", "error"}
    finally:
        _drop(db)


def _assert_scenarios_were_exercised(dump: dict, requests: list) -> None:
    """Guard against vacuous parity: the dataset must really hit each behaviour."""
    errors = {row[7] for row in dump["enrichments"] if row[5] == "error"}
    complete = [row for row in dump["enrichments"] if row[5] == "complete"]
    assert len(complete) >= 15, len(complete)
    assert "no_data" in errors
    # a real timeout benched the provider (the circuit breaker), and the retry pass then skipped it
    assert any(r[0] == "par_timeout" and r[6] for r in dump["stats"]), dump["stats"]
    assert "providers_unavailable: par_timeout (cooldown)" in errors, errors
    assert any(e and e.startswith("providers_unavailable: ") and "cooldown" in e for e in errors), errors
    assert any(e and "object has no attribute 'strip'" in e for e in errors), errors
    spend = dump["spend"]
    statuses = {s[8] for s in spend}
    assert {"settled", "uncertain", "dispatched"} & statuses and "settled" in statuses and "uncertain" in statuses, statuses
    assert any(wb[0] == "wb-paid-capped" and wb[4] > 0 for wb in dump["workbooks"]), dump["workbooks"]
    assert all(s[9] == 50000 for s in spend), "every reservation is the connector's catalog price in micro-USD"
    # non-ASCII identities (workspace row identity, column id and name, company) hash identically
    assert any(s[1] == "wb-unicode" and s[8] == "settled" and "ä" in s[4] for s in spend), spend
    # float budgets: 0.15 spent of 0.2 admits exactly one more 0.05 lookup
    edge = [s for s in spend if s[1] == "wb-budget-edge" and s[8] == "settled"]
    assert len(edge) == 1, edge
    assert {e for e in errors} & {"accounting_uncertain", "workbook_budget", "attempt_not_dispatchable"}, errors
    # tenant isolation: each workspace's cells carry only its own workspace id
    assert {r[1] for r in dump["enrichments"] if r[0].startswith("wb-iso")} == {"ws-par-2"}
    assert {r[1] for r in dump["enrichments"] if r[0] == "wb-basic"} == {"ws-par-1"}
    assert any(r[0] == "wb-iso" and r[5] == "complete" for r in dump["enrichments"])
    assert not [r for r in dump["enrichments"] if r[0] == "wb-paused"]
    # a retry pass really happened: the flaky domain was requested twice per path
    flaky = [r for r in requests if ("domain", "flaky.example") in r[2]]
    assert len(flaky) >= 2
    assert any(r[0] == "POST" for r in requests)
    assert any(dict(r[4]).get("x-api-key") for r in requests)
    assert any(dict(r[2]).get("key") for r in requests)
    receipts = [j[4].get("execution_result") for j in dump["jobs"] if j[4].get("execution_result")]
    assert len(receipts) >= 8, receipts


def _assert_evidence(evidence: dict, dump: dict) -> None:
    """Go's additive evidence: where each winning value came from, without secrets."""
    assert evidence, "the Go path wrote no evidence"
    for key, ev in evidence.items():
        assert ev["sha256"] and ev["bytes"] > 0 and ev["status"] == 200 and ev["method"] in ("GET", "POST"), (key, ev)
        assert fixtures.SECRET not in json.dumps(ev), (key, ev)
        assert ev["source_url"].startswith("https://127.0.0.1:"), ev
    assert any("key=REDACTED" in ev["source_url"] for ev in evidence.values()), "query-auth key was not redacted"
