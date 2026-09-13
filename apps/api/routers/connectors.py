"""Discoverable catalog for the versioned community connector SDK."""

from fastapi import APIRouter, Depends

from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.services.leadgen.enrichment.declarative.manifest import load_all_manifests, validate_manifest_directory
from apps.api.services.workspace.secrets import get_secret

router = APIRouter(prefix="/api/connectors", tags=["connectors"])


@router.get("/catalog")
def connector_catalog(ctx: WorkspaceCtx = Depends(current_workspace)):
    review = validate_manifest_directory()
    signatures = {item["id"]: item.get("signature", {"status": "unsigned"}) for item in review["connectors"]}
    manifests = load_all_manifests()
    from apps.api.services.integrations.certification import certification_statuses

    maturity = certification_statuses([
        f"connector:{manifest.name}" for manifest in manifests
    ])
    connectors = []
    for manifest in manifests:
        item = manifest.catalog_entry()
        credential_key = item.pop("credential_key", None)
        item["configured"] = manifest.auth.type == "none" or bool(credential_key and get_secret(ctx.workspace_id, credential_key))
        item["auth_type"] = manifest.auth.type
        item["signature"] = signatures.get(manifest.name, {"status": "unknown"})
        item.update(maturity[f"connector:{manifest.name}"])
        connectors.append(item)
    return {"manifest_version": "1", "signature_policy": review["signature_policy"], "total": len(connectors), "connectors": connectors}


@router.get("/compatibility")
def connector_compatibility(ctx: WorkspaceCtx = Depends(current_workspace)):
    # Dependency is intentional: validation output is available only to an
    # authenticated workspace, even though it contains no credential values.
    _ = ctx
    report = validate_manifest_directory()
    for connector in report["connectors"]:
        connector.pop("credential_key", None)
    return report
