"""The existing workspace tests, run again against the PostgreSQL metadata store.

    TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
        uv run pytest tests/test_multihost_meta_parity_pg.py

The workspace manager, SCIM, OIDC SSO, RBAC and ``/api/auth/workspace-context``
tests were written against ``data/workspaces.db``. They only reach that file
through ``manager._project_root`` and the manager API, so importing them here
and switching ``WORKSPACE_META_STORE=postgres`` for the module re-runs the very
same assertions against the shared tables, as the NOSUPERUSER NOBYPASSRLS
runtime role under forced row-level security. A divergence between the two
backends fails one of these. (The legacy-file schema upgrade test is SQLite
specific and is deliberately not imported.)
"""
import pytest
from sqlalchemy import create_engine, text

from tests.multihost_support import TEST_DATABASE_URL, temp_database

# Re-collected here, under the PostgreSQL fixture below.
from tests.test_workspace_context_api import (  # noqa: F401
    env,
    test_defaults_to_active_workspace,
    test_missing_invalid_or_refresh_token_is_unauthorized,
    test_non_member_is_forbidden,
    test_owner_role_reported,
    test_reports_requested_workspace_and_role,
    test_sso_enforcement_applies,
)
from tests.test_workspace_oidc import (  # noqa: F401
    test_oidc_callback_jit_provisions_binds_and_deprovisions,
    test_oidc_callback_rejects_unverified_or_wrong_domain,
    test_oidc_config_is_allowlisted_and_secret_is_encrypted,
    test_oidc_enforcement_requires_enabled_provider,
    test_oidc_jit_stale_user_lookup_converges_on_unique_account,
    test_query_transport_obeys_workspace_sso,
    test_refresh_preserves_sso_authentication_method,
    test_workspace_sso_enforcement_and_owner_break_glass,
)
from tests.test_workspace_rbac import (  # noqa: F401
    test_governance_policy_api_lists_and_updates_members,
    test_member_permission_persistence_and_cleanup,
    test_permission_overrides_precede_role_defaults,
    test_workspace_member_lifecycle_preserves_overrides,
)
from tests.test_workspace_scim import (  # noqa: F401
    test_scim_group_lifecycle_and_membership_patch,
    test_scim_directory_pages_are_bounded_stable_and_workspace_scoped,
    test_scim_rejects_wrong_token_and_username_mutation,
    test_scim_token_expiry_fails_closed,
    test_scim_tokens_are_hashed_rotatable_and_workspace_scoped,
    test_scim_user_lifecycle_and_filter,
)

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL, reason="TEST_DATABASE_URL not set (PostgreSQL metadata tests skipped)"
)

CONTROL_PLANE_TABLES = (
    "workspaces, workspace_settings, workspace_members, workspace_member_permissions, "
    "workspace_oidc_identities, workspace_scim_tokens, workspace_scim_users, "
    "workspace_scim_groups, workspace_scim_group_members, user_active_workspace, app_settings"
)


@pytest.fixture(scope="module")
def _pgdb():
    with temp_database() as d:
        yield d


@pytest.fixture(autouse=True)
def pg_backend(_pgdb, monkeypatch):
    from apps.api.core.config import settings
    from apps.api.services.workspace import pg_meta

    owner = create_engine(_pgdb.owner_url)
    with owner.begin() as c:
        c.execute(text(f"TRUNCATE {CONTROL_PLANE_TABLES}"))
    owner.dispose()
    engine = create_engine(_pgdb.app_url, pool_size=5, max_overflow=0)
    pg_meta._ready.clear()               # re-seed the default workspace after TRUNCATE
    pg_meta.use_engine(engine)
    monkeypatch.setattr(settings, "WORKSPACE_META_STORE", "postgres")
    yield
    pg_meta.use_engine(None)
    engine.dispose()
