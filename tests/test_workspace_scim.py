from pathlib import Path

import pytest
from fastapi import HTTPException
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.database import Base
from apps.api.models import User
from apps.api.routers.scim import PatchInput, ScimUserInput, create_user, delete_user, list_users, patch_user
from apps.api.services.workspace import manager, scim


def _db():
    engine = create_engine("sqlite:///:memory:")
    Base.metadata.create_all(engine, tables=[User.__table__])
    return sessionmaker(bind=engine)()


def test_scim_tokens_are_hashed_rotatable_and_workspace_scoped(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    first = manager.create_workspace("SCIM One", owner_id=1)
    second = manager.create_workspace("SCIM Two", owner_id=1)
    token = scim.rotate_token(first.id, 1)
    assert scim.authenticate(first.id, token)
    assert not scim.authenticate(second.id, token)
    conn = manager._get_db()
    stored = conn.execute("SELECT token_hash FROM workspace_scim_tokens WHERE workspace_id=?", (first.id,)).fetchone()["token_hash"]
    conn.close()
    assert token not in stored
    replacement = scim.rotate_token(first.id, 1)
    assert replacement != token and not scim.authenticate(first.id, token)
    assert scim.authenticate(first.id, replacement)
    scim.revoke_token(first.id)
    assert not scim.authenticate(first.id, replacement)


def test_scim_user_lifecycle_and_filter(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    workspace = manager.create_workspace("SCIM Lifecycle", owner_id=1)
    token = scim.rotate_token(workspace.id, 1)
    authorization = f"Bearer {token}"
    db = _db()
    db.add(User(id=1, username="owner", hashed_password="x")); db.commit()

    created = create_user(workspace.slug, ScimUserInput(userName="Person@Example.com", externalId="idp-7", displayName="Person Example"), authorization, db)
    user_id = int(created["id"])
    assert created["userName"] == "person@example.com"
    assert created["active"] is True and manager.is_member(workspace.id, user_id)
    page = list_users(workspace.slug, authorization, 'userName eq "PERSON@example.com"', 1, 100, db)
    assert page["totalResults"] == 1 and page["Resources"][0]["externalId"] == "idp-7"

    disabled = patch_user(workspace.slug, user_id, PatchInput(schemas=["urn:ietf:params:scim:api:messages:2.0:PatchOp"], Operations=[{"op": "replace", "path": "active", "value": False}]), authorization, db)
    assert disabled["active"] is False and not manager.is_member(workspace.id, user_id)
    enabled = patch_user(workspace.slug, user_id, PatchInput(schemas=["urn:ietf:params:scim:api:messages:2.0:PatchOp"], Operations=[{"op": "replace", "value": {"active": True, "displayName": "Updated"}}]), authorization, db)
    assert enabled["active"] is True and enabled["displayName"] == "Updated"
    assert manager.member_role(workspace.id, user_id) == "viewer"

    response = delete_user(workspace.slug, user_id, authorization)
    assert response.status_code == 204 and not manager.is_member(workspace.id, user_id)
    assert db.query(User).filter(User.id == user_id).one().is_active is True
    db.close()


def test_scim_rejects_wrong_token_and_username_mutation(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    workspace = manager.create_workspace("SCIM Guard", owner_id=1)
    token = scim.rotate_token(workspace.id, 1)
    db = _db(); db.add(User(id=1, username="owner", hashed_password="x")); db.commit()
    with pytest.raises(HTTPException) as denied:
        list_users(workspace.slug, "Bearer wrong", None, 1, 100, db)
    assert denied.value.status_code == 401
    created = create_user(workspace.slug, ScimUserInput(userName="person@example.com"), f"Bearer {token}", db)
    second = create_user(workspace.slug, ScimUserInput(userName="second@example.com"), f"Bearer {token}", db)
    assert second["externalId"] == ""
    with pytest.raises(HTTPException) as rename:
        patch_user(workspace.slug, int(created["id"]), PatchInput(schemas=["urn:ietf:params:scim:api:messages:2.0:PatchOp"], Operations=[{"op": "replace", "path": "userName", "value": "attacker@example.com"}]), f"Bearer {token}", db)
    assert rename.value.status_code == 409
    db.close()
