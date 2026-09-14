import sqlite3
import time
from pathlib import Path

import pytest
from fastapi import HTTPException
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.database import Base
from apps.api.models import User
from apps.api.routers.scim import (
    PatchInput,
    ScimGroupInput,
    ScimUserInput,
    create_group,
    create_user,
    delete_group,
    delete_user,
    get_resource_type,
    get_schema,
    list_groups,
    list_users,
    patch_group,
    patch_user,
    replace_group,
    resource_types,
    schemas,
)
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
    initial_status = scim.token_status(first.id)
    assert initial_status is not None
    assert initial_status["expires_at"] > initial_status["created_at"]
    assert initial_status["expired"] is False
    assert scim.authenticate(first.id, token)
    assert scim.token_status(first.id)["last_used_at"] is not None
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


def test_scim_token_expiry_fails_closed(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    monkeypatch.setenv("OPENGTM_SCIM_TOKEN_TTL_DAYS", "1")
    workspace = manager.create_workspace("SCIM Expiry", owner_id=1)
    token = scim.rotate_token(workspace.id, 1)
    conn = manager._get_db()
    conn.execute("UPDATE workspace_scim_tokens SET expires_at=? WHERE workspace_id=?", (time.time() - 1, workspace.id))
    conn.commit(); conn.close()

    assert not scim.authenticate(workspace.id, token)
    assert scim.token_status(workspace.id)["expired"] is True


def test_scim_token_schema_upgrades_existing_workspace_database(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    data_dir = tmp_path / "data"
    data_dir.mkdir()
    legacy = sqlite3.connect(data_dir / "workspaces.db")
    legacy.execute(
        "CREATE TABLE workspace_scim_tokens (workspace_id TEXT PRIMARY KEY, token_hash TEXT NOT NULL, "
        "token_prefix TEXT NOT NULL, created_at REAL NOT NULL, created_by INTEGER)"
    )
    legacy.commit(); legacy.close()
    conn = manager._get_db()
    columns = {row[1] for row in conn.execute("PRAGMA table_info(workspace_scim_tokens)").fetchall()}
    conn.close()
    assert {"expires_at", "last_used_at"} <= columns


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


def test_scim_group_lifecycle_and_membership_patch(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    workspace = manager.create_workspace("SCIM Groups", owner_id=1)
    authorization = f"Bearer {scim.rotate_token(workspace.id, 1)}"
    db = _db(); db.add(User(id=1, username="owner", hashed_password="x")); db.commit()
    first = create_user(workspace.slug, ScimUserInput(userName="first@example.com"), authorization, db)
    second = create_user(workspace.slug, ScimUserInput(userName="second@example.com"), authorization, db)
    with pytest.raises(HTTPException) as invalid_create:
        create_group(
            workspace.slug,
            ScimGroupInput(displayName="Must Not Exist", members=[{"value": "999999"}]),
            authorization,
        )
    assert invalid_create.value.status_code == 400
    assert scim.list_groups(workspace.id) == []
    group = create_group(workspace.slug, ScimGroupInput(displayName="Revenue", externalId="group-7", members=[{"value": first["id"]}]), authorization)
    assert group["displayName"] == "Revenue" and [m["value"] for m in group["members"]] == [first["id"]]
    page = list_groups(workspace.slug, authorization, 'displayName eq "revenue"', 1, 100)
    assert page["totalResults"] == 1

    with pytest.raises(HTTPException) as invalid_replace:
        replace_group(
            workspace.slug, group["id"],
            ScimGroupInput(
                displayName="Must Roll Back", externalId="changed",
                members=[{"value": "999999"}],
            ),
            authorization,
        )
    assert invalid_replace.value.status_code == 400
    preserved = scim.get_group(workspace.id, group["id"])
    assert preserved["display_name"] == "Revenue"
    assert preserved["external_id"] == "group-7"
    assert preserved["members"] == [first["id"]]

    with pytest.raises(HTTPException) as invalid_patch:
        patch_group(
            workspace.slug, group["id"],
            PatchInput(
                schemas=["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
                Operations=[
                    {"op": "replace", "path": "displayName", "value": "Must Also Roll Back"},
                    {
                        "op": "replace", "path": "members",
                        "value": [{"value": "999999"}],
                    },
                ],
            ),
            authorization,
        )
    assert invalid_patch.value.status_code == 400
    after_patch = scim.get_group(workspace.id, group["id"])
    assert after_patch["display_name"] == "Revenue"
    assert after_patch["members"] == [first["id"]]

    updated = patch_group(workspace.slug, group["id"], PatchInput(schemas=["urn:ietf:params:scim:api:messages:2.0:PatchOp"], Operations=[{"op": "add", "path": "members", "value": [{"value": second["id"]}]}]), authorization)
    assert {member["value"] for member in updated["members"]} == {first["id"], second["id"]}
    updated = patch_group(workspace.slug, group["id"], PatchInput(schemas=["urn:ietf:params:scim:api:messages:2.0:PatchOp"], Operations=[{"op": "remove", "path": f'members[value eq "{first["id"]}"]'}]), authorization)
    assert [member["value"] for member in updated["members"]] == [second["id"]]
    response = delete_group(workspace.slug, group["id"], authorization)
    assert response.status_code == 204 and list_groups(workspace.slug, authorization, None, 1, 100)["totalResults"] == 0
    assert manager.is_member(workspace.id, int(first["id"]))
    assert resource_types(workspace.slug, authorization)["totalResults"] == 2
    assert get_resource_type(workspace.slug, "Group", authorization)["endpoint"] == "/Groups"
    schema_page = schemas(workspace.slug, authorization)
    assert schema_page["Resources"][0]["attributes"]
    assert get_schema(workspace.slug, "urn:ietf:params:scim:schemas:core:2.0:Group", authorization)["name"] == "Group"
    db.close()
