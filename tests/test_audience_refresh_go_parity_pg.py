"""Python/Go parity for the audience_refresh job (PG-gated).

    TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
        uv run pytest tests/test_audience_refresh_go_parity_pg.py

For each time-zone combination it clones a migrated template into two fresh
databases, seeds both with tests/audience_refresh_parity/dataset.sql (34 leads,
three dozen audiences isolating one behaviour each, destinations, automation
rules, stale members, reconciliation cases and a second tenant), replays
scenarios.json (the unchanged Python handler and failure reconciler on one
database, the Go executor of internal/jobs/audiencerefresh on the other, both as
NOSUPERUSER NOBYPASSRLS roles under forced RLS, page size 4) and requires identical
audiences, members, membership events, destination runs, schedule mirror and jobs,
and the same error from every step. See tests/parity_harness/pg.py for the recipe.

Two things are normalised because they cannot match: serial values the wall clock
decides (job timestamps of Python's ``add_job``) and the wording of a database error
recorded in ``last_refresh_error`` (driver specific; compared by ``boom`` content).
"""
from __future__ import annotations

import json
import re
from datetime import datetime, timezone
from zoneinfo import ZoneInfo

import pytest

from tests.parity_harness import pg
from tests.parity_harness.pg import ZONES, needs_go, needs_pg

FIXTURES = pg.REPO / "tests" / "audience_refresh_parity"
GO_PACKAGE = "internal/jobs/audiencerefresh"
NOW = datetime(2026, 6, 15, 12, 0, 0, 250000, tzinfo=timezone.utc)


@pytest.fixture(scope="module")
def template_db():
    return pg.shared_template()


def _normal_payload(text: str):
    payload = json.loads(text)
    if isinstance(payload, dict) and isinstance(payload.get("targets"), list):
        payload["targets"] = sorted(payload["targets"], key=lambda t: (t["workbook_id"], t["row_id"]))
    return json.dumps(payload, sort_keys=True)


def _dump(db: str) -> dict:
    norm = pg.norm_uuid

    def collect(conn, rows):
        dump = {}
        dump["audiences"] = rows(
            """SELECT workspace_id, id, refresh_enabled, refresh_interval_minutes, member_count, refreshed_at::text,
                      next_refresh_at::text, refresh_health,
                      CASE WHEN last_refresh_error IS NULL THEN NULL
                           WHEN last_refresh_error ILIKE '%boom%' THEN '<error containing boom>'
                           WHEN last_refresh_error ~ '^(Queue retry|Final failure): ' THEN last_refresh_error
                           ELSE '<error>' END,
                      consecutive_refresh_failures
               FROM audiences ORDER BY 1, 2""")
        # Serial ids are compared by rank: a rolled-back statement consumes values
        # (how many depends on how the statement was batched), which is not part of
        # the contract; the order rows were inserted in is.
        member_rows = rows(
            f"""SELECT audience_id, lead_id, id, snapshot::jsonb::text, {norm('refresh_token')}, joined_at::text,
                       last_seen_at::text FROM audience_members ORDER BY id""")
        for rank, row in enumerate(member_rows):
            row[2] = rank
        dump["members"] = sorted(member_rows, key=lambda r: (r[0], r[1]))
        dump["member_tokens"] = rows(
            """SELECT audience_id, count(DISTINCT refresh_token) FROM audience_members GROUP BY 1 ORDER BY 1""")
        dump["events"] = rows(
            f"""SELECT id, workspace_id, audience_id, lead_id, event_type, snapshot::jsonb::text, {norm('refresh_id')}
                FROM audience_membership_events ORDER BY id""")
        event_rank = {row[0]: rank for rank, row in enumerate(dump["events"])}
        for row in dump["events"]:
            row[0] = event_rank[row[0]]
        dump["event_refreshes"] = rows(
            """SELECT audience_id, count(DISTINCT refresh_id) FROM audience_membership_events GROUP BY 1 ORDER BY 1""")
        dump["destination_runs"] = rows(
            f"""SELECT workspace_id, destination_id, status, requested_by, attempted, succeeded, failed, skipped,
                       CASE WHEN id LIKE 'r-%' THEN id ELSE '<uuid>' END
                FROM destination_runs ORDER BY 2, 4 NULLS FIRST, 3, 9""")
        dump["schedules"] = rows(
            "SELECT audience_id, workspace_id, enabled, next_refresh_at::text FROM audience_schedules ORDER BY 1")
        jobs = rows(
            f"""SELECT type, status, priority, max_retries, retry_count, workspace_id, {norm('fire_key')},
                       {norm('payload::jsonb::text')},
                       CASE WHEN type = 'audience_refresh' THEN next_run_at::text END,
                       error, completed_at::text, worker_id, locked_at::text, started_at::text, last_heartbeat::text
                FROM jobs""")
        def by_rank(text):
            # trigger_eval fire keys embed the serial id of the membership event
            return re.sub(r"audience:(\d+):(entered|exited)",
                          lambda m: f"audience:#{event_rank[int(m.group(1))]}:{m.group(2)}", text)

        for row in jobs:
            row[6] = by_rank(row[6]) if row[6] else row[6]
            row[7] = by_rank(_normal_payload(row[7]))
        dump["jobs"] = sorted(jobs, key=lambda r: json.dumps(r, default=str))
        return dump

    return pg.dump_with(db, collect)


@needs_pg
@needs_go
@pytest.mark.postgres
@pytest.mark.parametrize("zone", ZONES, ids=[z[0] for z in ZONES])
def test_python_and_go_leave_identical_state(template_db, tmp_path, zone):
    _, pg_tz, proc_tz = zone
    scenarios = FIXTURES / "scenarios.json"
    with pg.DatabasePair(template_db, pg_tz, [FIXTURES / "dataset.sql"], "opengtm_arpar") as dbs:
        py_out, go_out = tmp_path / "py.json", tmp_path / "go.json"
        pg.run_python("tests.audience_refresh_parity.python_runner", dbs.py, proc_tz, py_out,
                      "--scenarios", str(scenarios))
        pg.run_go(GO_PACKAGE, dbs.go, proc_tz, scenarios, go_out)

        py_steps, go_steps = json.loads(py_out.read_text()), json.loads(go_out.read_text())
        spec = json.loads(scenarios.read_text())["steps"]
        problems = pg.compare_steps(spec, py_steps, go_steps)
        assert not problems, "step outcomes differ:\n" + "\n".join(problems)

        py_dump, _ = pg.assert_identical_state(dbs.py, dbs.go, _dump)
        _assert_scenarios_were_exercised(py_dump, py_steps, spec, pg_tz)


def _local(dt: datetime, tz: str) -> str:
    """A naive timestamp as the database stores an aware instant: in the session zone."""
    text = dt.astimezone(ZoneInfo(tz)).replace(tzinfo=None).isoformat(sep=" ")
    return text.rstrip("0").rstrip(".") if "." in text else text   # PostgreSQL trims trailing zeros


def _assert_scenarios_were_exercised(dump: dict, steps: list, spec: list, pg_tz: str) -> None:
    """Guard against vacuous parity: the dataset must really hit each behaviour."""
    aud = {a[1]: a for a in dump["audiences"]}
    members: dict = {}
    for audience, lead, *_rest in dump["members"]:
        members.setdefault(audience, []).append(lead)
    events: dict = {}
    for _id, _ws, audience, lead, kind, snap, _rid in dump["events"]:
        events.setdefault(audience, []).append((lead, kind, json.loads(snap)))
    jobs = dump["jobs"]
    fire = {j[6]: j[1] for j in jobs if j[0] == "audience_refresh" and j[6]}

    def expect_errors(note_prefix):
        return [s["error"] for s, sp in zip(steps, spec) if sp.get("note", "").startswith(note_prefix)]

    # filters: each audience's membership
    assert len(members["a-all"]) == 34 and 10 not in members["a-all"] and 35 in members["a-all"]
    assert len(events["a-all"]) == 34 + 2
    assert {e[:2] for e in events["a-all"][-2:]} == {(35, "entered"), (10, "exited")}
    assert members["a-city"] == [3, 6, 9, 12, 15, 18, 21, 24, 27, 30, 33]
    assert sorted(members["a-ids"]) == [1, 2, 3] and "a-ids-empty" not in members
    assert sorted(members["a-ids-null"]) == [2, 5, 8, 14, 17, 20, 23, 26, 29, 32]
    assert len(members["a-falsy"]) == len(members["a-nullfilters"]) == 34
    assert aud["a-ids-empty"][4] == 0 and aud["a-ids-empty"][7] == "healthy"
    assert sorted(members["a-score"]) == [1, 2, 3]
    assert members["a-jobs"] and members["a-spec"] and members["a-email-yes"] and members["a-email-no"]
    assert members["a-nophone"] and members["a-score"] and members["a-score-zero"] and members["a-size"]
    assert members["a-search"] and members["a-search-utf"] == [5]
    assert all(aud[a][4] == len(members.get(a, [])) for a in aud if a.startswith("a-") and aud[a][5] is not None)

    # diff: stale snapshot rewritten, unseen/falsy members exit with an event
    assert sorted(members["a-members"]) == [1, 2, 3]
    exits = {lead: snap for lead, kind, snap in events["a-members"] if kind == "exited"}
    assert set(exits) == {77, 78, 79} and exits[78] == {} and exits[79] == {} and exits[77]["company"] == "gone"
    # the Lead dataclass's created_at/updated_at fallback is the frozen instant
    snap3 = next(json.loads(m[3]) for m in dump["members"] if m[0] == "a-all" and m[1] == 3)
    assert snap3["created_at"] == "2026-06-15T12:00:00.250000+00:00" and snap3["updated_at"] == "2026-06-15T12:00:00.250000+00:00"

    # shrink: members across several pages exit
    assert "a-shrink" not in members and aud["a-shrink"][4] == 0
    assert len([e for e in events["a-shrink"] if e[1] == "exited"]) == len([e for e in events["a-shrink"] if e[1] == "entered"]) > 8

    # destinations: a sync only for enabled destinations without an active run, and not when nothing changed
    runs = [r for r in dump["destination_runs"] if r[3] == "audience_refresh"]
    assert sorted(r[1] for r in runs if r[0] == "ws-ar") == ["d-done", "d-noop", "d-noop", "d-ok"]
    assert len([j for j in jobs if j[0] == "audience_destination_sync"]) == 4

    # automations: rules fire only while enabled, per matching rule and event
    evals = [json.loads(j[7]) for j in jobs if j[0] == "trigger_eval"]
    assert sorted((e["trigger_id"], e["fire_source"]) for e in evals) == [
        ("t-enter-any", "audience_entered"), ("t-enter-any", "audience_entered"),
        ("t-enter-this", "audience_entered"), ("t-enter-this", "audience_entered"),
        ("t-exit", "audience_exited"),
    ]
    assert all(e["fire_key"].startswith("audience:") and e["dry_run"] is False for e in evals)
    assert "a-auto-off" in members and not any("a-auto-off" in str(e) for e in evals)

    # failures: rolled back, degraded, next occurrence booked, job error surfaced
    # The reconciler reads the stored naive next_refresh_at as UTC (Python's _as_utc), so
    # in a session zone behind UTC the handler's own booking looks "past" and the two
    # reconcile steps count the failure again (in both implementations).
    booked_in_future = _local(NOW.replace(minute=30), pg_tz) > _local(NOW, "UTC")
    for failing in ("a-fail", "a-bad-ids", "a-bad-status", "a-bad-min", "a-bad-shape"):
        a = aud[failing]
        expected = 3 if failing == "a-fail" and not booked_in_future else 1
        assert a[7] == "degraded" and a[9] == expected and a[8] is not None and failing not in members, failing
        assert any(k.startswith(f"audience_refresh:{failing}:") and v == "pending" for k, v in fire.items()), failing
    assert aud["a-fail"][8] == ("<error containing boom>" if booked_in_future else "Final failure: worker timeout")
    assert all(e is not None for e in expect_errors("a-fail: database error"))

    # scheduling
    assert fire["audience_refresh:a-i5:2026-06-15T12:15:00.250000+00:00"] == "pending"
    assert fire["audience_refresh:a-i0:2026-06-15T13:00:00.250000+00:00"] == "pending"
    assert fire["audience_refresh:a-ibig:2026-06-22T12:00:00.250000+00:00"] == "pending"
    assert fire["audience_refresh:a-all:2026-06-15T12:30:00.250000+00:00"] == "pending"
    assert fire["audience_refresh:a-all:2026-06-15T11:30:00+00:00"] == "cancelled"
    assert fire["audience_refresh:a-all:2026-06-15T11:45:00+00:00"] == "processing"
    assert fire["audience_refresh:a-disabled:2026-06-15T12:30:00+00:00"] == "cancelled"
    assert fire["audience_refresh:a-disabled:2026-06-15T13:30:00+00:00"] == "processing"
    assert fire["audience_refresh:a-ghost:2026-06-15T12:30:00+00:00"] == "cancelled"
    assert fire["audience_refresh:aXw:2026-06-15T15:00:00+00:00"] == "cancelled"        # '_' wildcard quirk
    assert fire["audience_refresh:a-dup:2026-06-15T12:30:00.250000+00:00"] == "processing"
    assert fire["audience_refresh:a-other:2026-06-15T12:30:00+00:00"] == "pending"      # other tenant untouched
    sched = {s[0]: s for s in dump["schedules"]}
    assert "a-disabled" not in sched and "a-ghost" not in sched and sched["a-other"][3] is not None
    assert sched["a-all"][2] is True and sched["a-all"][3] == _local(NOW.replace(minute=30), pg_tz)
    assert aud["a-disabled"][5] is None and aud["a-other"][4] == 0 and "a-other" not in members

    # reconciliation
    assert aud["a-rec1"][7:10] == ["degraded", "Queue retry: worker timeout", 1]
    assert aud["a-rec1"][6] == "2026-06-15 12:02:00" and sched["a-rec1"][3] == "2026-06-15 12:02:00"
    assert aud["a-rec2"][7:10] == ["degraded", "Final failure: worker timeout", 3]
    assert fire["audience_refresh:a-rec2:2026-06-15T12:45:00.250000+00:00"] == "pending"
    assert aud["a-rec3"][7] == "healthy" and aud["a-rec3"][9] == 0 and aud["a-rec3b"][7] == "healthy"
    assert aud["a-rec4"][9] == 1 and aud["a-rec5"][7] == "healthy"
    assert aud["a-rec7"][8] == "Final failure: " + "é" * 985
    assert aud["a-rec8"][9] == 1 and aud["a-rec9"][7:10] == ["degraded", "Final failure: audience refresh failed", 4]
    assert aud["a-rec10"][9] == 1 and aud["a-rec11"][9] == 1
    errors = {sp.get("job"): s["error"] for s, sp in zip(steps, spec) if sp["op"] == "reconcile"}
    assert errors[3006] and "requires workspace_id and audience_id" in errors[3006]
