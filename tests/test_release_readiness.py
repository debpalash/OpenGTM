from apps.api.routers import operations


def _items(*ids, maturity="supported"):
    return [{"id": item_id, "maturity": maturity} for item_id in ids]


def _patch_catalogs(monkeypatch, *, maturity="supported"):
    import apps.api.services.database_readiness as database
    import apps.api.services.integrations.certification as certification
    import apps.api.services.leadgen.enrichment.declarative.manifest as manifests
    import apps.api.services.workbook.providers as providers
    import apps.api.services.scale_certification as scale

    monkeypatch.setattr(certification, "integration_catalog", lambda: _items("hubspot", maturity=maturity))
    monkeypatch.setattr(certification, "signal_source_catalog", lambda: _items("jobspy", maturity=maturity))
    monkeypatch.setattr(certification, "agent_capability_catalog", lambda: _items("grounded_research", maturity=maturity))
    monkeypatch.setattr(certification, "governance_capability_catalog", lambda: _items("oidc_sso", maturity=maturity))
    monkeypatch.setattr(certification, "certification_statuses", lambda subjects, **kwargs: {
        subject: {"maturity": maturity} for subject in subjects
    })
    monkeypatch.setattr(manifests, "load_all_manifests", lambda: [type("Manifest", (), {"name": "sample"})()])
    monkeypatch.setattr(manifests, "validate_manifest_directory", lambda: {
        "ok": True,
        "connectors": [{"id": "sample", "manifest_sha256": "a" * 64}],
    })
    monkeypatch.setattr(providers, "list_providers", lambda: [
        {"name": "hunter_io", "capabilities": ["email"]},
    ])
    monkeypatch.setattr(scale, "scale_report_valid", lambda report, key, build: (
        report.get("build_sha") == build and bool(key)
    ))
    monkeypatch.setattr(database, "database_readiness", lambda: {
        "eligible": True,
        "reason_codes": [],
    })


def test_readiness_fails_closed_without_live_artifact(monkeypatch):
    _patch_catalogs(monkeypatch, maturity="beta")
    monkeypatch.delenv(operations.GAUNTLET_ARTIFACT_ENV, raising=False)
    monkeypatch.delenv("OPENGTM_BUILD_SHA", raising=False)
    monkeypatch.delenv(operations.WORKBOOK_SCALE_REPORT_ENV, raising=False)
    monkeypatch.delenv(operations.QUEUE_SCALE_REPORT_ENV, raising=False)

    result = operations._release_readiness()

    assert result["eligible"] is False
    assert result["first_party"] == {
        "required": 4,
        "supported": 0,
        "missing": [
            "integrations:hubspot", "signals:jobspy",
            "agents:grounded_research", "governance:oidc_sso",
        ],
    }
    assert result["gauntlet"]["reason_codes"] == ["artifact_missing"]
    assert result["community_connectors"]["missing"] == ["connector:sample"]
    assert result["enrichment_providers"]["missing"] == ["provider:hunter_io"]
    assert result["scale"]["eligible"] is False
    assert result["parity"]["eligible"] is False
    assert "first_party:integrations:hubspot" in result["parity"]["blockers"]
    assert "community_connectors:connector:sample" in result["parity"]["blockers"]
    assert "enrichment_providers:provider:hunter_io" in result["parity"]["blockers"]


def _configure_scale(tmp_path, monkeypatch, build_sha="release-build"):
    monkeypatch.setenv("OPENGTM_SCALE_ATTESTATION_KEY", "scale-key")
    for gate, environment in (
        ("workbook_scale", operations.WORKBOOK_SCALE_REPORT_ENV),
        ("queue_scale", operations.QUEUE_SCALE_REPORT_ENV),
    ):
        path = tmp_path / f"{gate}.json"
        path.write_text(
            __import__("json").dumps({
                "gate": gate, "build_sha": build_sha,
                "run_id": f"{gate}-1", "finished_at": "2026-09-12T00:00:00Z",
            }),
            encoding="utf-8",
        )
        monkeypatch.setenv(environment, str(path))


def test_readiness_requires_both_certifications_and_gauntlet(tmp_path, monkeypatch):
    _patch_catalogs(monkeypatch)
    artifact = tmp_path / "live.json"
    artifact.write_text("{}", encoding="utf-8")
    monkeypatch.setenv(operations.GAUNTLET_ARTIFACT_ENV, str(artifact))
    monkeypatch.setenv("OPENGTM_BUILD_SHA", "release-build")
    _configure_scale(tmp_path, monkeypatch)
    import apps.api.services.evaluation.gtm_gauntlet as gauntlet
    monkeypatch.setattr(gauntlet, "load_artifact", lambda path: {"path": str(path)})
    monkeypatch.setattr(gauntlet, "score_gauntlet", lambda value: {"build_sha": "release-build", "release": {
        "eligible": True,
        "reason_codes": [],
        "consecutive_production_like_passes": 10,
        "required_consecutive_production_like_passes": 10,
    }})

    result = operations._release_readiness()

    assert result["eligible"] is True
    assert result["first_party"] == {"required": 4, "supported": 4, "missing": []}
    assert result["community_connectors"]["supported"] == 1
    assert result["enrichment_providers"]["supported"] == 1
    assert result["gauntlet"]["consecutive_production_like_passes"] == 10
    assert result["gauntlet"]["build_matches_deployment"] is True
    assert result["scale"]["eligible"] is True
    assert result["parity"] == {"eligible": True, "blockers": []}


def test_readiness_rejects_gauntlet_from_another_build(tmp_path, monkeypatch):
    _patch_catalogs(monkeypatch)
    artifact = tmp_path / "live.json"
    artifact.write_text("{}", encoding="utf-8")
    monkeypatch.setenv(operations.GAUNTLET_ARTIFACT_ENV, str(artifact))
    monkeypatch.setenv("OPENGTM_BUILD_SHA", "deployed-build")
    _configure_scale(tmp_path, monkeypatch, build_sha="deployed-build")
    import apps.api.services.evaluation.gtm_gauntlet as gauntlet
    monkeypatch.setattr(gauntlet, "load_artifact", lambda path: {})
    monkeypatch.setattr(gauntlet, "score_gauntlet", lambda value: {
        "build_sha": "other-build",
        "release": {
            "eligible": True,
            "reason_codes": [],
            "consecutive_production_like_passes": 10,
            "required_consecutive_production_like_passes": 10,
        },
    })

    result = operations._release_readiness()

    assert result["eligible"] is False
    assert result["gauntlet"]["build_matches_deployment"] is False
    assert result["gauntlet"]["reason_codes"] == ["build_mismatch"]


def test_readiness_requires_both_distinct_scale_gates(tmp_path, monkeypatch):
    _patch_catalogs(monkeypatch)
    monkeypatch.setenv("OPENGTM_BUILD_SHA", "release-build")
    monkeypatch.setenv("OPENGTM_SCALE_ATTESTATION_KEY", "scale-key")
    workbook = tmp_path / "workbook.json"
    workbook.write_text('{"gate":"workbook_scale","build_sha":"release-build"}', encoding="utf-8")
    monkeypatch.setenv(operations.WORKBOOK_SCALE_REPORT_ENV, str(workbook))
    monkeypatch.setenv(operations.QUEUE_SCALE_REPORT_ENV, str(workbook))
    monkeypatch.delenv(operations.GAUNTLET_ARTIFACT_ENV, raising=False)

    result = operations._release_readiness()

    assert result["scale"]["gates"]["workbook_scale"]["valid"] is True
    assert result["scale"]["gates"]["queue_scale"]["valid"] is False
    assert result["eligible"] is False


def test_readiness_fails_closed_when_database_is_not_release_ready(
    tmp_path, monkeypatch,
):
    _patch_catalogs(monkeypatch)
    monkeypatch.setenv("OPENGTM_BUILD_SHA", "release-build")
    _configure_scale(tmp_path, monkeypatch)
    monkeypatch.delenv(operations.GAUNTLET_ARTIFACT_ENV, raising=False)
    import apps.api.services.database_readiness as database
    monkeypatch.setattr(database, "database_readiness", lambda: {
        "eligible": False,
        "reason_codes": ["tenant_rls_invalid"],
        "violations": {"new_table": ["rls_not_forced"]},
    })

    result = operations._release_readiness()

    assert result["eligible"] is False
    assert result["database"]["reason_codes"] == ["tenant_rls_invalid"]
    assert "database:tenant_rls_invalid" in result["parity"]["blockers"]


def test_core_release_can_pass_while_strict_parity_reports_catalog_gaps(
    tmp_path, monkeypatch,
):
    _patch_catalogs(monkeypatch)
    import apps.api.services.integrations.certification as certification
    monkeypatch.setattr(certification, "certification_statuses", lambda subjects, **kwargs: {
        subject: {
            "maturity": "beta" if subject.startswith("provider:") else "supported",
        }
        for subject in subjects
    })
    artifact = tmp_path / "live.json"
    artifact.write_text("{}", encoding="utf-8")
    monkeypatch.setenv(operations.GAUNTLET_ARTIFACT_ENV, str(artifact))
    monkeypatch.setenv("OPENGTM_BUILD_SHA", "release-build")
    _configure_scale(tmp_path, monkeypatch)
    import apps.api.services.evaluation.gtm_gauntlet as gauntlet
    monkeypatch.setattr(gauntlet, "load_artifact", lambda path: {})
    monkeypatch.setattr(gauntlet, "score_gauntlet", lambda value: {
        "build_sha": "release-build",
        "release": {"eligible": True, "reason_codes": []},
    })

    result = operations._release_readiness()

    assert result["eligible"] is True
    assert result["parity"] == {
        "eligible": False,
        "blockers": ["enrichment_providers:provider:hunter_io"],
    }
