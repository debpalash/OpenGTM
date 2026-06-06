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
    owner_id: Optional[int] = None
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
            owner_id INTEGER,
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

        -- Tenant membership: which users belong to which workspaces
        CREATE TABLE IF NOT EXISTS workspace_members (
            workspace_id TEXT NOT NULL,
            user_id INTEGER NOT NULL,
            role TEXT DEFAULT 'member',  -- owner, admin, member, viewer
            created_at REAL,
            PRIMARY KEY (workspace_id, user_id),
            FOREIGN KEY (workspace_id) REFERENCES workspaces(id)
        );

        -- Per-user active workspace (replaces the global ACTIVE_WORKSPACE setting)
        CREATE TABLE IF NOT EXISTS user_active_workspace (
            user_id INTEGER PRIMARY KEY,
            workspace_id TEXT NOT NULL
        );
    """)
    # owner_id was added after the initial release — backfill the column for
    # pre-existing databases.
    cols = {r[1] for r in conn.execute("PRAGMA table_info(workspaces)").fetchall()}
    if "owner_id" not in cols:
        conn.execute("ALTER TABLE workspaces ADD COLUMN owner_id INTEGER")
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

def create_workspace(
    name: str, description: str = "", icon: str = "🏢", owner_id: Optional[int] = None
) -> Workspace:
    """Create a new workspace, owned by ``owner_id`` who becomes its first member."""
    slug = name.lower().replace(" ", "-").replace("_", "-")
    slug = "".join(c for c in slug if c.isalnum() or c == "-")

    ws = Workspace(name=name, slug=slug, description=description, icon=icon, owner_id=owner_id)
    conn = _get_db()
    conn.execute(
        "INSERT INTO workspaces (id, name, slug, description, icon, owner_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
        (ws.id, ws.name, ws.slug, ws.description, ws.icon, ws.owner_id, ws.created_at, ws.updated_at),
    )
    if owner_id is not None:
        conn.execute(
            "INSERT OR REPLACE INTO workspace_members (workspace_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)",
            (ws.id, owner_id, time.time()),
        )
    conn.commit()
    conn.close()

    # Create workspace data directory
    ws_dir = _project_root() / "data" / "workspaces" / ws.slug
    ws_dir.mkdir(parents=True, exist_ok=True)

    logger.info(f"Created workspace: {ws.name} ({ws.slug}) owner={owner_id}")
    return ws


# ── Membership & tenancy ──────────────────────────────────────

def add_member(workspace_id: str, user_id: int, role: str = "member") -> None:
    """Add (or update) a user's membership in a workspace."""
    conn = _get_db()
    conn.execute(
        "INSERT OR REPLACE INTO workspace_members (workspace_id, user_id, role, created_at) VALUES (?, ?, ?, ?)",
        (workspace_id, user_id, role, time.time()),
    )
    conn.commit()
    conn.close()


def remove_member(workspace_id: str, user_id: int) -> None:
    conn = _get_db()
    conn.execute(
        "DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?",
        (workspace_id, user_id),
    )
    conn.commit()
    conn.close()


def is_member(workspace_id: str, user_id: int) -> bool:
    """True if the user belongs to the workspace (or is its owner)."""
    conn = _get_db()
    row = conn.execute(
        "SELECT 1 FROM workspace_members WHERE workspace_id = ? AND user_id = ? "
        "UNION SELECT 1 FROM workspaces WHERE id = ? AND owner_id = ? LIMIT 1",
        (workspace_id, user_id, workspace_id, user_id),
    ).fetchone()
    conn.close()
    return row is not None


def member_role(workspace_id: str, user_id: int) -> Optional[str]:
    """Return the user's role in the workspace, or None if not a member."""
    conn = _get_db()
    row = conn.execute(
        "SELECT role FROM workspace_members WHERE workspace_id = ? AND user_id = ?",
        (workspace_id, user_id),
    ).fetchone()
    conn.close()
    return row["role"] if row else None


def list_user_workspace_ids(user_id: int) -> List[str]:
    """All workspace ids the user can access (member of, or owns)."""
    conn = _get_db()
    rows = conn.execute(
        "SELECT workspace_id AS id FROM workspace_members WHERE user_id = ? "
        "UNION SELECT id FROM workspaces WHERE owner_id = ?",
        (user_id, user_id),
    ).fetchall()
    conn.close()
    return [r["id"] for r in rows]


def list_user_workspaces(user_id: int) -> List[Workspace]:
    """Full workspace objects the user can access."""
    ids = set(list_user_workspace_ids(user_id))
    return [ws for ws in list_workspaces() if ws.id in ids]


def get_user_active_workspace(user_id: int) -> Optional[str]:
    """The user's currently active workspace id (must be one they belong to)."""
    conn = _get_db()
    row = conn.execute(
        "SELECT workspace_id FROM user_active_workspace WHERE user_id = ?", (user_id,)
    ).fetchone()
    conn.close()
    active = row["workspace_id"] if row else None
    if active and is_member(active, user_id):
        return active
    # Fall back to the first workspace the user can access
    ids = list_user_workspace_ids(user_id)
    return ids[0] if ids else None


def set_user_active_workspace(user_id: int, workspace_id: str) -> bool:
    """Set the user's active workspace; rejects workspaces they don't belong to."""
    if not is_member(workspace_id, user_id):
        return False
    conn = _get_db()
    conn.execute(
        "INSERT OR REPLACE INTO user_active_workspace (user_id, workspace_id) VALUES (?, ?)",
        (user_id, workspace_id),
    )
    conn.commit()
    conn.close()
    return True


def workspace_slug(workspace_id: str) -> Optional[str]:
    conn = _get_db()
    row = conn.execute("SELECT slug FROM workspaces WHERE id = ?", (workspace_id,)).fetchone()
    conn.close()
    return row["slug"] if row else None


def workspace_leads_db_path(slug: str) -> str:
    """Absolute path to a workspace's leads.db file.

    The "main" workspace maps to the collection pipeline's canonical DB
    (leadgen config DB_PATH) so that collected leads, the JobRunner, and the
    UI all read/write the SAME file. Other workspaces get their own file.
    """
    if slug == "main":
        from apps.api.services.leadgen.config import DB_PATH
        return str(DB_PATH)
    return str(_project_root() / "data" / "workspaces" / slug / "leads.db")


def ensure_tenancy_backfill(admin_user_id: int) -> None:
    """One-time backfill: assign owner-less workspaces to the admin and ensure
    the admin is a member of every workspace. Safe to call repeatedly."""
    conn = _get_db()
    conn.execute(
        "UPDATE workspaces SET owner_id = ? WHERE owner_id IS NULL", (admin_user_id,)
    )
    rows = conn.execute("SELECT id FROM workspaces").fetchall()
    now = time.time()
    for r in rows:
        conn.execute(
            "INSERT OR IGNORE INTO workspace_members (workspace_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)",
            (r["id"], admin_user_id, now),
        )
    conn.commit()
    conn.close()
    logger.info(f"Tenancy backfill complete: admin user {admin_user_id} owns existing workspaces")


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
            owner_id=r["owner_id"] if "owner_id" in r.keys() else None,
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
        owner_id=row["owner_id"] if "owner_id" in row.keys() else None,
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
    conn.execute("DELETE FROM workspace_members WHERE workspace_id = ?", (ws_id,))
    conn.execute("DELETE FROM user_active_workspace WHERE workspace_id = ?", (ws_id,))
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
