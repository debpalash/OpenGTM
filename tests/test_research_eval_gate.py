"""
Research-column eval harness + regression gate (offline).

Drives apps.api.services.leadgen.enrichment.eval.research_eval over deterministic
fixtures for BOTH the native tool-use path and the legacy ReAct path, and asserts
the pass/fail gate so §10's "compare to baseline" can block a rollout (AC10).
"""
from apps.api.services.leadgen.enrichment.eval import research_eval as RE


def test_research_eval_native_meets_or_beats_legacy_and_cites():
    native = RE.run_eval(native=True)
    legacy = RE.run_eval(native=False)
    # Both score full correctness on the deterministic fixtures.
    assert native.correctness >= legacy.correctness
    # Native carries citations (validated against the fetched set); legacy does not.
    assert native.citation_presence >= 0.9
    passed, msg = RE.gate(native, legacy, tolerance=0.0, citation_floor=0.9)
    assert passed, msg


def test_research_eval_gate_fails_on_regression():
    # A synthetic regression (native worse than legacy) must FAIL the gate.
    native = RE.EvalResult("native", 2, correctness=0.0, citation_presence=1.0)
    legacy = RE.EvalResult("legacy", 2, correctness=1.0, citation_presence=0.0)
    passed, _ = RE.gate(native, legacy, tolerance=0.0, citation_floor=0.9)
    assert passed is False


def test_research_eval_cli_exit_code():
    assert RE.main(["--json"]) == 0
