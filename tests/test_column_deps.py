"""
Column dependency ordering (Phase 4, PR C): topo-sort by {col} references.
"""
from apps.api.services.workbook.column_deps import downstream_columns, topo_sort_columns, _refs_in


def _ids(cols):
    return [c["id"] for c in cols]


def test_no_deps_preserves_order():
    cols = [{"id": "a", "type": "formula", "formula": "1"},
            {"id": "b", "type": "formula", "formula": "2"}]
    assert _ids(topo_sort_columns(cols)) == ["a", "b"]


def test_dependent_runs_after_input():
    # 'domain' formula references {email}; 'email' must come first
    cols = [
        {"id": "domain", "name": "Domain", "type": "formula", "formula": '{email}.split("@")[1]'},
        {"id": "email", "name": "Email", "type": "waterfall", "target_field": "email"},
    ]
    out = _ids(topo_sort_columns(cols))
    assert out.index("email") < out.index("domain")


def test_chain_of_three():
    cols = [
        {"id": "c", "type": "formula", "formula": "upper({b})"},
        {"id": "b", "type": "formula", "formula": "{a} + \"!\""},
        {"id": "a", "type": "ai_formula", "prompt": "describe {company}"},
    ]
    out = _ids(topo_sort_columns(cols))
    assert out.index("a") < out.index("b") < out.index("c")


def test_reference_by_display_name():
    cols = [
        {"id": "col1", "name": "Email", "type": "waterfall", "target_field": "email"},
        {"id": "col2", "name": "Domain", "type": "formula", "formula": '{Email}.split("@")[1]'},
    ]
    out = _ids(topo_sort_columns(cols))
    assert out.index("col1") < out.index("col2")


def test_http_url_and_headers_refs():
    cols = [
        {"id": "key", "type": "ai_formula", "prompt": "x"},
        {"id": "call", "type": "http", "http_url": "https://api/x?k={key}",
         "http_headers": {"Authorization": "{key}"}},
    ]
    out = _ids(topo_sort_columns(cols))
    assert out.index("key") < out.index("call")


def test_cycle_falls_back_to_config_order():
    cols = [
        {"id": "a", "type": "formula", "formula": "{b}"},
        {"id": "b", "type": "formula", "formula": "{a}"},
    ]
    # no crash, returns config order
    assert _ids(topo_sort_columns(cols)) == ["a", "b"]


def test_refs_extraction():
    col = {"type": "formula", "formula": 'upper({first}) + {last}'}
    assert _refs_in(col) == {"first", "last"}
    # lead_field placeholders (not other columns) are still extracted; the sorter
    # ignores refs that don't match a column id/name
    assert _refs_in({"type": "http", "http_url": "https://x/{domain}"}) == {"domain"}
    assert _refs_in({"type": "ai_formula", "input_columns": ["company", "domain"]}) == {"company", "domain"}


def test_transitive_downstream_columns_follow_aliases_in_order():
    cols = [
        {"id": "company", "name": "Company", "type": "input", "lead_field": "company"},
        {"id": "research", "name": "Research", "type": "research", "goal": "Research {Company}"},
        {"id": "score", "name": "Score", "type": "formula", "formula": "len({research})"},
        {"id": "push", "type": "output", "destination_config": {"body": {"score": "{Score}"}}},
        {"id": "unrelated", "type": "formula", "formula": "1 + 1"},
    ]
    assert _ids(downstream_columns(cols, {"company"})) == ["research", "score", "push"]


def test_downstream_fields_are_case_insensitive():
    cols = [{"id": "summary", "type": "ai_formula", "prompt": "Summarize {Company Name}"}]
    assert _ids(downstream_columns(cols, {"company name"})) == ["summary"]


def test_ineligible_dependency_stops_reactive_propagation():
    cols = [
        {"id": "research", "type": "research", "goal": "{company}", "reactive": False},
        {"id": "score", "type": "formula", "formula": "len({research})"},
    ]
    eligible = lambda column: column.get("reactive", True)
    assert downstream_columns(cols, {"company"}, eligible=eligible) == []
