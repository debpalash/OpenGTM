"""Deployment-wide operational telemetry for platform administrators."""

import os
from pathlib import Path

from fastapi import APIRouter, Depends
from sqlalchemy.orm import Session

from apps.api.core.security import get_current_admin_user
from apps.api.database import get_db
from apps.api.models import User
from apps.api.services.queue_service import queue_service

router = APIRouter(prefix="/admin/operations", tags=["operations"])
GAUNTLET_ARTIFACT_ENV = "OPENGTM_GAUNTLET_ARTIFACT"


@router.get("/queue")
def queue_metrics(
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_admin_user),
):
    _ = current_user
    return queue_service.metrics(db)


def _release_readiness() -> dict:
    from apps.api.services.evaluation.gtm_gauntlet import load_artifact, score_gauntlet
    from apps.api.services.integrations.certification import (
        BUILD_SHA_ENV,
        agent_capability_catalog,
        certification_statuses,
        governance_capability_catalog,
        integration_catalog,
        signal_source_catalog,
    )
    from apps.api.services.leadgen.enrichment.declarative.manifest import (
        load_all_manifests,
        validate_manifest_directory,
    )
    from apps.api.services.workbook.providers import list_providers

    groups = {
        "integrations": integration_catalog(),
        "signals": signal_source_catalog(),
        "agents": agent_capability_catalog(),
        "governance": governance_capability_catalog(),
    }
    missing = [
        f"{group}:{item['id']}"
        for group, items in groups.items()
        for item in items
        if item.get("maturity") != "supported"
    ]

    review = validate_manifest_directory()
    reviewed = {item["id"]: item for item in review.get("connectors", [])}
    connector_subjects = [
        f"connector:{manifest.name}" for manifest in load_all_manifests()
    ]
    connector_status = certification_statuses(
        connector_subjects,
        subject_builds={
            f"connector:{name}": str(item.get("manifest_sha256") or "")
            for name, item in reviewed.items()
        },
    )
    connector_missing = [
        subject for subject in connector_subjects
        if connector_status[subject]["maturity"] != "supported"
    ]
    provider_subjects = [
        f"provider:{provider['name']}" for provider in list_providers()
    ]
    provider_status = certification_statuses(provider_subjects)
    provider_missing = [
        subject for subject in provider_subjects
        if provider_status[subject]["maturity"] != "supported"
    ]

    artifact_path = os.getenv(GAUNTLET_ARTIFACT_ENV, "")
    deployed_build_sha = os.getenv(BUILD_SHA_ENV, "")
    gauntlet = {
        "eligible": False,
        "reason_codes": ["artifact_missing"],
        "consecutive_production_like_passes": 0,
        "required_consecutive_production_like_passes": 10,
    }
    if artifact_path:
        try:
            report = score_gauntlet(load_artifact(Path(artifact_path)))
            gauntlet = dict(report["release"])
            gauntlet_build_sha = str(report.get("build_sha") or "")
            build_matches = bool(
                deployed_build_sha and gauntlet_build_sha == deployed_build_sha
            )
            gauntlet["build_sha"] = gauntlet_build_sha
            gauntlet["deployed_build_sha"] = deployed_build_sha
            gauntlet["build_matches_deployment"] = build_matches
            if not build_matches:
                gauntlet["eligible"] = False
                gauntlet["reason_codes"] = list(dict.fromkeys([
                    *gauntlet.get("reason_codes", []),
                    "build_mismatch",
                ]))
        except (OSError, ValueError, KeyError, TypeError):
            gauntlet["reason_codes"] = ["artifact_invalid"]

    required_count = sum(len(items) for items in groups.values())
    return {
        "eligible": not missing and gauntlet.get("eligible") is True,
        "deployed_build_sha": deployed_build_sha,
        "first_party": {
            "required": required_count,
            "supported": required_count - len(missing),
            "missing": missing,
        },
        "community_connectors": {
            "total": len(connector_subjects),
            "supported": len(connector_subjects) - len(connector_missing),
            "missing": connector_missing,
            "manifest_review_ok": review.get("ok") is True,
        },
        "enrichment_providers": {
            "total": len(provider_subjects),
            "supported": len(provider_subjects) - len(provider_missing),
            "missing": provider_missing,
        },
        "gauntlet": gauntlet,
    }


@router.get("/release-readiness")
def release_readiness(
    current_user: User = Depends(get_current_admin_user),
):
    """Aggregate fail-closed live certification and gauntlet release gates."""
    _ = current_user
    return _release_readiness()
