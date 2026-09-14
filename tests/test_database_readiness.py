from apps.api.services.database_readiness import evaluate_database_readiness


POLICY = {
    "permissive": "PERMISSIVE",
    "qual": "workspace_id = current_setting('app.workspace_id', true)",
    "with_check": "workspace_id = current_setting('app.workspace_id', true)",
}


def _evaluate(**changes):
    values = {
        "code_heads": ["head"],
        "database_heads": ["head"],
        "role": {"current_user": "opengtm_app", "rolsuper": False, "rolbypassrls": False},
        "tenant_tables": ["leads", "jobs"],
        "table_security": {"leads": (True, True), "jobs": (False, False)},
        "policies": {"leads": [POLICY]},
    }
    values.update(changes)
    return evaluate_database_readiness(**values)


def test_database_readiness_accepts_current_forced_rls_and_known_exemptions():
    result = _evaluate()
    assert result["eligible"] is True
    assert result["tenant_tables_checked"] == 1
    assert result["exempt_tables_present"] == ["jobs"]


def test_database_readiness_fails_on_heads_role_or_rls_drift():
    assert "multiple_code_heads" in _evaluate(code_heads=["a", "b"])["reason_codes"]
    assert "migration_head_mismatch" in _evaluate(database_heads=["old"])["reason_codes"]
    assert "unsafe_runtime_role" in _evaluate(role={
        "current_user": "postgres", "rolsuper": True, "rolbypassrls": False,
    })["reason_codes"]
    result = _evaluate(
        tenant_tables=["leads", "new_tenant_table"],
        table_security={"leads": (True, True), "new_tenant_table": (True, False)},
        policies={"leads": [POLICY], "new_tenant_table": []},
    )
    assert result["eligible"] is False
    assert result["violations"]["new_tenant_table"] == [
        "rls_not_forced", "workspace_policy_invalid",
    ]


def test_database_readiness_rejects_extra_or_unscoped_policy():
    unscoped = {**POLICY, "qual": "true"}
    assert "tenant_rls_invalid" in _evaluate(
        policies={"leads": [POLICY, unscoped]},
    )["reason_codes"]
