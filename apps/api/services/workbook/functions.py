"""
Workbook Functions — Reusable, versioned column chains.

A function packages a set of workbook columns (enrichment chain) as a
named, versioned unit that can be applied to any workbook.
"""

import json
import time
import logging
import sqlite3
import uuid
from typing import List, Optional, Dict, Any
from pathlib import Path

logger = logging.getLogger("workbook.functions")


def _get_db():
    """Get functions DB."""
    project_root = Path(__file__).resolve().parents[4]
    db_path = project_root / "data" / "functions.db"
    db_path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(db_path))
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS functions (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            description TEXT DEFAULT '',
            category TEXT DEFAULT 'custom',
            columns_chain TEXT DEFAULT '[]',
            version INTEGER DEFAULT 1,
            usage_count INTEGER DEFAULT 0,
            created_at REAL,
            updated_at REAL
        );
    """)
    conn.commit()
    return conn


def create_function(
    name: str,
    description: str,
    columns_chain: List[dict],
    category: str = "custom",
) -> Dict[str, Any]:
    """Create a reusable function from a set of column definitions."""
    func_id = str(uuid.uuid4())
    now = time.time()
    conn = _get_db()
    conn.execute(
        "INSERT INTO functions (id, name, description, category, columns_chain, version, usage_count, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
        (func_id, name, description, category, json.dumps(columns_chain), 1, 0, now, now),
    )
    conn.commit()
    conn.close()
    logger.info(f"Created function: {name} ({len(columns_chain)} columns)")
    return {"id": func_id, "name": name, "columns_count": len(columns_chain)}


def list_functions(category: str = None) -> List[Dict[str, Any]]:
    """List all functions."""
    conn = _get_db()
    if category:
        rows = conn.execute("SELECT * FROM functions WHERE category = ? ORDER BY usage_count DESC", (category,)).fetchall()
    else:
        rows = conn.execute("SELECT * FROM functions ORDER BY usage_count DESC").fetchall()
    conn.close()
    return [
        {
            "id": r["id"],
            "name": r["name"],
            "description": r["description"],
            "category": r["category"],
            "columns_chain": json.loads(r["columns_chain"]),
            "columns_count": len(json.loads(r["columns_chain"])),
            "version": r["version"],
            "usage_count": r["usage_count"],
            "created_at": r["created_at"],
        }
        for r in rows
    ]


def get_function(func_id: str) -> Optional[Dict[str, Any]]:
    """Get a function by ID."""
    conn = _get_db()
    row = conn.execute("SELECT * FROM functions WHERE id = ?", (func_id,)).fetchone()
    conn.close()
    if not row:
        return None
    return {
        "id": row["id"],
        "name": row["name"],
        "description": row["description"],
        "category": row["category"],
        "columns_chain": json.loads(row["columns_chain"]),
        "version": row["version"],
        "usage_count": row["usage_count"],
    }


def apply_function(func_id: str, workbook_id: str, db_session) -> Dict[str, Any]:
    """Apply a function's column chain to a workbook.

    Appends the function's columns to the workbook's columns_config.
    """
    from apps.api.services.workbook.models import Workbook

    func = get_function(func_id)
    if not func:
        return {"error": "Function not found"}

    workbook = db_session.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not workbook:
        return {"error": "Workbook not found"}

    # Merge columns (avoid duplicates by key)
    existing_keys = {c.get("key") for c in (workbook.columns_config or [])}
    new_columns = [c for c in func["columns_chain"] if c.get("key") not in existing_keys]

    updated_config = list(workbook.columns_config or []) + new_columns
    workbook.columns_config = updated_config
    db_session.commit()

    # Increment usage count
    conn = _get_db()
    conn.execute("UPDATE functions SET usage_count = usage_count + 1 WHERE id = ?", (func_id,))
    conn.commit()
    conn.close()

    return {
        "applied": func["name"],
        "columns_added": len(new_columns),
        "total_columns": len(updated_config),
    }


def delete_function(func_id: str) -> bool:
    """Delete a function."""
    conn = _get_db()
    conn.execute("DELETE FROM functions WHERE id = ?", (func_id,))
    conn.commit()
    conn.close()
    return True
