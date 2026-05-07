"""
Functions Router — Reusable workbook column chains.
"""

from fastapi import APIRouter, HTTPException, Depends
from pydantic import BaseModel
from typing import List, Optional
from sqlalchemy.orm import Session
from apps.api.database import get_db

router = APIRouter(prefix="/api/functions", tags=["functions"])


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
def create_function(req: CreateFunctionRequest):
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
def apply_function(func_id: str, req: ApplyFunctionRequest, db: Session = Depends(get_db)):
    """Apply a function's column chain to a workbook."""
    from apps.api.services.workbook.functions import apply_function as _apply
    result = _apply(func_id, req.workbook_id, db)
    if "error" in result:
        raise HTTPException(status_code=404, detail=result["error"])
    return result


@router.delete("/{func_id}")
def delete_function(func_id: str):
    """Delete a function."""
    from apps.api.services.workbook.functions import delete_function as _delete
    _delete(func_id)
    return {"status": "ok"}
