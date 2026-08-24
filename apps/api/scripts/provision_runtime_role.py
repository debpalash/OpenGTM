"""Provision the concrete least-privilege PostgreSQL runtime login.

Alembic owns the NOLOGIN ``yupcha_app`` group and its grants. This helper,
enabled explicitly by the deployment, owns only the concrete login credential
and grants it membership in that group.
"""

from __future__ import annotations

import os
import re

from psycopg import sql

from apps.api.database import engine


def provision_runtime_role() -> None:
    if engine.dialect.name != "postgresql":
        return
    if os.getenv("YUPCHA_PROVISION_RUNTIME_ROLE", "").strip().lower() not in {
        "1", "true", "yes", "on"
    }:
        return

    group = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")
    login = os.getenv("YUPCHA_RUNTIME_DB_USER", "yupcha_runtime")
    password = os.getenv("YUPCHA_RUNTIME_DB_PASSWORD", "")
    for name, value in (("YUPCHA_APP_DB_ROLE", group), ("YUPCHA_RUNTIME_DB_USER", login)):
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", value):
            raise RuntimeError(f"{name} is not a safe PostgreSQL role identifier")
    if not password:
        raise RuntimeError("YUPCHA_RUNTIME_DB_PASSWORD must be set when provisioning is enabled")

    raw = engine.raw_connection()
    try:
        with raw.cursor() as cursor:
            cursor.execute("SELECT 1 FROM pg_roles WHERE rolname = %s", (group,))
            if cursor.fetchone() is None:
                raise RuntimeError(
                    f"application group role {group!r} is missing; run Alembic first"
                )
            cursor.execute("SELECT 1 FROM pg_roles WHERE rolname = %s", (login,))
            if cursor.fetchone() is None:
                cursor.execute(
                    sql.SQL("CREATE ROLE {} LOGIN INHERIT NOSUPERUSER NOBYPASSRLS PASSWORD {}")
                    .format(sql.Identifier(login), sql.Literal(password))
                )
            else:
                cursor.execute(
                    sql.SQL("ALTER ROLE {} LOGIN INHERIT NOSUPERUSER NOBYPASSRLS PASSWORD {}")
                    .format(sql.Identifier(login), sql.Literal(password))
                )
            cursor.execute(
                sql.SQL("GRANT {} TO {}").format(
                    sql.Identifier(group), sql.Identifier(login)
                )
            )
            cursor.execute(
                sql.SQL("GRANT CONNECT ON DATABASE {} TO {}").format(
                    sql.Identifier(engine.url.database), sql.Identifier(login)
                )
            )
            cursor.execute(
                sql.SQL("GRANT USAGE ON SCHEMA public TO {}").format(sql.Identifier(login))
            )
        raw.commit()
    except Exception:
        raw.rollback()
        raise
    finally:
        raw.close()


if __name__ == "__main__":
    provision_runtime_role()
