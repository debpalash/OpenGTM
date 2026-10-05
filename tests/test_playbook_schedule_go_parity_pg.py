"""Python/Go parity for the research_playbook_schedule job (PG-gated).

    TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
        uv run pytest tests/test_playbook_schedule_go_parity_pg.py

For each time-zone combination it clones a migrated template into two fresh
databases, seeds both with tests/playbook_schedule_parity/dataset.sql, replays
scenarios.json (the unchanged Python handler on one database, the Go handler of
internal/jobs/playbooksched on the other, both as NOSUPERUSER NOBYPASSRLS roles
under forced RLS) and requires identical playbook runs, playbooks, schedule
mirror and jobs, and the same error from every step. See
tests/parity_harness/pg.py for the shared recipe.
"""
from __future__ import annotations

import json
from datetime import datetime, timezone
from zoneinfo import ZoneInfo

import pytest

from tests.parity_harness import pg
from tests.parity_harness.pg import ZONES, needs_go, needs_pg

FIXTURES = pg.REPO / "tests" / "playbook_schedule_parity"
GO_PACKAGE = "internal/jobs/playbooksched"


@pytest.fixture(scope="module")
def template_db():
    return pg.shared_template()


def _dump(db: str) -> dict:
    norm = pg.norm_uuid

    def collect(conn, rows):
        dump = {}
        dump["runs"] = rows(
            f"""SELECT workspace_id, {norm('id')}, playbook_id, audience_id, status, prompt_version,
                       prompt_snapshot, steps_snapshot::jsonb::text, max_members, attempted, succeeded,
                       failed, error, requested_by, started_at::text, finished_at::text
                FROM playbook_runs ORDER BY 1, 3, 5, 2""")
        dump["playbooks"] = rows(
            """SELECT workspace_id, id, enabled, schedule_audience_id, schedule_interval_minutes,
                      next_run_at::text, version FROM research_playbooks ORDER BY 1, 2""")
        dump["schedules"] = rows(
            "SELECT playbook_id, workspace_id, enabled, next_run_at::text FROM playbook_schedules ORDER BY 1")
        # Serial job ids are not part of the contract; the run job's timestamps come
        # from the wall clock (add_job reads the real time) and are not compared.
        jobs = rows(
            f"""SELECT type, status, priority, max_retries, retry_count, workspace_id, {norm('fire_key')},
                       {norm('payload::jsonb::text')},
                       CASE WHEN type = 'research_playbook_run' THEN NULL ELSE next_run_at::text END,
                       error, completed_at::text, worker_id, locked_at::text, started_at::text,
                       last_heartbeat::text
                FROM jobs""")
        dump["jobs"] = sorted(jobs, key=lambda r: json.dumps(r, default=str))
        return dump

    return pg.dump_with(db, collect)


@needs_pg
@needs_go
@pytest.mark.postgres
@pytest.mark.parametrize("zone", ZONES, ids=[z[0] for z in ZONES])
def test_python_and_go_leave_identical_state(template_db, tmp_path, zone):
    _, pg_tz, proc_tz = zone
    seed = [FIXTURES / "dataset.sql"]
    scenarios = FIXTURES / "scenarios.json"
    with pg.DatabasePair(template_db, pg_tz, seed, "opengtm_pbpar") as dbs:
        py_out, go_out = tmp_path / "py.json", tmp_path / "go.json"
        pg.run_python("tests.playbook_schedule_parity.python_runner", dbs.py, proc_tz, py_out,
                      "--scenarios", str(scenarios))
        pg.run_go(GO_PACKAGE, dbs.go, proc_tz, scenarios, go_out)

        py_steps, go_steps = json.loads(py_out.read_text()), json.loads(go_out.read_text())
        spec = json.loads(scenarios.read_text())["steps"]
        problems = pg.compare_steps(spec, py_steps, go_steps)
        assert not problems, "step outcomes differ:\n" + "\n".join(problems)

        py_dump, _ = pg.assert_identical_state(dbs.py, dbs.go, _dump)
        _assert_scenarios_were_exercised(py_dump, py_steps, pg_tz)


def _assert_scenarios_were_exercised(dump: dict, steps: list, pg_tz: str) -> None:
    """Guard against vacuous parity: the dataset must really hit each behaviour."""
    runs = {}
    for ws, _id, pbid, aud, status, ver, prompt, steps_snap, maxm, *_ in dump["runs"]:
        runs.setdefault(pbid, []).append((status, ver, prompt, json.loads(steps_snap), maxm, aud))
    new = lambda pbid: [r for r in runs.get(pbid, []) if r[2] != "old"]  # noqa: E731

    # a tick on pb-a: one new pending run carrying the playbook snapshot, a second tick adds none
    assert new("pb-a") == [("pending", 3, "prompt for pb-a", [{"tool": "search"}], 100, "aud-1")]
    assert [r[0] for r in runs["pb-h"]] == ["running"] and [r[0] for r in runs["pb-i"]].count("pending") == 1
    assert new("pb-j")[0][3] == [] and new("pb-k")[0][3] == []
    assert new("pb-l")[0][3] == {"a": 1, "b": [1, 2.5, "x"]}
    assert "pb-f" not in runs and "pb-g" not in runs and "pb-ghost" not in runs
    assert len(new("pb-d")) == 1 and len(new("pb-e")) == 1
    # per-step outcomes: invalid payloads raise, valid ticks do not
    errors = [s["error"] for s in steps]
    assert errors[:14] == [None] * 14 and all(errors[14:18]) and errors[18:] == [None, None]
    assert all("requires workspace_id and playbook_id" in e for e in errors[14:18])

    sched = {s[0]: s for s in dump["schedules"]}
    # naive timestamps are stored in the database session zone
    local = datetime(2026, 6, 15, 12, 30, 0, 123456, tzinfo=timezone.utc).astimezone(ZoneInfo(pg_tz))
    assert sched["pb-a"][2:] == [True, local.replace(tzinfo=None).isoformat(sep=" ")]
    assert sched["pb-d"][2] is False and sched["pb-d"][3] is None
    assert "pb-f" not in sched and "pb-g" not in sched and "pb-ghost" not in sched
    assert sched["pb-o"][2] is True                      # another tenant untouched

    # schedule occurrences: interval clamping and the iso fire_key (microseconds kept)
    fire = {j[6]: j[1] for j in dump["jobs"] if j[0] == "research_playbook_schedule" and j[6]}
    assert fire["playbook_schedule:pb-a:2026-06-15T12:30:00.123456+00:00"] == "pending"
    assert fire["playbook_schedule:pb-b:2026-06-15T12:15:00.123456+00:00"] == "pending"
    assert fire["playbook_schedule:pb-c:2026-06-22T12:00:00.123456+00:00"] == "pending"
    assert fire["playbook_schedule:pb-a:2026-06-15T11:30:00+00:00"] == "cancelled"
    assert fire["playbook_schedule:pb-a:2026-06-15T11:45:00+00:00"] == "processing"
    assert fire["playbook_schedule:pb-f:2026-06-15T12:30:00+00:00"] == "cancelled"
    assert fire["playbook_schedule:pb-f:2026-06-15T13:30:00+00:00"] == "processing"
    assert fire["playbook_schedule:pbXw:2026-06-15T15:00:00+00:00"] == "cancelled"   # '_' wildcard quirk
    assert fire["playbook_schedule:pb-o:2026-06-15T12:30:00+00:00"] == "pending"
    assert fire["playbook_schedule:pb-dup:2026-06-15T12:30:00.123456+00:00"] == "processing"
    assert not [k for k in fire if k.startswith(("playbook_schedule:pb-d:", "playbook_schedule:pb-e:"))]
    run_jobs = [j for j in dump["jobs"] if j[0] == "research_playbook_run"]
    assert len(run_jobs) == sum(len(new(p)) for p in runs)      # one run job per created run
    assert all(j[5] and j[6].startswith("playbook:<uuid>") for j in run_jobs)
