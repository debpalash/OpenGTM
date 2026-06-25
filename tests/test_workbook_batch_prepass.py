"""
Cost lever B — Message Batches pre-pass in the workbook column runner.

These tests never touch the network or a real Anthropic batch: `llm` is patched
so we control whether Anthropic is the serving provider and what the batch
returns. They assert that:
  - eligible columns are exactly the independent, text-output ai_formula columns
    (JSON AI columns and dependency-coupled columns stay on the sync path);
  - the batch path is taken only when Anthropic serves AND the run clears the
    row threshold, mapping results back by custom_id and removing handled cells;
  - below the threshold, and for non-Anthropic providers, the pre-pass no-ops
    (everything falls through to the synchronous per-row loop);
  - a batch submission error degrades gracefully to the sync path.
"""
import asyncio
import contextlib

import pytest

from apps.api.services.workbook import enrichment as E
from apps.api.services.workbook import column_deps as cd


# ── independent_columns / eligibility ────────────────────────────────

def test_independent_columns_excludes_coupled():
    cols = [
        {"id": "a", "type": "ai_formula", "prompt": "x {company}"},
        {"id": "b", "type": "ai_formula", "prompt": "y {a}"},   # depends on a
        {"id": "c", "type": "ai_formula", "prompt": "z {website}"},  # base ref only
    ]
    indep = {c["id"] for c in cd.independent_columns(cols)}
    assert indep == {"c"}  # a is referenced-by, b references-out


def test_batch_eligible_only_independent_text_ai():
    cols = [
        {"id": "a", "type": "ai_formula", "prompt": "x {website}", "output_format": "text"},
        {"id": "j", "type": "ai_formula", "prompt": "x {website}", "output_format": "json"},
        {"id": "r", "type": "research", "prompt": "x {website}"},
        {"id": "coupled", "type": "ai_formula", "prompt": "uses {dep}", "output_format": "text"},
        {"id": "dep", "type": "ai_formula", "prompt": "x {website}", "output_format": "text"},
        {"id": "e", "type": "ai_formula", "prompt": "", "output_format": "text"},  # empty
    ]
    eligible = {c["id"] for c in E._batch_eligible_columns(cols)}
    # only `a` qualifies: json excluded, research excluded, `coupled`/`dep` are
    # dependency-linked (not independent), empty-prompt excluded.
    assert eligible == {"a"}


def test_apply_batch_handled_prunes_rows_and_cells():
    work = [
        ({"id": 1}, [{"id": "a"}, {"id": "b"}]),
        ({"id": 2}, [{"id": "a"}]),
    ]
    out = E._apply_batch_handled(work, {(1, "a"), (2, "a")})
    # row 1 keeps col b; row 2 fully handled → dropped
    assert out == [({"id": 1}, [{"id": "b"}])]


# ── pre-pass gating + result mapping ─────────────────────────────────

@contextlib.contextmanager
def _patched(monkeypatch, *, anthropic, batch_return, capture):
    """Patch llm, _set_enrichment, and SessionLocal for an offline pre-pass run."""
    prov = {"id": "anthropic", "model": "claude-opus-4-8"} if anthropic else None
    monkeypatch.setattr(E.llm, "anthropic_provider", lambda: prov)

    async def _fake_batch(requests, prov=None, **kw):
        capture["requests"] = requests
        if isinstance(batch_return, Exception):
            raise batch_return
        return batch_return

    monkeypatch.setattr(E.llm, "batch_complete_anthropic", _fake_batch)

    def _fake_set(db, wb, lead_id, col_id, value, status, provider=None, error=None, metadata=None):
        capture.setdefault("written", []).append((lead_id, col_id, value, status, provider))

    monkeypatch.setattr(E, "_set_enrichment", _fake_set)

    class _FakeSession:
        def __enter__(self): return self
        def __exit__(self, *a): return False
        def commit(self): pass

    monkeypatch.setattr(E, "SessionLocal", lambda: _FakeSession())
    monkeypatch.setattr(E, "BATCH_MIN_ROWS", 3, raising=False)
    monkeypatch.setattr(E, "BATCH_ENABLED", True, raising=False)
    yield capture


def _work(n, col_id="ai", prompt="Summarize {company}", fmt="text"):
    col = {"id": col_id, "type": "ai_formula", "prompt": prompt, "output_format": fmt}
    return [({"id": i, "company": f"Co{i}"}, [col]) for i in range(1, n + 1)]


def test_prepass_runs_and_maps_results(monkeypatch):
    cap = {}
    ret = {f"{i}::ai": f"summary {i}" for i in range(1, 5)}
    with _patched(monkeypatch, anthropic=True, batch_return=ret, capture=cap):
        handled, completed = asyncio.run(
            E._run_ai_batch_prepass("wb1", _work(4), columns_config=[], redis_client=None)
        )
    assert completed == 4
    assert handled == {(i, "ai") for i in range(1, 5)}
    # custom_ids are lead::col; per-row prompt is resolved (company substituted)
    cids = {r["custom_id"] for r in cap["requests"]}
    assert cids == {f"{i}::ai" for i in range(1, 5)}
    assert any("Co1" in r["prompt"] for r in cap["requests"])
    assert all(p == "ai" for *_, p in cap["written"])


def test_prepass_partial_results_leaves_rest_for_sync(monkeypatch):
    cap = {}
    # batch only returns 2 of 4 → the other two fall through to the sync path
    ret = {"1::ai": "s1", "3::ai": "s3"}
    with _patched(monkeypatch, anthropic=True, batch_return=ret, capture=cap):
        handled, completed = asyncio.run(
            E._run_ai_batch_prepass("wb1", _work(4), columns_config=[], redis_client=None)
        )
    assert completed == 2
    assert handled == {(1, "ai"), (3, "ai")}


def test_prepass_skips_below_threshold(monkeypatch):
    cap = {}
    with _patched(monkeypatch, anthropic=True, batch_return={"1::ai": "s"}, capture=cap):
        handled, completed = asyncio.run(
            E._run_ai_batch_prepass("wb1", _work(2), columns_config=[], redis_client=None)
        )
    assert handled == set() and completed == 0
    assert "requests" not in cap  # never submitted


def test_prepass_skips_non_anthropic(monkeypatch):
    cap = {}
    with _patched(monkeypatch, anthropic=False, batch_return={"1::ai": "s"}, capture=cap):
        handled, completed = asyncio.run(
            E._run_ai_batch_prepass("wb1", _work(50), columns_config=[], redis_client=None)
        )
    assert handled == set() and completed == 0
    assert "requests" not in cap


def test_prepass_disabled_env(monkeypatch):
    cap = {}
    with _patched(monkeypatch, anthropic=True, batch_return={"1::ai": "s"}, capture=cap):
        monkeypatch.setattr(E, "BATCH_ENABLED", False, raising=False)
        handled, completed = asyncio.run(
            E._run_ai_batch_prepass("wb1", _work(50), columns_config=[], redis_client=None)
        )
    assert handled == set() and completed == 0


def test_prepass_graceful_on_batch_error(monkeypatch):
    cap = {}
    with _patched(monkeypatch, anthropic=True,
                  batch_return=RuntimeError("batch api down"), capture=cap):
        handled, completed = asyncio.run(
            E._run_ai_batch_prepass("wb1", _work(5), columns_config=[], redis_client=None)
        )
    # submission failed → no cells handled, sync path takes over
    assert handled == set() and completed == 0


def test_prepass_excludes_json_columns(monkeypatch):
    cap = {}
    with _patched(monkeypatch, anthropic=True, batch_return={}, capture=cap):
        handled, completed = asyncio.run(
            E._run_ai_batch_prepass(
                "wb1", _work(50, fmt="json"), columns_config=[], redis_client=None
            )
        )
    # all columns are json-output → nothing eligible → never submitted
    assert handled == set() and completed == 0
    assert "requests" not in cap
