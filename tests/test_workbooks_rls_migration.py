"""Migration backfill / NULL-abort tests for the workbooks RLS cutover (OD-1a).

Always-on (SQLite, in-process alembic). Drives the real migration
``e5f6a7b8c9d0`` from the prior head ``d4e5f6a7b8c9`` against a throwaway SQLite
file and asserts the OD-1a "backfill-then-block" policy:

  * single workspace  → NULL-workspace workbooks (and their children) backfill to
    that workspace;
  * a 'main' workspace → backfill to it when multiple workspaces exist;
  * ambiguous (≥2 workspaces, no 'main') with NULL workbooks → the migration
    ABORTS rather than guess a tenant.

The workspace meta store lives outside the migration bind, so the fallback is
resolved via the workspace manager — monkeypatched here to drive each scenario.
"""

import os
import sqlite3

import pytest
from alembic import command
from alembic.config import Config

PRIOR_HEAD = "d4e5f6a7b8c9"
NEW_HEAD = "e5f6a7b8c9d0"

_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))


class _WS:
    def __init__(self, wid, slug):
        self.id = wid
        self.slug = slug


def _cfg(db_path, monkeypatch):
    monkeypatch.setenv("DATABASE_URL", f"sqlite:///{db_path}")
    return Config(os.path.join(_REPO_ROOT, "alembic.ini"))


def _seed_pre_migration(db_path):
    """At PRIOR_HEAD: a NULL-workspace workbook with one child row of each kind
    (children have no workspace_id column yet)."""
    con = sqlite3.connect(db_path)
    con.execute("INSERT INTO workbooks (id, name) VALUES ('wbX', 'legacy')")
    con.execute("INSERT INTO workbook_rows (workbook_id, position, data) VALUES ('wbX', 0, '{}')")
    con.execute(
        "INSERT INTO workbook_enrichments (workbook_id, lead_id, column_id, status) "
        "VALUES ('wbX', 1, 'c', 'complete')"
    )
    con.execute("INSERT INTO workbook_activity (workbook_id, kind) VALUES ('wbX', 'info')")
    con.execute(
        "INSERT INTO cell_traces (workbook_id, lead_id, column_id, goal) "
        "VALUES ('wbX', 1, 'c', 'g')"
    )
    con.commit()
    con.close()


def _patch_workspaces(monkeypatch, workspaces):
    import apps.api.services.workspace.manager as wsm
    monkeypatch.setattr(wsm, "list_workspaces", lambda: list(workspaces))


def test_backfill_single_workspace(tmp_path, monkeypatch):
    db_path = str(tmp_path / "mig_single.db")
    cfg = _cfg(db_path, monkeypatch)
    command.upgrade(cfg, PRIOR_HEAD)
    _seed_pre_migration(db_path)

    # Exactly one workspace (slug need not be 'main').
    _patch_workspaces(monkeypatch, [_WS("ws_solo", "acme")])
    command.upgrade(cfg, NEW_HEAD)

    con = sqlite3.connect(db_path)
    assert con.execute("SELECT workspace_id FROM workbooks WHERE id='wbX'").fetchone()[0] == "ws_solo"
    for tbl in ("workbook_rows", "workbook_enrichments", "workbook_activity", "cell_traces"):
        ws = con.execute(f"SELECT workspace_id FROM {tbl} WHERE workbook_id='wbX'").fetchone()[0]
        assert ws == "ws_solo", f"{tbl} not backfilled from parent"
    con.close()


def test_backfill_main_workspace(tmp_path, monkeypatch):
    db_path = str(tmp_path / "mig_main.db")
    cfg = _cfg(db_path, monkeypatch)
    command.upgrade(cfg, PRIOR_HEAD)
    _seed_pre_migration(db_path)

    # Multiple workspaces, one named 'main' → backfill to it.
    _patch_workspaces(monkeypatch, [_WS("ws_a", "alpha"), _WS("ws_main", "main")])
    command.upgrade(cfg, NEW_HEAD)

    con = sqlite3.connect(db_path)
    assert con.execute("SELECT workspace_id FROM workbooks WHERE id='wbX'").fetchone()[0] == "ws_main"
    assert con.execute("SELECT workspace_id FROM workbook_rows WHERE workbook_id='wbX'").fetchone()[0] == "ws_main"
    con.close()


def test_ambiguous_null_aborts(tmp_path, monkeypatch):
    db_path = str(tmp_path / "mig_abort.db")
    cfg = _cfg(db_path, monkeypatch)
    command.upgrade(cfg, PRIOR_HEAD)
    _seed_pre_migration(db_path)

    # ≥2 workspaces, none named 'main', and a NULL-workspace workbook → abort
    # (never guess a tenant).
    _patch_workspaces(monkeypatch, [_WS("ws_a", "alpha"), _WS("ws_b", "beta")])
    with pytest.raises(Exception) as ei:
        command.upgrade(cfg, NEW_HEAD)
    assert "NULL workspace_id" in str(ei.value) or "ambiguous" in str(ei.value)

    # The aborted upgrade left the schema at the prior head (no partial cutover).
    con = sqlite3.connect(db_path)
    ver = con.execute("SELECT version_num FROM alembic_version").fetchone()[0]
    con.close()
    assert ver == PRIOR_HEAD


def test_no_nulls_upgrades_clean(tmp_path, monkeypatch):
    """A DB with no NULL-workspace workbooks upgrades without touching the
    workspace manager at all (the common fresh-install path)."""
    db_path = str(tmp_path / "mig_clean.db")
    cfg = _cfg(db_path, monkeypatch)
    command.upgrade(cfg, PRIOR_HEAD)

    con = sqlite3.connect(db_path)
    con.execute("INSERT INTO workbooks (id, name, workspace_id) VALUES ('wbY', 'ok', 'ws_z')")
    con.execute(
        "INSERT INTO workbook_rows (workbook_id, position, data) VALUES ('wbY', 0, '{}')"
    )
    con.commit()
    con.close()

    # Make the manager explode if consulted — it must not be needed here.
    import apps.api.services.workspace.manager as wsm
    monkeypatch.setattr(wsm, "list_workspaces",
                        lambda: (_ for _ in ()).throw(AssertionError("should not resolve")))
    command.upgrade(cfg, NEW_HEAD)

    con = sqlite3.connect(db_path)
    assert con.execute("SELECT workspace_id FROM workbook_rows WHERE workbook_id='wbY'").fetchone()[0] == "ws_z"
    con.close()
