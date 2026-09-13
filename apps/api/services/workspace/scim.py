"""Workspace-scoped SCIM credential and directory mapping store."""

import hashlib
import hmac
import secrets
import time
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
