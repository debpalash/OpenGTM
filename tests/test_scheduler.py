from apps.api import scheduler


def test_reconcile_runs_every_bootstrap_even_when_one_fails(monkeypatch):
    called = []

    def good():
        called.append("good")
        return 1

    def bad():
        called.append("bad")
        raise RuntimeError("broken subsystem")

    monkeypatch.setattr(
        scheduler,
        "_bootstrap_functions",
        lambda: (("first", bad), ("second", good)),
    )

    result = scheduler.reconcile_once()
    assert called == ["bad", "good"]
    assert result["second"] == 1
    assert result["first"].startswith("error:")
