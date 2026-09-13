"""Workspace-scoped SCIM credential and directory mapping store."""

import hashlib
import hmac
import secrets
import time
import uuid
from typing import Optional

from apps.api.services.workspace import manager


def _digest(token: str) -> str:
    return hashlib.sha256(token.encode("utf-8")).hexdigest()


def rotate_password() -> str:
    """Create an unusable random local password for directory-created users."""
    return secrets.token_urlsafe(48)


def rotate_token(workspace_id: str, created_by: int) -> str:
    token = f"og_scim_{secrets.token_urlsafe(36)}"
    conn = manager._get_db()
    conn.execute(
        "INSERT INTO workspace_scim_tokens (workspace_id, token_hash, token_prefix, created_at, created_by) VALUES (?, ?, ?, ?, ?) "
        "ON CONFLICT(workspace_id) DO UPDATE SET token_hash=excluded.token_hash, token_prefix=excluded.token_prefix, created_at=excluded.created_at, created_by=excluded.created_by",
        (workspace_id, _digest(token), token[:16], time.time(), created_by),
    )
    conn.commit(); conn.close()
    return token


def revoke_token(workspace_id: str) -> None:
    conn = manager._get_db()
    conn.execute("DELETE FROM workspace_scim_tokens WHERE workspace_id = ?", (workspace_id,))
    conn.commit(); conn.close()


def token_status(workspace_id: str) -> Optional[dict]:
    conn = manager._get_db()
    row = conn.execute("SELECT token_prefix, created_at, created_by FROM workspace_scim_tokens WHERE workspace_id = ?", (workspace_id,)).fetchone()
    conn.close()
    return dict(row) if row else None


def authenticate(workspace_id: str, token: str) -> bool:
    conn = manager._get_db()
    row = conn.execute("SELECT token_hash FROM workspace_scim_tokens WHERE workspace_id = ?", (workspace_id,)).fetchone()
    conn.close()
    return bool(row and token and hmac.compare_digest(row["token_hash"], _digest(token)))


def get_mapping(workspace_id: str, user_id: int) -> Optional[dict]:
    conn = manager._get_db()
    row = conn.execute("SELECT * FROM workspace_scim_users WHERE workspace_id = ? AND user_id = ?", (workspace_id, user_id)).fetchone()
    conn.close()
    return dict(row) if row else None


def list_mappings(workspace_id: str) -> list[dict]:
    conn = manager._get_db()
    rows = conn.execute("SELECT * FROM workspace_scim_users WHERE workspace_id = ? ORDER BY user_id", (workspace_id,)).fetchall()
    conn.close()
    return [dict(row) for row in rows]


def upsert_mapping(workspace_id: str, user_id: int, external_id: str, display_name: str, active: bool) -> dict:
    now = time.time(); conn = manager._get_db()
    if external_id:
        collision = conn.execute(
            "SELECT user_id FROM workspace_scim_users WHERE workspace_id = ? AND external_id = ? AND user_id != ?",
            (workspace_id, external_id, user_id),
        ).fetchone()
        if collision:
            conn.close()
            raise ValueError("SCIM externalId already exists")
    conn.execute(
        "INSERT INTO workspace_scim_users (workspace_id, user_id, external_id, display_name, active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?) "
        "ON CONFLICT(workspace_id, user_id) DO UPDATE SET external_id=excluded.external_id, display_name=excluded.display_name, active=excluded.active, updated_at=excluded.updated_at",
        (workspace_id, user_id, external_id, display_name, int(active), now, now),
    )
    conn.commit(); conn.close()
    return get_mapping(workspace_id, user_id)


def create_group(workspace_id: str, display_name: str, external_id: str = "") -> dict:
    now = time.time(); group_id = str(uuid.uuid4()); conn = manager._get_db()
    conn.execute("INSERT INTO workspace_scim_groups (id, workspace_id, external_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)", (group_id, workspace_id, external_id, display_name, now, now))
    conn.commit(); conn.close()
    return get_group(workspace_id, group_id)


def get_group(workspace_id: str, group_id: str) -> Optional[dict]:
    conn = manager._get_db()
    row = conn.execute("SELECT * FROM workspace_scim_groups WHERE workspace_id=? AND id=?", (workspace_id, group_id)).fetchone()
    if not row: conn.close(); return None
    result = dict(row)
    result["members"] = [str(item["user_id"]) for item in conn.execute("SELECT user_id FROM workspace_scim_group_members WHERE workspace_id=? AND group_id=? ORDER BY user_id", (workspace_id, group_id)).fetchall()]
    conn.close(); return result


def list_groups(workspace_id: str) -> list[dict]:
    conn = manager._get_db(); ids = [row["id"] for row in conn.execute("SELECT id FROM workspace_scim_groups WHERE workspace_id=? ORDER BY display_name", (workspace_id,)).fetchall()]; conn.close()
    return [get_group(workspace_id, group_id) for group_id in ids]


def update_group(workspace_id: str, group_id: str, display_name: str, external_id: str) -> Optional[dict]:
    conn = manager._get_db(); cursor = conn.execute("UPDATE workspace_scim_groups SET display_name=?, external_id=?, updated_at=? WHERE workspace_id=? AND id=?", (display_name, external_id, time.time(), workspace_id, group_id)); conn.commit(); conn.close()
    return get_group(workspace_id, group_id) if cursor.rowcount else None


def add_group_members(workspace_id: str, group_id: str, user_ids: list[int]) -> None:
    for user_id in user_ids:
        if not get_mapping(workspace_id, user_id): raise ValueError(f"SCIM user {user_id} not found")
    conn = manager._get_db(); now = time.time()
    for user_id in user_ids:
        conn.execute("INSERT OR IGNORE INTO workspace_scim_group_members (workspace_id, group_id, user_id, created_at) VALUES (?, ?, ?, ?)", (workspace_id, group_id, user_id, now))
    conn.execute("UPDATE workspace_scim_groups SET updated_at=? WHERE workspace_id=? AND id=?", (now, workspace_id, group_id)); conn.commit(); conn.close()


def remove_group_members(workspace_id: str, group_id: str, user_ids: list[int]) -> None:
    conn = manager._get_db()
    for user_id in user_ids: conn.execute("DELETE FROM workspace_scim_group_members WHERE workspace_id=? AND group_id=? AND user_id=?", (workspace_id, group_id, user_id))
    conn.execute("UPDATE workspace_scim_groups SET updated_at=? WHERE workspace_id=? AND id=?", (time.time(), workspace_id, group_id)); conn.commit(); conn.close()


def delete_group(workspace_id: str, group_id: str) -> bool:
    conn = manager._get_db(); conn.execute("DELETE FROM workspace_scim_group_members WHERE workspace_id=? AND group_id=?", (workspace_id, group_id)); cursor = conn.execute("DELETE FROM workspace_scim_groups WHERE workspace_id=? AND id=?", (workspace_id, group_id)); conn.commit(); conn.close(); return cursor.rowcount > 0
