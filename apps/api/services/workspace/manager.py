"""
Workspace Manager — Multi-tenant workspace isolation for agencies.

Each workspace gets its own leads database, settings, and workbooks.
Default workspace = "main" for single-user mode.
Agency mode = one workspace per client.
"""

import json
import os
import time
import logging
import sqlite3
import uuid
from typing import List, Optional, Dict, Any
from dataclasses import dataclass, field, asdict
from pathlib import Path

logger = logging.getLogger("workspace.manager")


@dataclass
class Workspace:
    id: str = field(default_factory=lambda: str(uuid.uuid4()))
    name: str = "Default"
    slug: str = "main"
    description: str = ""
    icon: str = "🏢"
    leads_count: int = 0
    workbooks_count: int = 0
    created_at: float = field(default_factory=time.time)
    updated_at: float = field(default_factory=time.time)


def _project_root() -> Path:
    return Path(__file__).resolve().parents[4]


def _get_db():
    """Get workspaces meta DB."""
    db_path = _project_root() / "data" / "workspaces.db"
    db_path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(db_path))
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS workspaces (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            slug TEXT UNIQUE NOT NULL,
            description TEXT DEFAULT '',
            icon TEXT DEFAULT '🏢',
            created_at REAL,
            updated_at REAL
        );

        CREATE TABLE IF NOT EXISTS workspace_settings (
            workspace_id TEXT NOT NULL,
            key TEXT NOT NULL,
            value TEXT DEFAULT '',
            PRIMARY KEY (workspace_id, key),
            FOREIGN KEY (workspace_id) REFERENCES workspaces(id)
        );
    """)
    conn.commit()

    # Ensure default workspace exists
    existing = conn.execute("SELECT id FROM workspaces WHERE slug = 'main'").fetchone()
    if not existing:
        now = time.time()
        conn.execute(
            "INSERT INTO workspaces (id, name, slug, description, icon, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
            (str(uuid.uuid4()), "Default Workspace", "main", "Your main workspace", "🏠", now, now),
        )
        conn.commit()

    return conn


def _get_active_workspace_id() -> str:
    """Get the currently active workspace ID."""
    try:
        from apps.api.routers.settings import _db_get
        ws_id = _db_get("ACTIVE_WORKSPACE", "")
        if ws_id:
            return ws_id
    except Exception:
        pass
    # Fallback to default
    conn = _get_db()
    row = conn.execute("SELECT id FROM workspaces WHERE slug = 'main'").fetchone()
    conn.close()
    return row["id"] if row else ""


def _set_active_workspace(ws_id: str):
    """Set the active workspace."""
    try:
        from apps.api.routers.settings import _db_set
        _db_set("ACTIVE_WORKSPACE", ws_id)
    except Exception:
        pass


# ── CRUD ──────────────────────────────────────────────────────

def create_workspace(name: str, description: str = "", icon: str = "🏢") -> Workspace:
    """Create a new workspace."""
    slug = name.lower().replace(" ", "-").replace("_", "-")
    slug = "".join(c for c in slug if c.isalnum() or c == "-")

    ws = Workspace(name=name, slug=slug, description=description, icon=icon)
    conn = _get_db()
    conn.execute(
        "INSERT INTO workspaces (id, name, slug, description, icon, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
        (ws.id, ws.name, ws.slug, ws.description, ws.icon, ws.created_at, ws.updated_at),
    )
    conn.commit()
    conn.close()

    # Create workspace data directory
    ws_dir = _project_root() / "data" / "workspaces" / ws.slug
    ws_dir.mkdir(parents=True, exist_ok=True)

    logger.info(f"Created workspace: {ws.name} ({ws.slug})")
    return ws


def list_workspaces() -> List[Workspace]:
    """List all workspaces with stats."""
    conn = _get_db()
    rows = conn.execute("SELECT * FROM workspaces ORDER BY created_at ASC").fetchall()
    conn.close()

    workspaces = []
    for r in rows:
        ws = Workspace(
            id=r["id"], name=r["name"], slug=r["slug"],
            description=r["description"] or "", icon=r["icon"] or "🏢",
            created_at=r["created_at"], updated_at=r["updated_at"],
        )
        # Count leads for this workspace
        ws.leads_count = _count_workspace_leads(ws.slug)
        workspaces.append(ws)

    return workspaces


def get_workspace(ws_id: str) -> Optional[Workspace]:
    """Get workspace by ID."""
    conn = _get_db()
    row = conn.execute("SELECT * FROM workspaces WHERE id = ?", (ws_id,)).fetchone()
    conn.close()
    if not row:
        return None
    return Workspace(
        id=row["id"], name=row["name"], slug=row["slug"],
        description=row["description"] or "", icon=row["icon"] or "🏢",
        created_at=row["created_at"], updated_at=row["updated_at"],
        leads_count=_count_workspace_leads(row["slug"]),
    )


def delete_workspace(ws_id: str) -> bool:
    """Delete a workspace (cannot delete 'main')."""
    conn = _get_db()
    row = conn.execute("SELECT slug FROM workspaces WHERE id = ?", (ws_id,)).fetchone()
    if not row or row["slug"] == "main":
        conn.close()
        return False
    conn.execute("DELETE FROM workspace_settings WHERE workspace_id = ?", (ws_id,))
    conn.execute("DELETE FROM workspaces WHERE id = ?", (ws_id,))
    conn.commit()
    conn.close()
    return True


def get_agency_dashboard() -> Dict[str, Any]:
    """Cross-workspace analytics for agency view."""
    workspaces = list_workspaces()
    total_leads = sum(ws.leads_count for ws in workspaces)

    return {
        "total_workspaces": len(workspaces),
        "total_leads": total_leads,
        "workspaces": [
            {
                "id": ws.id,
                "name": ws.name,
                "slug": ws.slug,
                "icon": ws.icon,
                "leads_count": ws.leads_count,
                "created_at": ws.created_at,
            }
            for ws in workspaces
        ],
    }


# ── Helpers ───────────────────────────────────────────────────

def _count_workspace_leads(slug: str) -> int:
    """Count leads in a workspace's DB."""
    if slug == "main":
        db_path = _project_root() / "data" / "leads.db"
    else:
        db_path = _project_root() / "data" / "workspaces" / slug / "leads.db"

    if not db_path.exists():
        return 0

    try:
        conn = sqlite3.connect(str(db_path))
        row = conn.execute("SELECT COUNT(*) as c FROM leads").fetchone()
        conn.close()
        return row[0] if row else 0
    except Exception:
        return 0
