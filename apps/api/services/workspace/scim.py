"""Workspace-scoped SCIM credential and directory mapping store."""

import hashlib
import hmac
import os
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


def _token_ttl_seconds() -> int:
    try:
        days = int(os.getenv("OPENGTM_SCIM_TOKEN_TTL_DAYS", "90"))
    except ValueError:
        days = 90
    return max(1, min(days, 365)) * 86400


def rotate_token(workspace_id: str, created_by: int) -> str:
    token = f"og_scim_{secrets.token_urlsafe(36)}"
    now = time.time()
    expires_at = now + _token_ttl_seconds()
    conn = manager._get_db()
    conn.execute(
        "INSERT INTO workspace_scim_tokens (workspace_id, token_hash, token_prefix, created_at, expires_at, last_used_at, created_by) VALUES (?, ?, ?, ?, ?, NULL, ?) "
        "ON CONFLICT(workspace_id) DO UPDATE SET token_hash=excluded.token_hash, token_prefix=excluded.token_prefix, created_at=excluded.created_at, expires_at=excluded.expires_at, last_used_at=NULL, created_by=excluded.created_by",
        (workspace_id, _digest(token), token[:16], now, expires_at, created_by),
    )
    conn.commit(); conn.close()
    return token


def revoke_token(workspace_id: str) -> None:
    conn = manager._get_db()
    conn.execute("DELETE FROM workspace_scim_tokens WHERE workspace_id = ?", (workspace_id,))
    conn.commit(); conn.close()


def token_status(workspace_id: str) -> Optional[dict]:
    conn = manager._get_db()
    row = conn.execute("SELECT token_prefix, created_at, expires_at, last_used_at, created_by FROM workspace_scim_tokens WHERE workspace_id = ?", (workspace_id,)).fetchone()
    conn.close()
    if not row:
        return None
    result = dict(row)
    result["expires_at"] = result["expires_at"] or result["created_at"] + _token_ttl_seconds()
    result["expired"] = result["expires_at"] <= time.time()
    return result


def authenticate(workspace_id: str, token: str) -> bool:
    conn = manager._get_db()
    row = conn.execute("SELECT token_hash, created_at, expires_at, last_used_at FROM workspace_scim_tokens WHERE workspace_id = ?", (workspace_id,)).fetchone()
    now = time.time()
    valid = bool(
        row and token
        and (row["expires_at"] or row["created_at"] + _token_ttl_seconds()) > now
        and hmac.compare_digest(row["token_hash"], _digest(token))
    )
    if valid and (not row["last_used_at"] or now - row["last_used_at"] >= 60):
        conn.execute("UPDATE workspace_scim_tokens SET last_used_at=? WHERE workspace_id=?", (now, workspace_id))
        conn.commit()
    conn.close()
    return valid


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


def _validated_member_ids(conn, workspace_id: str, user_ids: list[int]) -> list[int]:
    unique_ids = list(dict.fromkeys(user_ids))
    for user_id in unique_ids:
        mapping = conn.execute(
            "SELECT 1 FROM workspace_scim_users WHERE workspace_id=? AND user_id=?",
            (workspace_id, user_id),
        ).fetchone()
        if not mapping:
            raise ValueError(f"SCIM user {user_id} not found")
    return unique_ids


def create_group(
    workspace_id: str,
    display_name: str,
    external_id: str = "",
    user_ids: Optional[list[int]] = None,
) -> dict:
    """Atomically create a group and its validated initial membership."""
    now = time.time(); group_id = str(uuid.uuid4()); conn = manager._get_db()
    try:
        unique_ids = _validated_member_ids(conn, workspace_id, user_ids or [])
        conn.execute(
            "INSERT INTO workspace_scim_groups "
            "(id, workspace_id, external_id, display_name, created_at, updated_at) "
            "VALUES (?, ?, ?, ?, ?, ?)",
            (group_id, workspace_id, external_id, display_name, now, now),
        )
        for user_id in unique_ids:
            conn.execute(
                "INSERT INTO workspace_scim_group_members "
                "(workspace_id, group_id, user_id, created_at) VALUES (?, ?, ?, ?)",
                (workspace_id, group_id, user_id, now),
            )
        conn.commit()
    except Exception:
        conn.rollback()
        raise
    finally:
        conn.close()
    return get_group(workspace_id, group_id)


def replace_group(
    workspace_id: str,
    group_id: str,
    display_name: str,
    external_id: str,
    user_ids: list[int],
) -> Optional[dict]:
    """Atomically replace group metadata and membership after validation."""
    conn = manager._get_db()
    try:
        group = conn.execute(
            "SELECT id FROM workspace_scim_groups WHERE workspace_id=? AND id=?",
            (workspace_id, group_id),
        ).fetchone()
        if not group:
            return None
        unique_ids = _validated_member_ids(conn, workspace_id, user_ids)
        now = time.time()
        conn.execute(
            "UPDATE workspace_scim_groups SET display_name=?, external_id=?, updated_at=? "
            "WHERE workspace_id=? AND id=?",
            (display_name, external_id, now, workspace_id, group_id),
        )
        conn.execute(
            "DELETE FROM workspace_scim_group_members WHERE workspace_id=? AND group_id=?",
            (workspace_id, group_id),
        )
        for user_id in unique_ids:
            conn.execute(
                "INSERT INTO workspace_scim_group_members "
                "(workspace_id, group_id, user_id, created_at) VALUES (?, ?, ?, ?)",
                (workspace_id, group_id, user_id, now),
            )
        conn.commit()
    except Exception:
        conn.rollback()
        raise
    finally:
        conn.close()
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
    conn = manager._get_db(); now = time.time()
    try:
        if not conn.execute(
            "SELECT 1 FROM workspace_scim_groups WHERE workspace_id=? AND id=?",
            (workspace_id, group_id),
        ).fetchone():
            raise ValueError("SCIM group not found")
        unique_ids = _validated_member_ids(conn, workspace_id, user_ids)
        for user_id in unique_ids:
            conn.execute(
                "INSERT OR IGNORE INTO workspace_scim_group_members "
                "(workspace_id, group_id, user_id, created_at) VALUES (?, ?, ?, ?)",
                (workspace_id, group_id, user_id, now),
            )
        conn.execute(
            "UPDATE workspace_scim_groups SET updated_at=? WHERE workspace_id=? AND id=?",
            (now, workspace_id, group_id),
        )
        conn.commit()
    except Exception:
        conn.rollback()
        raise
    finally:
        conn.close()


def remove_group_members(workspace_id: str, group_id: str, user_ids: list[int]) -> None:
    conn = manager._get_db()
    try:
        if not conn.execute(
            "SELECT 1 FROM workspace_scim_groups WHERE workspace_id=? AND id=?",
            (workspace_id, group_id),
        ).fetchone():
            raise ValueError("SCIM group not found")
        for user_id in list(dict.fromkeys(user_ids)):
            conn.execute(
                "DELETE FROM workspace_scim_group_members "
                "WHERE workspace_id=? AND group_id=? AND user_id=?",
                (workspace_id, group_id, user_id),
            )
        conn.execute(
            "UPDATE workspace_scim_groups SET updated_at=? WHERE workspace_id=? AND id=?",
            (time.time(), workspace_id, group_id),
        )
        conn.commit()
    except Exception:
        conn.rollback()
        raise
    finally:
        conn.close()


def delete_group(workspace_id: str, group_id: str) -> bool:
    conn = manager._get_db(); conn.execute("DELETE FROM workspace_scim_group_members WHERE workspace_id=? AND group_id=?", (workspace_id, group_id)); cursor = conn.execute("DELETE FROM workspace_scim_groups WHERE workspace_id=? AND id=?", (workspace_id, group_id)); conn.commit(); conn.close(); return cursor.rowcount > 0
