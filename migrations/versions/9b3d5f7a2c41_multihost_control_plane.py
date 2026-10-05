"""M8 multi-host: scheduler leases and the PostgreSQL control plane.

Adds, on PostgreSQL only (SQLite self-host keeps its local files):

``scheduler_leases``
    One row per periodic scheduler (``scheduler:<subsystem>``). A holder owns it
    until ``expires_at`` on the DATABASE clock; every takeover increments
    ``fencing_token``, which fences writes from a deposed holder. Global control
    table (like ``jobs``), so no workspace policy.

``app_settings``
    The global key/value settings that lived in ``data/data.db``. Sensitive
    values are stored as ``enc:v1:`` / ``enc:v2:vault:`` envelopes. Forced RLS,
    readable only inside a transaction that declares ``app.control_plane = on``.

The workspace directory that lived in ``data/workspaces.db``: ``workspaces``,
``workspace_settings`` (holds the encrypted per-workspace secrets),
``workspace_members``, ``workspace_member_permissions``,
``workspace_oidc_identities``, ``workspace_scim_*`` and ``user_active_workspace``.
No foreign keys, matching the SQLite files (which never enabled them), so a
backfill never fails on an orphan row it should report instead.

Row-level security, all tables FORCEd (the owner is bound too):

* ``workspace_settings`` is strictly tenant scoped:
  ``workspace_id = current_setting('app.workspace_id', true)``. Nothing else
  can read secrets, not even the control-plane flag.
* The other directory tables are tenant scoped OR visible to a transaction
  that sets ``app.control_plane = on``. Membership lookups ("which workspaces
  does this user belong to?") are cross-tenant by nature, so the directory code
  path opts in explicitly. A tenant-bound ORM session never sets the flag and
  therefore sees only its own workspace, so a missing ``WHERE`` elsewhere in
  the application cannot read another tenant's members, tokens or identities.

Additive and reversible: ``downgrade`` drops exactly these tables.

Revision ID: 9b3d5f7a2c41
Revises: 7c1e5a9d3b20
"""
import os

from alembic import op

revision = "9b3d5f7a2c41"
down_revision = "7c1e5a9d3b20"
branch_labels = None
depends_on = None

TENANT = "workspace_id = current_setting('app.workspace_id', true)"
CONTROL = "current_setting('app.control_plane', true) = 'on'"

# (table, policy predicate). workspace_settings is deliberately tenant-only.
RLS = {
    "workspace_settings": TENANT,
    "workspaces": f"id = current_setting('app.workspace_id', true) OR {CONTROL}",
    "workspace_members": f"{TENANT} OR {CONTROL}",
    "workspace_member_permissions": f"{TENANT} OR {CONTROL}",
    "workspace_oidc_identities": f"{TENANT} OR {CONTROL}",
    "workspace_scim_tokens": f"{TENANT} OR {CONTROL}",
    "workspace_scim_users": f"{TENANT} OR {CONTROL}",
    "workspace_scim_groups": f"{TENANT} OR {CONTROL}",
    "workspace_scim_group_members": f"{TENANT} OR {CONTROL}",
    "user_active_workspace": f"{TENANT} OR {CONTROL}",
    "app_settings": CONTROL,
}

TABLES = ("scheduler_leases",) + tuple(RLS)


def upgrade():
    bind = op.get_bind()
    if bind.dialect.name != "postgresql":
        return
    role = bind.dialect.identifier_preparer.quote(os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app"))

    op.execute("""
        CREATE TABLE scheduler_leases (
            name TEXT PRIMARY KEY,
            holder TEXT NOT NULL,
            fencing_token BIGINT NOT NULL CHECK (fencing_token >= 1),
            acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            renewed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            expires_at TIMESTAMPTZ NOT NULL
        )""")

    op.execute("""
        CREATE TABLE app_settings (
            key TEXT PRIMARY KEY,
            value TEXT NOT NULL DEFAULT '',
            updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
        )""")

    op.execute("""
        CREATE TABLE workspaces (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            slug TEXT NOT NULL UNIQUE,
            description TEXT DEFAULT '',
            icon TEXT DEFAULT '',
            owner_id INTEGER,
            created_at DOUBLE PRECISION,
            updated_at DOUBLE PRECISION
        )""")
    op.execute("""
        CREATE TABLE workspace_settings (
            workspace_id TEXT NOT NULL,
            key TEXT NOT NULL,
            value TEXT DEFAULT '',
            PRIMARY KEY (workspace_id, key)
        )""")
    op.execute("""
        CREATE TABLE workspace_members (
            workspace_id TEXT NOT NULL,
            user_id INTEGER NOT NULL,
            role TEXT DEFAULT 'member',
            created_at DOUBLE PRECISION,
            PRIMARY KEY (workspace_id, user_id)
        )""")
    op.execute("CREATE INDEX ix_workspace_members_user ON workspace_members (user_id)")
    op.execute("""
        CREATE TABLE workspace_member_permissions (
            workspace_id TEXT NOT NULL,
            user_id INTEGER NOT NULL,
            permission TEXT NOT NULL,
            effect TEXT NOT NULL CHECK (effect IN ('allow', 'deny')),
            updated_at DOUBLE PRECISION NOT NULL,
            PRIMARY KEY (workspace_id, user_id, permission)
        )""")
    op.execute("""
        CREATE TABLE workspace_oidc_identities (
            workspace_id TEXT NOT NULL,
            issuer TEXT NOT NULL,
            subject TEXT NOT NULL,
            user_id INTEGER NOT NULL,
            email TEXT DEFAULT '',
            created_at DOUBLE PRECISION NOT NULL,
            PRIMARY KEY (workspace_id, issuer, subject),
            UNIQUE (workspace_id, user_id)
        )""")
    op.execute("""
        CREATE TABLE workspace_scim_tokens (
            workspace_id TEXT PRIMARY KEY,
            token_hash TEXT NOT NULL,
            token_prefix TEXT NOT NULL,
            created_at DOUBLE PRECISION NOT NULL,
            expires_at DOUBLE PRECISION,
            last_used_at DOUBLE PRECISION,
            created_by INTEGER
        )""")
    op.execute("""
        CREATE TABLE workspace_scim_users (
            workspace_id TEXT NOT NULL,
            user_id INTEGER NOT NULL,
            external_id TEXT DEFAULT '',
            display_name TEXT DEFAULT '',
            active INTEGER NOT NULL DEFAULT 1,
            created_at DOUBLE PRECISION NOT NULL,
            updated_at DOUBLE PRECISION NOT NULL,
            PRIMARY KEY (workspace_id, user_id)
        )""")
    op.execute("""
        CREATE TABLE workspace_scim_groups (
            id TEXT PRIMARY KEY,
            workspace_id TEXT NOT NULL,
            external_id TEXT DEFAULT '',
            display_name TEXT NOT NULL,
            created_at DOUBLE PRECISION NOT NULL,
            updated_at DOUBLE PRECISION NOT NULL,
            UNIQUE (workspace_id, display_name)
        )""")
    op.execute("""
        CREATE TABLE workspace_scim_group_members (
            workspace_id TEXT NOT NULL,
            group_id TEXT NOT NULL,
            user_id INTEGER NOT NULL,
            created_at DOUBLE PRECISION NOT NULL,
            PRIMARY KEY (workspace_id, group_id, user_id)
        )""")
    op.execute("""
        CREATE TABLE user_active_workspace (
            user_id INTEGER PRIMARY KEY,
            workspace_id TEXT NOT NULL
        )""")

    for table in TABLES:
        op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON {table} TO {role}")
    for table, predicate in RLS.items():
        op.execute(f"ALTER TABLE {table} ENABLE ROW LEVEL SECURITY")
        op.execute(f"ALTER TABLE {table} FORCE ROW LEVEL SECURITY")
        op.execute(
            f"CREATE POLICY workspace_isolation ON {table} "
            f"USING ({predicate}) WITH CHECK ({predicate})"
        )


def downgrade():
    if op.get_bind().dialect.name != "postgresql":
        return
    for table in reversed(TABLES):
        op.execute(f"DROP TABLE IF EXISTS {table}")
