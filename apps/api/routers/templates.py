"""
Templates Router — Workbook template gallery.

Two generations live here:
  - Legacy templates (GET "" / POST "/{template_id}/create"): lead-field views
    from services/workbook/templates.py.
  - Recipe gallery (GET "/gallery" / GET "/gallery/{slug}" /
    POST "/gallery/{slug}/instantiate"): curated, importable workbook recipes
    loaded from services/templates/recipes/*.yaml — full column configs
    (source / waterfall / ai_formula / research / output), validated at load
    time. Instantiation reuses the workbooks router's create_workbook path.
"""

from typing import Optional

from fastapi import APIRouter, HTTPException, Depends
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session
from apps.api.database import get_db
from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role

router = APIRouter(prefix="/api/templates", tags=["templates"])
require_editor = require_workspace_role("editor", "admin")


# ── Recipe gallery ────────────────────────────────────────────────────────

class InstantiateRecipeRequest(BaseModel):
    """Optional overrides when creating a workbook from a recipe."""
    name: Optional[str] = Field(None, min_length=1, max_length=255)


@router.get("/gallery")
def list_gallery(category: str = None):
    """List gallery recipes (card summaries), optionally filtered by category."""
    from apps.api.services.templates.recipes import (
        RECIPE_CATEGORIES, load_recipes, recipe_summary,
    )
    if category and category not in RECIPE_CATEGORIES:
        raise HTTPException(status_code=404, detail=f"Unknown category '{category}'")
    recipes = load_recipes(category)
    return {
        "recipes": [recipe_summary(r) for r in recipes],
        "categories": RECIPE_CATEGORIES,
        "total": len(recipes),
    }


@router.get("/gallery/{slug}")
def get_gallery_recipe(slug: str):
    """Full recipe detail — metadata + the complete workbook definition."""
    from apps.api.services.templates.recipes import get_recipe
    recipe = get_recipe(slug)
    if not recipe:
        raise HTTPException(status_code=404, detail="Recipe not found")
    return recipe


@router.post("/gallery/{slug}/instantiate", status_code=201)
async def instantiate_gallery_recipe(
    slug: str,
    body: InstantiateRecipeRequest = None,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Create a workbook from a recipe in the caller's workspace.

    Delegates to the workbooks router's create_workbook (the single
    workbook-creation path) so row snapshotting, automations events and the
    response shape stay identical to a hand-built workbook.
    """
    from apps.api.routers.workbooks import create_workbook
    from apps.api.services.workbook.schemas import ColumnConfig, WorkbookCreate
    from apps.api.services.templates.recipes import get_recipe

    recipe = get_recipe(slug)
    if not recipe:
        raise HTTPException(status_code=404, detail="Recipe not found")

    wb_def = recipe["workbook"]
    create_body = WorkbookCreate(
        name=(body.name if body and body.name else recipe["name"]),
        description=wb_def.get("description") or recipe["description"],
        source="empty",
        columns_config=[ColumnConfig.model_validate(c) for c in wb_def["columns"]],
    )
    return await create_workbook(create_body, db=db, ctx=ctx)


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
    ctx: WorkspaceCtx = Depends(require_editor),
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
