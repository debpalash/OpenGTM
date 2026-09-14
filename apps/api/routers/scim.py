"""Interoperable SCIM 2.0 Users and Groups service, isolated per workspace."""

import re
from datetime import datetime, timezone

from fastapi import APIRouter, Depends, Header, HTTPException, Query, Response
from fastapi.responses import JSONResponse
from pydantic import BaseModel, ConfigDict, Field
from sqlalchemy import func
from sqlalchemy.orm import Session

from apps.api.auth import get_password_hash
from apps.api.database import get_db
from apps.api.models import User
from apps.api.services.workspace import manager, scim

class ScimJSONResponse(JSONResponse):
    media_type = "application/scim+json"


router = APIRouter(prefix="/scim/v2/{workspace_slug}", tags=["scim"], default_response_class=ScimJSONResponse)
USER_SCHEMA = "urn:ietf:params:scim:schemas:core:2.0:User"
LIST_SCHEMA = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
PATCH_SCHEMA = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
GROUP_SCHEMA = "urn:ietf:params:scim:schemas:core:2.0:Group"
SCHEMA_SCHEMA = "urn:ietf:params:scim:schemas:core:2.0:Schema"
RESOURCE_TYPE_SCHEMA = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"


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


class ScimGroupInput(BaseModel):
    model_config = ConfigDict(extra="allow")
    schemas: list[str] = Field(default_factory=lambda: [GROUP_SCHEMA])
    displayName: str = Field(min_length=1, max_length=150)
    externalId: str = ""
    members: list[dict] = Field(default_factory=list)


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


def _group_resource(workspace, group: dict) -> dict:
    return {
        "schemas": [GROUP_SCHEMA], "id": group["id"], "externalId": group["external_id"] or "", "displayName": group["display_name"],
        "members": [{"value": value, "$ref": f"/scim/v2/{workspace.slug}/Users/{value}"} for value in group["members"]],
        "meta": {"resourceType": "Group", "created": datetime.fromtimestamp(group["created_at"], timezone.utc).isoformat(), "lastModified": datetime.fromtimestamp(group["updated_at"], timezone.utc).isoformat(), "location": f"/scim/v2/{workspace.slug}/Groups/{group['id']}"},
    }


def _member_ids(members: list[dict]) -> list[int]:
    try: return [int(item["value"]) for item in members]
    except (KeyError, TypeError, ValueError) as exc: raise HTTPException(status_code=400, detail="Each group member requires a numeric value") from exc


def _schema_resources() -> list[dict]:
    common = {"multiValued": False, "required": False, "caseExact": False, "mutability": "readWrite", "returned": "default", "uniqueness": "none"}
    return [
        {"schemas": [SCHEMA_SCHEMA], "id": USER_SCHEMA, "name": "User", "description": "OpenGTM workspace user", "attributes": [
            {**common, "name": "userName", "type": "string", "required": True, "uniqueness": "server"},
            {**common, "name": "displayName", "type": "string"}, {**common, "name": "externalId", "type": "string"}, {**common, "name": "active", "type": "boolean"},
        ]},
        {"schemas": [SCHEMA_SCHEMA], "id": GROUP_SCHEMA, "name": "Group", "description": "OpenGTM workspace group", "attributes": [
            {**common, "name": "displayName", "type": "string", "required": True, "uniqueness": "server"}, {**common, "name": "externalId", "type": "string"},
            {**common, "name": "members", "type": "complex", "multiValued": True, "subAttributes": [{**common, "name": "value", "type": "string", "mutability": "immutable"}, {**common, "name": "$ref", "type": "reference", "referenceTypes": ["User"], "mutability": "immutable"}]},
        ]},
    ]


def _resource_type(workspace, kind: str, schema: str) -> dict:
    return {"schemas": [RESOURCE_TYPE_SCHEMA], "id": kind, "name": kind, "endpoint": f"/{kind}s", "schema": schema, "schemaExtensions": [], "meta": {"resourceType": "ResourceType", "location": f"/scim/v2/{workspace.slug}/ResourceTypes/{kind}"}}


@router.get("/ServiceProviderConfig")
def service_provider_config(workspace_slug: str, authorization: str | None = Header(default=None)):
    _workspace(workspace_slug, authorization)
    return {"schemas": ["urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"], "patch": {"supported": True}, "bulk": {"supported": False}, "filter": {"supported": True, "maxResults": 200}, "changePassword": {"supported": False}, "sort": {"supported": False}, "etag": {"supported": False}, "authenticationSchemes": [{"type": "oauthbearertoken", "name": "Bearer Token", "description": "Workspace-scoped SCIM token", "specUri": "https://www.rfc-editor.org/info/rfc6750", "primary": True}]}


@router.get("/ResourceTypes")
def resource_types(workspace_slug: str, authorization: str | None = Header(default=None)):
    workspace = _workspace(workspace_slug, authorization)
    resources = [_resource_type(workspace, kind, schema) for kind, schema in (("User", USER_SCHEMA), ("Group", GROUP_SCHEMA))]
    return {"schemas": [LIST_SCHEMA], "totalResults": 2, "startIndex": 1, "itemsPerPage": 2, "Resources": resources}


@router.get("/ResourceTypes/{resource_type}")
def get_resource_type(workspace_slug: str, resource_type: str, authorization: str | None = Header(default=None)):
    workspace = _workspace(workspace_slug, authorization)
    schemas_by_type = {"User": USER_SCHEMA, "Group": GROUP_SCHEMA}
    if resource_type not in schemas_by_type: raise HTTPException(status_code=404, detail="Resource type not found")
    return _resource_type(workspace, resource_type, schemas_by_type[resource_type])


@router.get("/Schemas")
def schemas(workspace_slug: str, authorization: str | None = Header(default=None)):
    _workspace(workspace_slug, authorization)
    resources = _schema_resources()
    return {"schemas": [LIST_SCHEMA], "totalResults": 2, "startIndex": 1, "itemsPerPage": 2, "Resources": resources}


@router.get("/Schemas/{schema_id:path}")
def get_schema(workspace_slug: str, schema_id: str, authorization: str | None = Header(default=None)):
    _workspace(workspace_slug, authorization)
    for resource in _schema_resources():
        if resource["id"] == schema_id: return resource
    raise HTTPException(status_code=404, detail="Schema not found")


@router.get("/Users")
def list_users(workspace_slug: str, authorization: str | None = Header(default=None), filter: str | None = None, startIndex: int = Query(1, ge=1), count: int = Query(100, ge=0, le=200), db: Session = Depends(get_db)):
    workspace = _workspace(workspace_slug, authorization)
    match = re.fullmatch(r'\s*userName\s+eq\s+"([^"]+)"\s*', filter or "", re.IGNORECASE) if filter else None
    if filter and not match:
        raise HTTPException(status_code=400, detail="Only userName eq filtering is supported")
    offset = startIndex - 1
    if match:
        user = db.query(User).filter(
            func.lower(User.username) == match.group(1).strip().lower(),
        ).first()
        mapping = scim.get_mapping(workspace.id, user.id) if user else None
        total = 1 if mapping else 0
        mappings = [mapping] if mapping and offset == 0 and count else []
    else:
        mappings, total = scim.list_mappings_page(
            workspace.id, offset=offset, limit=count,
        )
    users = {u.id: u for u in db.query(User).filter(User.id.in_([m["user_id"] for m in mappings])).all()} if mappings else {}
    resources = [_resource(workspace, users[m["user_id"]], m) for m in mappings if m["user_id"] in users]
    return {"schemas": [LIST_SCHEMA], "totalResults": total, "startIndex": startIndex, "itemsPerPage": len(resources), "Resources": resources}


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


@router.get("/Groups")
def list_groups(workspace_slug: str, authorization: str | None = Header(default=None), filter: str | None = None, startIndex: int = Query(1, ge=1), count: int = Query(100, ge=0, le=200)):
    workspace = _workspace(workspace_slug, authorization)
    match = re.fullmatch(r'\s*displayName\s+eq\s+"([^"]+)"\s*', filter or "", re.IGNORECASE) if filter else None
    if filter and not match: raise HTTPException(status_code=400, detail="Only displayName eq filtering is supported")
    groups, total = scim.list_groups_page(
        workspace.id, offset=startIndex - 1, limit=count,
        display_name=match.group(1) if match else None,
    )
    resources = [_group_resource(workspace, group) for group in groups]
    return {"schemas": [LIST_SCHEMA], "totalResults": total, "startIndex": startIndex, "itemsPerPage": len(resources), "Resources": resources}


@router.post("/Groups", status_code=201)
def create_group(workspace_slug: str, body: ScimGroupInput, authorization: str | None = Header(default=None)):
    workspace = _workspace(workspace_slug, authorization)
    try:
        member_ids = _member_ids(body.members)
        group = scim.create_group(
            workspace.id, body.displayName.strip(), body.externalId, member_ids,
        )
    except ValueError as exc: raise HTTPException(status_code=400, detail=str(exc)) from exc
    except Exception as exc: raise HTTPException(status_code=409, detail="SCIM group already exists") from exc
    return _group_resource(workspace, scim.get_group(workspace.id, group["id"]))


@router.get("/Groups/{group_id}")
def get_group(workspace_slug: str, group_id: str, authorization: str | None = Header(default=None)):
    workspace = _workspace(workspace_slug, authorization); group = scim.get_group(workspace.id, group_id)
    if not group: raise HTTPException(status_code=404, detail="SCIM group not found")
    return _group_resource(workspace, group)


@router.put("/Groups/{group_id}")
def replace_group(workspace_slug: str, group_id: str, body: ScimGroupInput, authorization: str | None = Header(default=None)):
    workspace = _workspace(workspace_slug, authorization); group = scim.get_group(workspace.id, group_id)
    if not group: raise HTTPException(status_code=404, detail="SCIM group not found")
    try:
        saved = scim.replace_group(
            workspace.id, group_id, body.displayName.strip(), body.externalId,
            _member_ids(body.members),
        )
    except ValueError as exc: raise HTTPException(status_code=400, detail=str(exc)) from exc
    if saved is None: raise HTTPException(status_code=404, detail="SCIM group not found")
    return _group_resource(workspace, saved)


@router.patch("/Groups/{group_id}")
def patch_group(workspace_slug: str, group_id: str, body: PatchInput, authorization: str | None = Header(default=None)):
    workspace = _workspace(workspace_slug, authorization); group = scim.get_group(workspace.id, group_id)
    if not group: raise HTTPException(status_code=404, detail="SCIM group not found")
    if PATCH_SCHEMA not in body.schemas: raise HTTPException(status_code=400, detail="PatchOp schema is required")
    display_name = group["display_name"]
    external_id = group["external_id"]
    member_ids = [int(value) for value in group["members"]]
    for operation in body.Operations:
        op = str(operation.get("op", "")).lower(); path = operation.get("path"); value = operation.get("value")
        if op in {"add", "replace"} and path == "members":
            requested = _member_ids(value or [])
            member_ids = requested if op == "replace" else list(dict.fromkeys([*member_ids, *requested]))
        elif op == "remove" and isinstance(path, str):
            match = re.fullmatch(r'members\[value\s+eq\s+"([0-9]+)"\]', path, re.IGNORECASE)
            if not match: raise HTTPException(status_code=400, detail="Unsupported group remove path")
            member_ids = [item for item in member_ids if item != int(match.group(1))]
        elif op in {"add", "replace"} and path == "displayName":
            display_name = str(value)
        elif op in {"add", "replace"} and path == "externalId":
            external_id = str(value)
        else: raise HTTPException(status_code=400, detail=f"Unsupported group patch operation: {op} {path}")
    try:
        saved = scim.replace_group(
            workspace.id, group_id, display_name, external_id, member_ids,
        )
    except ValueError as exc: raise HTTPException(status_code=400, detail=str(exc)) from exc
    if saved is None: raise HTTPException(status_code=404, detail="SCIM group not found")
    return _group_resource(workspace, saved)


@router.delete("/Groups/{group_id}", status_code=204)
def delete_group(workspace_slug: str, group_id: str, authorization: str | None = Header(default=None)):
    workspace = _workspace(workspace_slug, authorization)
    if not scim.delete_group(workspace.id, group_id): raise HTTPException(status_code=404, detail="SCIM group not found")
    return Response(status_code=204)
