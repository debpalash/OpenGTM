"""Progress events are stamped and delivered only to their workspace."""

from apps.api.services.leadgen.progress import ProgressBus, progress


def test_bound_jobs_route_only_to_matching_workspace(monkeypatch):
    monkeypatch.delenv("REDIS_URL", raising=False)
    published = []
    monkeypatch.setattr(
        ProgressBus,
        "_publish_redis",
        lambda self, workspace_id, event: published.append((workspace_id, event)),
    )
    with progress._sub_lock:
        progress._events.clear()
        progress._subscribers.clear()
        progress._job_workspaces.clear()

    one = progress.subscribe("W1")
    two = progress.subscribe("W2")
    progress.bind_job("job-one", "W1")
    progress.bind_job("job-two", "W2")

    event_one = progress.emit("job_progress", {"job_id": "job-one", "message": "one"})
    event_two = progress.emit("job_progress", {"job_id": "job-two", "message": "two"})
    progress.emit("cli_progress", {"message": "unscoped"})

    assert event_one["workspace_id"] == "W1"
    assert event_two["workspace_id"] == "W2"
    assert [event["message"] for event in one] == ["one"]
    assert [event["message"] for event in two] == ["two"]
    assert [event["message"] for event in progress.recent(10, "W1")] == ["one"]
    assert [workspace_id for workspace_id, _ in published] == ["W1", "W2"]

    progress.unsubscribe(one)
    progress.unsubscribe(two)
