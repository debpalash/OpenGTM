"""Python/Go parity for the retention_enforce job (PG-gated, one command).

    TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
        uv run pytest tests/test_retention_go_parity_pg.py

TEST_DATABASE_URL only has to name a server on which the role may create
databases and roles (a superuser, like tests/test_queue_claim_pg.py needs); the
harness never touches the database it names. It needs ``go`` on PATH.

For each time-zone combination the harness

1. migrates a template database with Alembic once, then clones it into two
   fresh databases (one for Python, one for Go);
2. seeds both with the identical dataset in tests/retention_parity (eight
   target tables, rows 1 second either side of every cutoff, several
   workspaces, legal hold, disabled and custom policies, an unsupported
   category, mid-run failures, reconciliation cases);
3. replays tests/retention_parity/scenarios.json: the Python handler and
   reconciler (unchanged, ``datetime.now`` frozen) on one database, the Go
   handler and reconciler (internal/jobs/retention, via ``go test``) on the
   other, both connected as NOSUPERUSER NOBYPASSRLS roles under forced RLS;
4. dumps retention_runs (status, deleted_counts, error, policy_snapshot, ...),
   policies, the schedule mirror, the retention_enforce jobs (fire_key, next
   run, payload) and every remaining row of the eight target tables, and
   requires the two dumps, and the error each step raised, to be identical.

A separate test compares the pure helpers (normalized_days and the cutoff
arithmetic) over a corpus of valid and malformed inputs.

Set KEEP_PARITY_DBS=1 to keep the databases of a failing run for inspection.
"""
from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from tests.parity_harness import pg
from tests.parity_harness.pg import ZONES, needs_go, needs_pg

FIXTURES = pg.REPO / "tests" / "retention_parity"
GO_PACKAGE = "internal/jobs/retention"

TARGET_KEYS = {
    "governance_audit_events": "id",
    "llm_usage_daily": "provider",
    "signals": "id",
    "destination_deliveries": "idempotency_key",
    "destination_inbound_receipts": "id",
    "audience_membership_events": "lead_id::text",
    "playbook_results": "lead_id::text",
    "outreach_sends": "idempotency_key",
}


@pytest.fixture(scope="module")
def template_db():
    return pg.shared_template()


def _seed_files() -> list[Path]:
    return [FIXTURES / "dataset_lib.sql", FIXTURES / "dataset.sql"]


def _run_python(db: str, tz: str, out: Path) -> None:
    pg.run_python("tests.retention_parity.python_runner", db, tz, out,
                  "--scenarios", str(FIXTURES / "scenarios.json"))


def _run_go(db: str, tz: str, out: Path) -> None:
    pg.run_go(GO_PACKAGE, db, tz, FIXTURES / "scenarios.json", out)


def _dump(db: str) -> dict:
    norm = pg.norm_uuid

    dump: dict = {}
    import psycopg

    with psycopg.connect(pg.urls(db)[1], autocommit=True) as conn:
        def rows(sql: str) -> list[list]:
            return pg.rows(conn, sql)

        dump["runs"] = rows(
            f"""SELECT workspace_id, {norm('id')}, status, requested_by, policy_snapshot::text,
                       deleted_counts::text, error, started_at::text, finished_at::text
                FROM retention_runs ORDER BY 1, 2, 3, 4, 5, 6, 7, 8, 9""")
        # The substring-compared database error differs in wording by driver.
        for row in dump["runs"]:
            if row[0] == "ws-trigger" and row[6] and "boom" in row[6] and not row[6].startswith(("Final failure: ", "Queue retry")):
                row[6] = "<error containing boom>"
        dump["policies"] = rows(
            """SELECT workspace_id, enabled, legal_hold, retention_days::text, next_run_at::text
               FROM retention_policies ORDER BY 1""")
        dump["schedules"] = rows(
            "SELECT workspace_id, enabled, next_run_at::text FROM retention_schedules ORDER BY 1")
        jobs = rows(
            f"""SELECT type, status, priority, max_retries, retry_count, workspace_id, fire_key,
                       {norm('payload::jsonb::text')}, next_run_at::text, error, completed_at::text,
                       worker_id, locked_at::text, started_at::text, last_heartbeat::text
                FROM jobs WHERE type = 'retention_enforce'""")
        dump["jobs"] = sorted(jobs, key=lambda r: json.dumps(r, default=str))
        dump["targets"] = {
            table: {ws: sorted(k) for ws, k in pg.group(rows(
                f"SELECT workspace_id, {key} FROM {table} ORDER BY 1, 2")).items()}
            for table, key in TARGET_KEYS.items()
        }
    return dump


def _expected_deleted(days: int, date_only: bool = False) -> int:
    """Seeded rows strictly older than `days` before the frozen instant."""
    ages = [1, 30, 45, 90, 100, 179, 180, 181, 364, 365, 366, 400, 3650]
    return sum(
        (a > days) if date_only else (a * 86400 + o > days * 86400)
        for a in ages for o in (1, 0, -1)
    )


@needs_pg
@needs_go
@pytest.mark.postgres
@pytest.mark.parametrize("zone", ZONES, ids=[z[0] for z in ZONES])
def test_python_and_go_leave_identical_state(template_db, tmp_path, zone):
    _, pg_tz, proc_tz = zone
    with pg.DatabasePair(template_db, pg_tz, _seed_files(), "opengtm_retpar") as dbs:
        py_out, go_out = tmp_path / "py.json", tmp_path / "go.json"
        _run_python(dbs.py, proc_tz, py_out)
        _run_go(dbs.go, proc_tz, go_out)

        py_steps, go_steps = json.loads(py_out.read_text()), json.loads(go_out.read_text())
        spec = json.loads((FIXTURES / "scenarios.json").read_text())["steps"]
        problems = pg.compare_steps(spec, py_steps, go_steps)
        assert not problems, "step outcomes differ:\n" + "\n".join(problems)

        py_dump, _ = pg.assert_identical_state(dbs.py, dbs.go, _dump)
        _assert_scenarios_were_exercised(py_dump, py_steps, exact_counts=(pg_tz == proc_tz == "UTC"))


def _assert_scenarios_were_exercised(dump: dict, steps: list, exact_counts: bool) -> None:
    """Guard against vacuous parity: the dataset must really hit each behaviour."""
    runs = {}
    for ws, run_id, status, by, snap, counts, err, started, finished in dump["runs"]:
        runs.setdefault(ws, []).append((run_id, status, by, snap, json.loads(counts), err))

    def only(ws):
        assert len(runs[ws]) == 1, (ws, runs[ws])
        return runs[ws][0]

    assert only("ws-default")[1] == "completed" and only("ws-custom")[1] == "completed"
    assert only("ws-nopolicy")[1] == "pending"
    assert only("ws-hold")[1] == "cancelled" and "legal hold" in only("ws-hold")[5]
    assert [r[1] for r in runs["ws-disabled"]] == ["cancelled"]
    assert only("ws-disabled-manual")[1] == "completed"
    assert only("ws-sched")[1] == "completed" and only("ws-sched")[2] == "scheduler"
    assert "ws-badcat" not in runs and "ws-badint" not in runs   # failed before any run existed
    assert only("ws-midfail")[1] == "failed" and only("ws-midfail")[5].startswith("Final failure: ")
    assert only("ws-trigger")[1] == "failed"
    assert only("ws-snap-str")[1] == "failed" and "timedelta" in only("ws-snap-str")[5]
    assert only("ws-snap-float")[1] == "completed" and only("ws-snap-neg")[1] == "completed"
    assert only("ws-rc-hold")[1] == "cancelled" and only("ws-rc-retry")[1] == "failed"
    assert only("ws-rc-nopolicy")[1] == "failed"
    assert only("ws-rc-long")[5] == "Final failure: " + "é" * 1000

    rows = dump["targets"]
    seeded = 3 * 13
    # Tenant isolation and no-op paths leave every seeded row in place.
    for ws in ("ws-nopolicy", "ws-hold", "ws-disabled", "ws-midfail", "ws-trigger", "ws-snap-str"):
        assert len(rows["governance_audit_events"][ws]) == seeded, ws
        assert len(rows["signals"][ws]) == seeded + 1, ws
    # Rows were really deleted where a run completed.
    assert len(rows["governance_audit_events"]["ws-default"]) < seeded
    assert len(rows["outreach_sends"]["ws-custom"]) < seeded + 1
    assert any(s["error"] for s in steps) and any(s["error"] is None for s in steps)

    if exact_counts:  # UTC everywhere: the absolute numbers are known
        _, _, _, snap, counts, _ = only("ws-default")
        assert counts == {
            "audit": _expected_deleted(365), "llm_usage": _expected_deleted(365, True),
            "signals": _expected_deleted(365), "activation": 2 * _expected_deleted(180),
            "audience_history": _expected_deleted(365), "agent_results": _expected_deleted(180),
            "outreach_history": _expected_deleted(365),
        }
        assert len(rows["governance_audit_events"]["ws-default"]) == seeded - _expected_deleted(365)
        assert len(rows["governance_audit_events"]["ws-custom"]) == seeded - _expected_deleted(90)
    # jobs: next runs were scheduled where expected and nowhere else.
    keys = {j[6]: j[1] for j in dump["jobs"] if j[6]}
    assert keys["retention:ws-default:2026-06-16"] == "pending"
    assert keys["retention:ws-default:2026-06-14"] == "cancelled"
    assert keys["retention:ws-other:2026-06-16"] == "pending"          # other workspace untouched
    assert keys["retention:wsXwild:2026-06-20"] == "cancelled"        # LIKE '_' wildcard quirk
    assert keys["retention:ws-dupkey:2026-06-16"] == "processing"      # no duplicate enqueued
    assert "retention:ws-hold:2026-06-16" in keys and keys["retention:ws-hold:2026-06-16"] == "cancelled"
    assert not [k for k in keys if k.startswith("retention:ws-disabled")]


@needs_go
@pytest.mark.parametrize("tz", ["UTC", "Asia/Kolkata", "America/Los_Angeles"])
def test_pure_helpers_match_over_a_corpus(tmp_path, tz):
    """normalized_days and the cutoff arithmetic of _targets, incl. malformed input."""
    cases = FIXTURES / "pure_cases.json"
    py_out, go_out = tmp_path / "py.json", tmp_path / "go.json"
    import subprocess
    import sys

    res = subprocess.run(
        [sys.executable, "-m", "tests.retention_parity.python_runner",
         "--pure", str(cases), "--out", str(py_out), "--tz", tz],
        cwd=pg.REPO, env=dict(os.environ, TZ=tz), capture_output=True, text=True,
    )
    assert res.returncode == 0, res.stderr
    pg.go_test(GO_PACKAGE, "TestParityPure",
               {"OPENGTM_PARITY_PURE": str(cases), "OPENGTM_PARITY_OUT": str(go_out), "TZ": tz})
    py, go = json.loads(py_out.read_text()), json.loads(go_out.read_text())
    corpus = json.loads(cases.read_text())
    problems = []
    for kind in ("normalize", "cutoff"):
        assert len(py[kind]) == len(go[kind]) == len(corpus[kind])
        for case, p, g in zip(corpus[kind], py[kind], go[kind]):
            if p != g:
                problems.append(f"{kind} {json.dumps(case)}:\n  python {json.dumps(p)}\n  go     {json.dumps(g)}")
    assert not problems, "\n".join(problems)
    # The corpus must contain both successes and failures.
    assert any("error" in r for r in py["normalize"]) and any("ok" in r for r in py["normalize"])
    assert any(r["error"] for r in py["cutoff"]) and any(not r["error"] for r in py["cutoff"])
