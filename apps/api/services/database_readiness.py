"""Live PostgreSQL migration and tenant-isolation release checks."""

from __future__ import annotations

from pathlib import Path
from typing import Any, Iterable, Mapping

from alembic.config import Config
from alembic.script import ScriptDirectory
from sqlalchemy import text

from apps.api.database import engine


# These workspace-bearing tables are intentionally usable before a tenant GUC
# exists. Adding an exemption is a security decision and must be reviewed.
RLS_EXEMPT_TABLES = frozenset({
    "jobs",
    "mcp_tokens",
    "workbook_ingest_tokens",
    "destination_inbound_credentials",
    "scheduled_triggers",
    "watch_schedules",
    "outreach_schedules",
    "outreach_inbound_schedules",
    "audience_schedules",
    "playbook_schedules",
    "retention_schedules",
})


def _code_heads() -> list[str]:
    root = Path(__file__).resolve().parents[3]
    config = Config(str(root / "alembic.ini"))
    config.set_main_option("script_location", str(root / "migrations"))
    return sorted(ScriptDirectory.from_config(config).get_heads())


def _policy_is_scoped(policy: Mapping[str, Any]) -> bool:
    qualification = str(policy.get("qual") or "")
    check = str(policy.get("with_check") or "")
    return bool(
        str(policy.get("permissive") or "").upper() == "PERMISSIVE"
        and "workspace_id" in qualification
        and "current_setting('app.workspace_id'" in qualification
        and "workspace_id" in check
        and "current_setting('app.workspace_id'" in check
    )


def evaluate_database_readiness(
    *,
    code_heads: Iterable[str],
    database_heads: Iterable[str],
    role: Mapping[str, Any],
    tenant_tables: Iterable[str],
    table_security: Mapping[str, tuple[bool, bool]],
    policies: Mapping[str, list[Mapping[str, Any]]],
) -> dict[str, Any]:
    code = sorted(set(code_heads))
    database = sorted(set(database_heads))
    unsafe_role = bool(role.get("rolsuper") or role.get("rolbypassrls"))
    violations: dict[str, list[str]] = {}
    for table in sorted(set(tenant_tables) - RLS_EXEMPT_TABLES):
        enabled, forced = table_security.get(table, (False, False))
        reasons = []
        if not enabled:
            reasons.append("rls_disabled")
        if not forced:
            reasons.append("rls_not_forced")
        table_policies = policies.get(table, [])
        if len(table_policies) != 1 or not all(
            _policy_is_scoped(policy) for policy in table_policies
        ):
            reasons.append("workspace_policy_invalid")
        if reasons:
            violations[table] = reasons
    reason_codes = []
    if len(code) != 1:
        reason_codes.append("multiple_code_heads")
    if database != code:
        reason_codes.append("migration_head_mismatch")
    if unsafe_role or not role.get("current_user"):
        reason_codes.append("unsafe_runtime_role")
    if violations:
        reason_codes.append("tenant_rls_invalid")
    return {
        "eligible": not reason_codes,
        "reason_codes": reason_codes,
        "code_heads": code,
        "database_heads": database,
        "runtime_role": {
            "name": role.get("current_user"),
            "superuser": bool(role.get("rolsuper")),
            "bypass_rls": bool(role.get("rolbypassrls")),
        },
        "tenant_tables_checked": len(set(tenant_tables) - RLS_EXEMPT_TABLES),
        "exempt_tables_present": sorted(set(tenant_tables) & RLS_EXEMPT_TABLES),
        "violations": violations,
    }


def database_readiness() -> dict[str, Any]:
    if engine.dialect.name != "postgresql":
        return {"eligible": False, "reason_codes": ["postgresql_required"]}
    try:
        with engine.connect() as connection:
            role_row = connection.execute(text(
                "SELECT current_user, rolsuper, rolbypassrls "
                "FROM pg_roles WHERE rolname = current_user"
            )).mappings().one_or_none()
            database_heads = connection.execute(text(
                "SELECT version_num FROM alembic_version"
            )).scalars().all()
            tenant_tables = connection.execute(text(
                "SELECT DISTINCT table_name FROM information_schema.columns "
                "WHERE table_schema='public' AND column_name='workspace_id'"
            )).scalars().all()
            security_rows = connection.execute(text(
                "SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity "
                "FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace "
                "WHERE n.nspname='public' AND c.relkind='r'"
            )).all()
            policy_rows = connection.execute(text(
                "SELECT tablename, permissive, qual, with_check FROM pg_policies "
                "WHERE schemaname='public'"
            )).mappings().all()
    except Exception as exc:
        return {
            "eligible": False,
            "reason_codes": ["database_check_failed"],
            "error_type": type(exc).__name__,
        }
    security = {
        row[0]: (bool(row[1]), bool(row[2])) for row in security_rows
    }
    policies: dict[str, list[Mapping[str, Any]]] = {}
    for row in policy_rows:
        policies.setdefault(str(row["tablename"]), []).append(row)
    return evaluate_database_readiness(
        code_heads=_code_heads(),
        database_heads=database_heads,
        role=role_row or {},
        tenant_tables=tenant_tables,
        table_security=security,
        policies=policies,
    )
