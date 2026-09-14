from apps.api.routers import operations


def _items(*ids, maturity="supported"):
    return [{"id": item_id, "maturity": maturity} for item_id in ids]


def _patch_catalogs(monkeypatch, *, maturity="supported"):
    import apps.api.services.integrations.certification as certification
    import apps.api.services.leadgen.enrichment.declarative.manifest as manifests

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


def test_readiness_fails_closed_without_live_artifact(monkeypatch):
    _patch_catalogs(monkeypatch, maturity="beta")
    monkeypatch.delenv(operations.GAUNTLET_ARTIFACT_ENV, raising=False)
    monkeypatch.delenv("OPENGTM_BUILD_SHA", raising=False)

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


def test_readiness_requires_both_certifications_and_gauntlet(tmp_path, monkeypatch):
    _patch_catalogs(monkeypatch)
    artifact = tmp_path / "live.json"
    artifact.write_text("{}", encoding="utf-8")
    monkeypatch.setenv(operations.GAUNTLET_ARTIFACT_ENV, str(artifact))
    monkeypatch.setenv("OPENGTM_BUILD_SHA", "release-build")
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
    assert result["gauntlet"]["consecutive_production_like_passes"] == 10
    assert result["gauntlet"]["build_matches_deployment"] is True


def test_readiness_rejects_gauntlet_from_another_build(tmp_path, monkeypatch):
    _patch_catalogs(monkeypatch)
    artifact = tmp_path / "live.json"
    artifact.write_text("{}", encoding="utf-8")
    monkeypatch.setenv(operations.GAUNTLET_ARTIFACT_ENV, str(artifact))
    monkeypatch.setenv("OPENGTM_BUILD_SHA", "deployed-build")
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
