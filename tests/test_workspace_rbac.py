from pathlib import Path

import pytest
from fastapi import HTTPException
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.core.tenancy import WorkspaceCtx, require_workspace_role
from apps.api.services.workspace import manager
from apps.api.database import Base
from apps.api.models import User as UserModel
from apps.api.routers.governance import (
    MemberCreate,
    MemberRoleUpdate,
    PermissionUpdate,
    add_workspace_member,
    get_rbac,
    remove_workspace_member,
    update_rbac,
    update_workspace_member,
)


class User:
    id = 7


def test_permission_overrides_precede_role_defaults(monkeypatch):
    ctx = WorkspaceCtx(user=User(), workspace_id="ws", slug="ws")
    dependency = require_workspace_role("editor", "admin", permission="tables.write")
    monkeypatch.setattr(manager, "member_role", lambda *_: "viewer")
    monkeypatch.setattr(manager, "member_permissions", lambda *_: {"tables.write": "allow"})
    assert dependency(ctx) is ctx
    monkeypatch.setattr(manager, "member_role", lambda *_: "admin")
    monkeypatch.setattr(manager, "member_permissions", lambda *_: {"tables.write": "deny"})
    with pytest.raises(HTTPException, match="Workspace permission denied"):
        dependency(ctx)
    monkeypatch.setattr(manager, "member_role", lambda *_: "owner")
    assert dependency(ctx) is ctx


def test_member_permission_persistence_and_cleanup(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    workspace = manager.create_workspace("RBAC Test", owner_id=1)
    manager.add_member(workspace.id, 7, "viewer")
    manager.set_member_permission(workspace.id, 7, "tables.write", "allow")
    assert manager.member_permissions(workspace.id, 7) == {"tables.write": "allow"}
    manager.set_member_permission(workspace.id, 7, "tables.write", "deny")
    assert manager.member_permissions(workspace.id, 7) == {"tables.write": "deny"}
    manager.set_member_permission(workspace.id, 7, "tables.write", None)
    assert manager.member_permissions(workspace.id, 7) == {}
    manager.set_member_permission(workspace.id, 7, "tables.write", "allow")
    manager.remove_member(workspace.id, 7)
    assert manager.member_permissions(workspace.id, 7) == {}


def test_governance_policy_api_lists_and_updates_members(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    workspace = manager.create_workspace("Policy Test", owner_id=1)
    manager.add_member(workspace.id, 7, "viewer")
    engine = create_engine("sqlite:///:memory:")
    Base.metadata.create_all(engine, tables=[UserModel.__table__])
    db = sessionmaker(bind=engine)()
    db.add_all(
        [
            UserModel(id=1, username="owner", hashed_password="x"),
            UserModel(id=7, username="analyst", hashed_password="x"),
        ]
    )
    db.commit()
    ctx = WorkspaceCtx(user=type("Owner", (), {"id": 1})(), workspace_id=workspace.id, slug=workspace.slug)
    result = get_rbac(db=db, ctx=ctx)
    assert {member["username"] for member in result["members"]} == {"owner", "analyst"}
    updated = update_rbac(7, "signals.write", PermissionUpdate(effect="allow"), ctx=ctx)
    assert updated["effective"] is True and manager.member_permissions(workspace.id, 7)["signals.write"] == "allow"
    with pytest.raises(HTTPException) as error:
        update_rbac(1, "signals.write", PermissionUpdate(effect="deny"), ctx=ctx)
    assert error.value.status_code == 409
    db.close()


def test_workspace_member_lifecycle_preserves_overrides(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    workspace = manager.create_workspace("Lifecycle Test", owner_id=1)
    engine = create_engine("sqlite:///:memory:")
    Base.metadata.create_all(engine, tables=[UserModel.__table__])
    db = sessionmaker(bind=engine)()
    db.add(UserModel(id=7, username="analyst", hashed_password="x"))
    db.commit()
    ctx = WorkspaceCtx(
        user=type("Owner", (), {"id": 1})(),
        workspace_id=workspace.id,
        slug=workspace.slug,
    )

    created = add_workspace_member(MemberCreate(username="analyst", role="viewer"), db, ctx)
    assert created["role"] == "viewer"
    manager.set_member_permission(workspace.id, 7, "signals.write", "allow")
    updated = update_workspace_member(7, MemberRoleUpdate(role="editor"), ctx)
    assert updated["role"] == "editor"
    assert updated["overrides"] == {"signals.write": "allow"}

    with pytest.raises(HTTPException) as duplicate:
        add_workspace_member(MemberCreate(username="analyst"), db, ctx)
    assert duplicate.value.status_code == 409
    with pytest.raises(HTTPException) as owner_change:
        update_workspace_member(1, MemberRoleUpdate(role="viewer"), ctx)
    assert owner_change.value.status_code == 409
    with pytest.raises(HTTPException) as owner_remove:
        remove_workspace_member(1, ctx)
    assert owner_remove.value.status_code == 409

    assert remove_workspace_member(7, ctx) is None
    assert not manager.is_member(workspace.id, 7)
    assert manager.member_permissions(workspace.id, 7) == {}
    db.close()
