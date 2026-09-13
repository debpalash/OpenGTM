"""Minimal interoperable SCIM 2.0 Users service, isolated per workspace."""

import re
from datetime import datetime, timezone

from fastapi import APIRouter, Depends, Header, HTTPException, Query, Response
from pydantic import BaseModel, ConfigDict, Field
from sqlalchemy.orm import Session

from apps.api.auth import get_password_hash
from apps.api.database import get_db
from apps.api.models import User
from apps.api.services.workspace import manager, scim

router = APIRouter(prefix="/scim/v2/{workspace_slug}", tags=["scim"])
USER_SCHEMA = "urn:ietf:params:scim:schemas:core:2.0:User"
LIST_SCHEMA = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
PATCH_SCHEMA = "urn:ietf:params:scim:api:messages:2.0:PatchOp"


class ScimUserInput(BaseModel):
    model_config = ConfigDict(extra="allow")
    schemas: list[str] = Field(default_factory=lambda: [USER_SCHEMA])
    userName: str = Field(min_length=1, max_length=150)
    externalId: str = ""
    displayName: str = ""
    active: bool = True


class PatchInput(BaseModel):
    schemas: list[str]
    Operations: list[dict]


def _workspace(workspace_slug: str, authorization: str | None):
    workspace = manager.get_workspace_by_slug(workspace_slug)
    if workspace is None:
        raise HTTPException(status_code=404, detail="Workspace not found")
    scheme, _, token = (authorization or "").partition(" ")
    if scheme.lower() != "bearer" or not scim.authenticate(workspace.id, token):
        raise HTTPException(status_code=401, detail="Invalid SCIM bearer token", headers={"WWW-Authenticate": "Bearer"})
    return workspace


def _resource(workspace, user: User, mapping: dict) -> dict:
    created = datetime.fromtimestamp(mapping["created_at"], timezone.utc).isoformat()
    updated = datetime.fromtimestamp(mapping["updated_at"], timezone.utc).isoformat()
    return {
        "schemas": [USER_SCHEMA], "id": str(user.id), "externalId": mapping["external_id"] or "",
        "userName": user.username, "displayName": mapping["display_name"] or user.username,
        "active": bool(mapping["active"]),
        "meta": {"resourceType": "User", "created": created, "lastModified": updated, "location": f"/scim/v2/{workspace.slug}/Users/{user.id}"},
    }


@router.get("/ServiceProviderConfig")
def service_provider_config(workspace_slug: str, authorization: str | None = Header(default=None)):
    _workspace(workspace_slug, authorization)
    return {"schemas": ["urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"], "patch": {"supported": True}, "bulk": {"supported": False}, "filter": {"supported": True, "maxResults": 200}, "changePassword": {"supported": False}, "sort": {"supported": False}, "etag": {"supported": False}, "authenticationSchemes": [{"type": "oauthbearertoken", "name": "Bearer Token", "description": "Workspace-scoped SCIM token", "specUri": "https://www.rfc-editor.org/info/rfc6750", "primary": True}]}


@router.get("/Users")
def list_users(workspace_slug: str, authorization: str | None = Header(default=None), filter: str | None = None, startIndex: int = Query(1, ge=1), count: int = Query(100, ge=0, le=200), db: Session = Depends(get_db)):
    workspace = _workspace(workspace_slug, authorization)
    mappings = scim.list_mappings(workspace.id)
    match = re.fullmatch(r'\s*userName\s+eq\s+"([^"]+)"\s*', filter or "", re.IGNORECASE) if filter else None
    if filter and not match:
        raise HTTPException(status_code=400, detail="Only userName eq filtering is supported")
    users = {u.id: u for u in db.query(User).filter(User.id.in_([m["user_id"] for m in mappings])).all()} if mappings else {}
    resources = [_resource(workspace, users[m["user_id"]], m) for m in mappings if m["user_id"] in users]
    if match:
        resources = [item for item in resources if item["userName"].casefold() == match.group(1).casefold()]
    total = len(resources); page = resources[startIndex - 1:startIndex - 1 + count]
    return {"schemas": [LIST_SCHEMA], "totalResults": total, "startIndex": startIndex, "itemsPerPage": len(page), "Resources": page}


@router.post("/Users", status_code=201)
def create_user(workspace_slug: str, body: ScimUserInput, authorization: str | None = Header(default=None), db: Session = Depends(get_db)):
    workspace = _workspace(workspace_slug, authorization)
    username = body.userName.strip().lower()
    user = db.query(User).filter(User.username == username).first()
    if user is None:
        user = User(username=username, hashed_password=get_password_hash(scim.rotate_password()), role="user", is_active=True)
        db.add(user); db.commit(); db.refresh(user)
    if scim.get_mapping(workspace.id, user.id):
        raise HTTPException(status_code=409, detail="SCIM userName already exists")
    if body.active and not manager.is_member(workspace.id, user.id):
        manager.add_member(workspace.id, user.id, "viewer")
    try:
        mapping = scim.upsert_mapping(workspace.id, user.id, body.externalId, body.displayName, body.active)
    except ValueError as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    return _resource(workspace, user, mapping)


@router.get("/Users/{user_id}")
def get_user(workspace_slug: str, user_id: int, authorization: str | None = Header(default=None), db: Session = Depends(get_db)):
    workspace = _workspace(workspace_slug, authorization); mapping = scim.get_mapping(workspace.id, user_id)
    user = db.query(User).filter(User.id == user_id).first()
    if not mapping or not user: raise HTTPException(status_code=404, detail="SCIM user not found")
    return _resource(workspace, user, mapping)


def _apply(workspace, user: User, mapping: dict, values: dict) -> dict:
    if "userName" in values and str(values["userName"]).strip().casefold() != user.username.casefold():
        raise HTTPException(status_code=409, detail="userName cannot be changed after provisioning")
    active = bool(values.get("active", mapping["active"]))
    if active and not manager.is_member(workspace.id, user.id): manager.add_member(workspace.id, user.id, "viewer")
    if not active and manager.is_member(workspace.id, user.id): manager.remove_member(workspace.id, user.id)
    try:
        return scim.upsert_mapping(workspace.id, user.id, str(values.get("externalId", mapping["external_id"])), str(values.get("displayName", mapping["display_name"])), active)
    except ValueError as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc


@router.put("/Users/{user_id}")
def replace_user(workspace_slug: str, user_id: int, body: ScimUserInput, authorization: str | None = Header(default=None), db: Session = Depends(get_db)):
    workspace = _workspace(workspace_slug, authorization); mapping = scim.get_mapping(workspace.id, user_id); user = db.query(User).filter(User.id == user_id).first()
    if not mapping or not user: raise HTTPException(status_code=404, detail="SCIM user not found")
    return _resource(workspace, user, _apply(workspace, user, mapping, body.model_dump()))


@router.patch("/Users/{user_id}")
def patch_user(workspace_slug: str, user_id: int, body: PatchInput, authorization: str | None = Header(default=None), db: Session = Depends(get_db)):
    workspace = _workspace(workspace_slug, authorization); mapping = scim.get_mapping(workspace.id, user_id); user = db.query(User).filter(User.id == user_id).first()
    if not mapping or not user: raise HTTPException(status_code=404, detail="SCIM user not found")
    if PATCH_SCHEMA not in body.schemas: raise HTTPException(status_code=400, detail="PatchOp schema is required")
    values = {}
    for operation in body.Operations:
        if str(operation.get("op", "")).lower() not in {"add", "replace"}: raise HTTPException(status_code=400, detail="Only add and replace patch operations are supported")
        path = operation.get("path"); value = operation.get("value")
        if path in {"active", "displayName", "externalId", "userName"}: values[path] = value
        elif not path and isinstance(value, dict): values.update({key: val for key, val in value.items() if key in {"active", "displayName", "externalId", "userName"}})
        else: raise HTTPException(status_code=400, detail=f"Unsupported patch path: {path}")
    return _resource(workspace, user, _apply(workspace, user, mapping, values))


@router.delete("/Users/{user_id}", status_code=204)
def delete_user(workspace_slug: str, user_id: int, authorization: str | None = Header(default=None)):
    workspace = _workspace(workspace_slug, authorization); mapping = scim.get_mapping(workspace.id, user_id)
    if not mapping: raise HTTPException(status_code=404, detail="SCIM user not found")
    if manager.is_member(workspace.id, user_id): manager.remove_member(workspace.id, user_id)
    scim.upsert_mapping(workspace.id, user_id, mapping["external_id"], mapping["display_name"], False)
    return Response(status_code=204)
