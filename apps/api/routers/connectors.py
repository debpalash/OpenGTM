"""Discoverable catalog for the versioned community connector SDK."""

from fastapi import APIRouter, Depends

from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.services.leadgen.enrichment.declarative.manifest import load_all_manifests, validate_manifest_directory
from apps.api.services.workspace.secrets import get_secret

router = APIRouter(prefix="/api/connectors", tags=["connectors"])


@router.get("/catalog")
def connector_catalog(ctx: WorkspaceCtx = Depends(current_workspace)):
    connectors = []
    for manifest in load_all_manifests():
        item = manifest.catalog_entry()
        credential_key = item.pop("credential_key", None)
        item["configured"] = manifest.auth.type == "none" or bool(credential_key and get_secret(ctx.workspace_id, credential_key))
        item["auth_type"] = manifest.auth.type
        connectors.append(item)
    return {"manifest_version": "1", "total": len(connectors), "connectors": connectors}


@router.get("/compatibility")
def connector_compatibility(ctx: WorkspaceCtx = Depends(current_workspace)):
    # Dependency is intentional: validation output is available only to an
    # authenticated workspace, even though it contains no credential values.
    _ = ctx
    report = validate_manifest_directory()
    for connector in report["connectors"]:
        connector.pop("credential_key", None)
    return report
