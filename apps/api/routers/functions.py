"""
Functions Router — Reusable workbook column chains.
"""

from fastapi import APIRouter, HTTPException, Depends
from pydantic import BaseModel
from typing import List, Optional
from sqlalchemy.orm import Session
from apps.api.database import get_db
from apps.api.core.security import get_current_active_user, get_current_admin_user
from apps.api.core.tenancy import WorkspaceCtx, require_workspace_role
from apps.api.services.workbook.models import Workbook

router = APIRouter(
    prefix="/api/functions",
    tags=["functions"],
    dependencies=[Depends(get_current_active_user)],
)
require_editor = require_workspace_role("editor", "admin")


class CreateFunctionRequest(BaseModel):
    name: str
    description: str = ""
    category: str = "custom"
    columns_chain: List[dict]


class ApplyFunctionRequest(BaseModel):
    workbook_id: str


@router.get("")
def list_functions(category: str = None):
    """List all reusable functions."""
    from apps.api.services.workbook.functions import list_functions as _list
    functions = _list(category)
    return {"functions": functions, "total": len(functions)}


@router.post("")
def create_function(
    req: CreateFunctionRequest, _admin=Depends(get_current_admin_user)
):
    """Create a new function from column definitions."""
    from apps.api.services.workbook.functions import create_function as _create
    result = _create(
        name=req.name,
        description=req.description,
        columns_chain=req.columns_chain,
        category=req.category,
    )
    return result


@router.get("/{func_id}")
def get_function(func_id: str):
    """Get function details."""
    from apps.api.services.workbook.functions import get_function as _get
    func = _get(func_id)
    if not func:
        raise HTTPException(status_code=404, detail="Function not found")
    return func


@router.post("/{func_id}/apply")
def apply_function(
    func_id: str,
    req: ApplyFunctionRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Apply a function's column chain to a workbook."""
    workbook = db.query(Workbook).filter(
        Workbook.id == req.workbook_id,
        Workbook.workspace_id == ctx.workspace_id,
    ).first()
    if workbook is None:
        raise HTTPException(status_code=404, detail="Workbook not found")
    from apps.api.services.workbook.functions import apply_function as _apply
    result = _apply(func_id, req.workbook_id, db)
    if "error" in result:
        raise HTTPException(status_code=404, detail=result["error"])
    return result


@router.delete("/{func_id}")
def delete_function(func_id: str, _admin=Depends(get_current_admin_user)):
    """Delete a function."""
    from apps.api.services.workbook.functions import delete_function as _delete
    _delete(func_id)
    return {"status": "ok"}
