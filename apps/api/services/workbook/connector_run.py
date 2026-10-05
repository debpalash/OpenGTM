"""Which workbook runs may use the ``run_workbook_connector`` job type.

``run_workbook_connector`` is the slice of ``run_workbook`` that the Go worker
(apps/server/internal/jobs/enrich) can execute: explicit provider chains made
only of manifest v1 connectors, over workbook rows that are not linked to a
lead. The Python worker keeps a handler for it (the unchanged
``handle_run_workbook``), so ``opengtm routes set run_workbook_connector python``
is a complete rollback. See docs/plans/m2-enrichment-slice.md.

Nothing here changes how ``run_workbook`` behaves. The route only consults this
module when ``job_executor_routes`` sends the new type to Go.
"""

from __future__ import annotations

import logging
import os
import re
from typing import Iterable, Optional

logger = logging.getLogger("workbook.connector_run")

JOB_TYPE = "run_workbook_connector"
LEGACY_JOB_TYPE = "run_workbook"
# Every job type that carries a workbook run (queue fencing, receipts, /stop).
WORKBOOK_RUN_JOB_TYPES = (LEGACY_JOB_TYPE, JOB_TYPE)

_ELIGIBLE_COLUMN_TYPES = ("enrichment", "waterfall")


def go_route_active(db) -> bool:
    """True when an operator routed the connector job type to the Go executor."""
    try:
        from sqlalchemy import text

        row = db.execute(
            text("SELECT executor FROM job_executor_routes WHERE job_type = :t"),
            {"t": JOB_TYPE},
        ).first()
    except Exception:  # table missing (SQLite dev) or a read error: stay on Python
        logger.debug("connector route lookup failed; using %s", LEGACY_JOB_TYPE, exc_info=True)
        try:
            db.rollback()
        except Exception:
            pass
        return False
    return bool(row) and row[0] == "go"


def _explicit_chain(column: dict) -> Optional[list]:
    """The user-selected provider chain, or None when defaults would apply."""
    waterfall = column.get("waterfall")
    if waterfall is not None:
        if isinstance(waterfall, list) and waterfall and all(isinstance(p, str) and p for p in waterfall):
            return list(waterfall)
        return None
    provider = column.get("provider")
    if isinstance(provider, str) and provider:
        return [provider]
    return None


_ENV_REF = re.compile(r"\$\{env:([A-Za-z0-9_]+)\}")


def _connector_mismatch(provider) -> Optional[str]:
    """Differences between how Python and the Go host run one connector.

    * Go only loads https connectors (the CI validator requires it) and grants a
      v1 connector the network capability of its endpoint origin.
    * Go resolves only the connector's declared secret (``auth.env_var``), and only
      from the process environment. Python resolves any ``${env:NAME}`` through
      its settings hook, then the environment.
    """
    manifest = provider.manifest
    if not manifest.request.url.startswith("https://"):
        return "non_https_connector"
    declared = manifest.auth.env_var if manifest.auth.type != "none" else None
    used = set(_ENV_REF.findall(manifest.model_dump_json()))
    if used - ({declared} if declared else set()):
        return "undeclared_env_reference"
    if declared and provider._env(declared) != os.getenv(declared, ""):
        return "secret_not_in_environment"
    return None


def ineligible_reason(columns: Iterable[dict], rows: Iterable[dict]) -> Optional[str]:
    """Why a run cannot use the Go connector job, or None when it can.

    ``columns`` are the columns the run will execute and ``rows`` the hydrated
    rows it covers (``row_execution_data`` dictionaries).
    """
    from apps.api.core.config import settings
    from apps.api.services.leadgen.enrichment.declarative.compiler import DeclarativeProvider
    from apps.api.services.workbook.column_deps import _refs_in
    from apps.api.services.workbook.providers import get_provider

    if getattr(settings, "AUTOMATIONS_ENABLED", False):
        return "automations_enabled"
    if getattr(settings, "PROVENANCE_TRACKING_ENABLED", False):
        return "provenance_tracking_enabled"

    columns = list(columns)
    if not columns:
        return "no_columns"
    for column in columns:
        cid = column.get("id")
        if column.get("type") not in _ELIGIBLE_COLUMN_TYPES:
            return f"column_type:{cid}"
        if column.get("condition"):
            return f"condition:{cid}"
        if _refs_in(column):
            return f"column_references:{cid}"
        target = column.get("target_field") or column.get("lead_field")
        if target == "email" and column.get("verify", True):
            return f"email_verify:{cid}"
        chain = _explicit_chain(column)
        if chain is None:
            return f"default_waterfall:{cid}"
        for name in chain:
            provider = get_provider(name)
            if not isinstance(provider, DeclarativeProvider):
                return f"not_a_connector:{name}"
            reason = _connector_mismatch(provider)
            if reason:
                return f"{reason}:{name}"

    for row in rows:
        if row.get("__row_id") is None:
            return "legacy_lead_rows"
        if row.get("__lead_id") is not None:
            return "linked_lead_rows"
    return None


def select_job_type(db, columns: Iterable[dict], rows: Iterable[dict]) -> str:
    """``run_workbook_connector`` only when routed to Go and the run qualifies."""
    if not go_route_active(db):
        return LEGACY_JOB_TYPE
    reason = ineligible_reason(columns, rows)
    if reason is not None:
        logger.info("workbook run stays on %s: %s", LEGACY_JOB_TYPE, reason)
        return LEGACY_JOB_TYPE
    return JOB_TYPE
