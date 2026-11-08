"""Guards for the CLA workflows, which hold a write token on untrusted events.

pull_request_target and issue_comment run with a write token even for fork pull
requests, so these workflows must check out only the base branch's
.github/scripts and never pull-request code.
"""
from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

import pytest
import yaml

_REPO = Path(__file__).resolve().parents[1]
_WORKFLOWS = [_REPO / ".github" / "workflows" / f"{name}.yml" for name in ("cla", "cla-backfill", "cla-reminders")]

_spec = importlib.util.spec_from_file_location("cla_check", _REPO / ".github" / "scripts" / "cla_check.py")
cla = importlib.util.module_from_spec(_spec)
sys.modules["cla_check"] = cla
_spec.loader.exec_module(cla)


def _steps(path: Path) -> list[dict]:
    data = yaml.safe_load(path.read_text(encoding="utf-8"))
    return [step for job in data["jobs"].values() for step in job.get("steps", [])]


@pytest.mark.parametrize("path", _WORKFLOWS, ids=lambda p: p.name)
def test_checkout_is_base_branch_scripts_only(path):
    checkouts = [s for s in _steps(path) if str(s.get("uses", "")).startswith("actions/checkout@")]
    assert checkouts, path
    for step in checkouts:
        options = step.get("with") or {}
        assert "ref" not in options and "repository" not in options, "must not check out pull-request code"
        assert options.get("persist-credentials") is False
        assert options.get("sparse-checkout") == ".github/scripts"


@pytest.mark.parametrize("path", _WORKFLOWS, ids=lambda p: p.name)
def test_steps_never_interpolate_untrusted_event_text(path):
    for step in _steps(path):
        run = step.get("run", "")
        assert "${{" not in run, f"{path.name}: pass event data through env, not into the script"


def test_agreement_and_contributing_carry_the_sign_phrase():
    for name in ("CLA.md", "CONTRIBUTING.md"):
        text = " ".join((_REPO / name).read_text(encoding="utf-8").split())
        assert cla.SIGN_PHRASE in text, name
    assert (_REPO / cla.DOCUMENT_PATH).is_file()
    assert (_REPO / "CLA-CORPORATE.md").is_file()
