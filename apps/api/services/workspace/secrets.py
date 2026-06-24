"""
Per-workspace encrypted integration secrets (spec WI-6).

Multi-tenancy requires that Team A's HubSpot/SMTP/Salesforce credentials never
serve Team B on a shared instance. This module is the secret resolver:

  - ``set_secret(workspace_id, key, value)`` — encrypt at rest, store ciphertext
    in the existing ``workspace_settings`` table (raw SQL, mirroring
    services/workspace/manager.py). Plaintext is NEVER persisted.
  - ``get_secret(workspace_id, key)`` — decrypt the per-workspace secret; if the
    workspace has none, FALL BACK to the existing GLOBAL setting (settings DB /
    env) so single-tenant installs keep working unchanged.

ENVELOPE ENCRYPTION
-------------------
Values are encrypted with Fernet (AES-128-CBC + HMAC-SHA256, authenticated).
The Fernet master key is loaded from ``SECRETS_MASTER_KEY`` (a urlsafe-base64
32-byte key). When that env/config value is unset we DERIVE a key from
``SECRET_KEY`` so dev/test work out of the box — but we FAIL CLOSED in a real
deployment (non-dev APP_ENV) when there is no master key AND SECRET_KEY is still
the shipped insecure default, refusing to "encrypt" with a publicly-known key.

Stored ciphertext is prefixed with ``enc:v1:`` so a value is self-describing and
we can migrate key versions / encryption schemes later without ambiguity.

NOTE (migrations): ``workspace_settings`` already exists; no schema change is
needed here (we reuse the (workspace_id, key) -> value column). Once Alembic
lands (separate parallel PR), any column/table changes introduced for secrets
should get a real migration; for now we follow the repo's create-table-if-not-
exists pattern via the workspace manager's DB.
"""

import base64
import hashlib
import logging
import sqlite3
from typing import Optional, Tuple

from cryptography.fernet import Fernet, InvalidToken

logger = logging.getLogger("workspace.secrets")

# Ciphertext envelope prefix — lets get_secret() distinguish encrypted values
# from any legacy plaintext and from future scheme versions.
_ENC_PREFIX = "enc:v1:"


# ── Master key / cipher ────────────────────────────────────────────────────

def _derive_fernet_key(material: str) -> bytes:
    """Derive a urlsafe-base64 32-byte Fernet key from arbitrary key material.

    Fernet requires exactly 32 bytes (base64-encoded). We hash the material with
    SHA-256 so any-length SECRET_KEY yields a valid, stable key.
    """
    digest = hashlib.sha256(material.encode("utf-8")).digest()
    return base64.urlsafe_b64encode(digest)


def _load_master_key() -> bytes:
    """Resolve the Fernet master key, failing closed in real deployments.

    Order:
      1. SECRETS_MASTER_KEY (used verbatim if it's a valid Fernet key, else
         treated as key material and hashed into one).
      2. Fall back to deriving from SECRET_KEY — allowed in dev/test; refused in
         a real deployment when SECRET_KEY is still the insecure default.
    """
    from apps.api.core.config import get_settings

    settings = get_settings()
    raw = (settings.SECRETS_MASTER_KEY or "").strip()
    if raw:
        # Accept a real Fernet key as-is; otherwise hash whatever was provided.
        try:
            Fernet(raw.encode("utf-8"))
            return raw.encode("utf-8")
        except (ValueError, TypeError):
            return _derive_fernet_key(raw)

    # No explicit master key — derive from SECRET_KEY, but never let a real
    # deployment encrypt secrets under the publicly-known insecure default.
    if settings.secret_key_is_insecure and not settings.is_dev_env:
        raise RuntimeError(
            "Refusing to encrypt per-workspace secrets: SECRETS_MASTER_KEY is "
            f"unset and SECRET_KEY is the insecure default while APP_ENV="
            f"{settings.APP_ENV!r}. Set SECRETS_MASTER_KEY (a Fernet key) or a "
            "strong SECRET_KEY in the environment/.env."
        )
    return _derive_fernet_key(settings.SECRET_KEY)


def _cipher() -> Fernet:
    return Fernet(_load_master_key())


def encrypt_value(plaintext: str) -> str:
    """Encrypt a plaintext secret into the at-rest envelope form (enc:v1:...)."""
    token = _cipher().encrypt(plaintext.encode("utf-8")).decode("ascii")
    return _ENC_PREFIX + token


def decrypt_value(stored: str) -> str:
    """Decrypt a stored envelope value back to plaintext.

    Tolerates legacy plaintext (no envelope prefix) so a value written before
    this module shipped still reads back — it just isn't encrypted at rest.
    """
    if not stored:
        return ""
    if not stored.startswith(_ENC_PREFIX):
        # Legacy / externally-written plaintext: return as-is.
        return stored
    token = stored[len(_ENC_PREFIX):].encode("ascii")
    try:
        return _cipher().decrypt(token).decode("utf-8")
    except InvalidToken as e:
        # Wrong/rotated master key — surface loudly rather than silently
        # returning ciphertext as if it were a credential.
        raise RuntimeError(
            "Failed to decrypt a per-workspace secret: the master key does not "
            "match the key used to encrypt it (rotated/incorrect "
            "SECRETS_MASTER_KEY?)."
        ) from e


# ── Storage (workspace_settings, raw SQL — mirrors workspace/manager.py) ────

def _db() -> sqlite3.Connection:
    """Workspaces meta DB connection (creates schema via the manager)."""
    from apps.api.services.workspace.manager import _get_db
    return _get_db()


def set_secret(workspace_id: str, key: str, value: str) -> None:
    """Encrypt and store a per-workspace secret. Plaintext is never persisted.

    A blank/whitespace-only value is ignored so a saved secret is not clobbered
    by an empty form field (matches the global integrations endpoint behaviour).
    """
    if not workspace_id or not key:
        raise ValueError("workspace_id and key are required")
    if value is None or not value.strip():
        return
    ciphertext = encrypt_value(value.strip())
    conn = _db()
    try:
        conn.execute(
            "INSERT INTO workspace_settings (workspace_id, key, value) VALUES (?, ?, ?) "
            "ON CONFLICT(workspace_id, key) DO UPDATE SET value = excluded.value",
            (workspace_id, key, ciphertext),
        )
        conn.commit()
    finally:
        conn.close()
    logger.info("Set per-workspace secret ws=%s key=%s (encrypted)", workspace_id, key)


def _raw_workspace_value(workspace_id: str, key: str) -> Optional[str]:
    """Return the raw stored (ciphertext) value for a workspace key, or None."""
    if not workspace_id or not key:
        return None
    conn = _db()
    try:
        row = conn.execute(
            "SELECT value FROM workspace_settings WHERE workspace_id = ? AND key = ?",
            (workspace_id, key),
        ).fetchone()
    finally:
        conn.close()
    if row is None:
        return None
    # sqlite3.Row supports index access; manager configures row_factory=Row.
    val = row["value"] if hasattr(row, "keys") else row[0]
    return val if val else None


def has_workspace_secret(workspace_id: str, key: str) -> bool:
    """True if a per-workspace secret is set for this key (ignores global)."""
    return _raw_workspace_value(workspace_id, key) is not None


def get_secret_with_source(workspace_id: Optional[str], key: str,
                           default: str = "") -> Tuple[str, str]:
    """Resolve a secret, returning (plaintext, source).

    Resolution order:
      1. Per-workspace encrypted secret (source="workspace").
      2. Global setting via the settings DB / env (source="global").
      3. ``default`` (source="default").
    """
    if workspace_id:
        raw = _raw_workspace_value(workspace_id, key)
        if raw is not None:
            return decrypt_value(raw), "workspace"

    # Fall back to the existing global setting so single-tenant installs and any
    # workspace without its own credential keep working unchanged.
    try:
        from apps.api.routers.settings import _db_get
        global_val = _db_get(key, "")
    except Exception:
        import os
        global_val = os.environ.get(key, "")
    if global_val:
        return global_val, "global"
    return default, "default"


def get_secret(workspace_id: Optional[str], key: str, default: str = "") -> str:
    """Resolve a secret: per-workspace first, then global, then ``default``."""
    value, source = get_secret_with_source(workspace_id, key, default)
    if source != "default":
        logger.debug("Resolved secret key=%s from %s (ws=%s)", key, source, workspace_id)
    return value
