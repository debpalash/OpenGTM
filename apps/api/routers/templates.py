"""
Templates Router — Workbook template gallery.
"""

from fastapi import APIRouter, HTTPException, Depends
from sqlalchemy.orm import Session
from apps.api.database import get_db
from apps.api.core.tenancy import WorkspaceCtx, current_workspace

router = APIRouter(prefix="/api/templates", tags=["templates"])


@router.get("")
def list_templates(category: str = None):
    """List workbook templates, optionally filtered by category."""
    from apps.api.services.workbook.templates import get_templates, TEMPLATE_CATEGORIES
    templates = get_templates(category)
    return {
        "templates": templates,
        "categories": TEMPLATE_CATEGORIES,
        "total": len(templates),
    }


@router.post("/{template_id}/create")
def create_from_template(
    template_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Create a new workbook from a template (scoped to the caller's workspace)."""
    from apps.api.services.workbook.templates import get_template
    from apps.api.services.workbook.models import Workbook
    import json

    template = get_template(template_id)
    if not template:
        raise HTTPException(status_code=404, detail="Template not found")

    # Transform template columns into the workbook editor's expected format
    # Template columns: {key, name, type} → Editor columns: {id, name, type: "lead_field", lead_field, width}
    editor_columns = []
    seen_ids = set()
    for col in template["columns"]:
        editor_columns.append({
            "id": col["key"],
            "name": col["name"],
            "type": "lead_field",
            "lead_field": col["key"],
            "width": 180,
        })
        seen_ids.add(col["key"])

    # Materialize REAL enrichment columns (waterfall / enrichment / ai_formula /
    # research / ...). These are stored verbatim — they already match the
    # ColumnConfig shape the enrichment engine consumes — so the workbook is
    # immediately runnable. Backward compatible: templates without
    # `enrichment_columns` simply add nothing here.
    enrichment_count = 0
    for ecol in template.get("enrichment_columns", []):
        # Copy so we never mutate the shared template definition.
        ecol = dict(ecol)
        # Guarantee a unique, non-colliding column id within this workbook.
        base_id = ecol.get("id") or f"enrich_{enrichment_count}"
        col_id = base_id
        suffix = 1
        while col_id in seen_ids:
            col_id = f"{base_id}_{suffix}"
            suffix += 1
        ecol["id"] = col_id
        seen_ids.add(col_id)
        editor_columns.append(ecol)
        enrichment_count += 1

    workbook = Workbook(
        name=template["name"],
        description=template["description"],
        columns_config=editor_columns,
        filter_criteria=template.get("filter", {}),
        workspace_id=ctx.workspace_id,
    )
    db.add(workbook)
    db.commit()
    db.refresh(workbook)

    return {
        "id": workbook.id,
        "name": workbook.name,
        "template": template_id,
        "columns_count": len(editor_columns),
        "enrichment_columns_count": enrichment_count,
    }
