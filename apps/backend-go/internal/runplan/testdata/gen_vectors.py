"""Generate runplan Python vectors (eligibility matrix + payload golden).

Run from legacy/python:  PYTHONPATH=. uv run python ../../apps/backend-go/internal/runplan/testdata/gen_vectors.py
Writes eligibility_vectors.json, payload_golden.json and manifests/*.yaml next to this file.
Never hand-edit the outputs: they are Python's answers (go_executor.go_eligibility / build_go_payload).
"""
import json
import pathlib
import shutil
import types

from apps.api.services import go_executor as g
from apps.api.services.leadgen.enrichment.declarative.manifest import MANIFESTS_DIR, ProviderManifest

HERE = pathlib.Path(__file__).resolve().parent

# ---- synthetic manifests for the eligibility matrix -------------------------------------------------
def m(name, capability, mappings, auth=None, url="https://api.example.com/x", headers=None):
    return {
        "name": name, "capability": capability,
        "auth": auth or {"type": "header", "param": "X-Key", "env_var": "KEY_" + name.upper()},
        "request": {"method": "GET", "url": url, "headers": headers or {}},
        "response": {"mappings": mappings},
    }

MANIFESTS = {
    "m_email": m("m_email", "email", {"email": "$.email"}),
    "m_desc": m("m_desc", "description", {"description": "$.d"}),
    "m_two": m("m_two", "email", {"email": "$.e", "phone": "$.p"}),
    "m_extra_env": m("m_extra_env", "email", {"email": "$.e"}, headers={"X-O": "${env:OTHER_KEY}"}),
    "m_tmpl_auth": m("m_tmpl_auth", "email", {"email": "$.e"}, url="https://{{input.domain}}.x.io/y"),
    "m_literal": m("m_literal", "email", {"email": "$.e"},
                   auth={"type": "header", "param": "X-Key", "value": "hunter2", "env_var": "KEY_L"}),
    "m_noauth": m("m_noauth", "company_size", {"company_size": "$.s"}, auth={"type": "none"}),
}
INDEX = {n: (ProviderManifest(**d), "sha-" + n) for n, d in MANIFESTS.items()}

def col(**kw):
    return kw

CASES = [
    ("ok_enrichment", {}, [col(id="a", type="enrichment", provider="m_desc", target_field="description")]),
    ("ok_waterfall", {}, [col(id="a", type="waterfall", waterfall=["m_desc"], target_field="description")]),
    ("ok_email_noverify", {}, [col(id="a", type="enrichment", provider="m_email", target_field="email", verify=False)]),
    ("email_verify_default", {}, [col(id="a", type="enrichment", provider="m_email", target_field="email")]),
    ("email_verify_true", {}, [col(id="a", type="enrichment", provider="m_email", target_field="email", verify=True)]),
    ("flag_off", {"enabled": False}, [col(id="a", type="enrichment", provider="m_desc", target_field="description")]),
    ("not_in_cohort", {"cohort": False}, [col(id="a", type="enrichment", provider="m_desc", target_field="description")]),
    ("provenance_on", {"provenance": True}, [col(id="a", type="enrichment", provider="m_desc", target_field="description")]),
    ("automations_on", {"automations": True}, [col(id="a", type="enrichment", provider="m_desc", target_field="description")]),
    ("no_columns", {"column_ids": ["zzz"]}, [col(id="a", type="enrichment", provider="m_desc", target_field="description")]),
    ("column_type_formula", {}, [col(id="a", type="formula", formula="1")]),
    ("column_type_ai", {}, [col(id="a", type="ai_formula", prompt="x")]),
    ("condition", {}, [col(id="a", type="enrichment", provider="m_desc", target_field="description", condition="{x} != ''")]),
    ("refs_prompt", {}, [col(id="a", type="enrichment", provider="m_desc", target_field="description", prompt="hi {company}")]),
    ("refs_input_columns", {}, [col(id="a", type="enrichment", provider="m_desc", target_field="description", input_columns=["company"])]),
    ("refs_blank_input_columns", {}, [col(id="a", type="enrichment", provider="m_desc", target_field="description", input_columns=["  "])]),
    ("refs_formula_key", {}, [col(id="a", type="enrichment", provider="m_desc", target_field="description", formula="{a}")]),
    ("structured_target", {}, [col(id="a", type="enrichment", provider="m_desc", target_field="decision_makers")]),
    ("empty_chain", {}, [col(id="a", type="waterfall", waterfall=[], target_field="description")]),
    ("default_chain_nondecl", {}, [col(id="a", type="enrichment", target_field="description")]),
    ("nondecl_provider", {}, [col(id="a", type="enrichment", provider="hunter_io", target_field="email", verify=False)]),
    ("mapping_not_target_only", {}, [col(id="a", type="enrichment", provider="m_two", target_field="email", verify=False)]),
    ("templated_authority", {}, [col(id="a", type="enrichment", provider="m_tmpl_auth", target_field="email", verify=False)]),
    ("literal_auth", {}, [col(id="a", type="enrichment", provider="m_literal", target_field="email", verify=False)]),
    ("extra_env", {}, [col(id="a", type="enrichment", provider="m_extra_env", target_field="email", verify=False)]),
    ("noauth_ok", {}, [col(id="a", type="enrichment", provider="m_noauth", target_field="company_size")]),
    ("target_from_lead_field", {}, [col(id="a", type="enrichment", provider="m_desc", lead_field="description")]),
    ("target_from_id", {}, [col(id="description", type="enrichment", provider="m_desc")]),
    ("second_col_fails", {}, [col(id="a", type="enrichment", provider="m_desc", target_field="description"),
                              col(id="b", type="enrichment", provider="hunter_io", target_field="description")]),
    ("selected_subset", {"column_ids": ["a"]}, [col(id="a", type="enrichment", provider="m_desc", target_field="description"),
                                                col(id="b", type="formula", formula="1")]),
    ("provider_waterfall_precedence", {}, [col(id="a", type="waterfall", waterfall=["m_desc"], provider="hunter_io", target_field="description")]),
    ("provider_zero_waterfall_null", {}, [col(id="a", type="enrichment", waterfall=None, provider="m_desc", target_field="description")]),
]

def run_case(flags, columns):
    settings = types.SimpleNamespace(
        GO_EXECUTOR_ENABLED=flags.get("enabled", True),
        GO_EXECUTOR_WORKSPACES="ws-1,ws-2" if flags.get("cohort", True) else "ws-9",
        PROVENANCE_TRACKING_ENABLED=flags.get("provenance", False),
        AUTOMATIONS_ENABLED=flags.get("automations", False),
    )
    g.get_settings = lambda: settings
    g.load_manifest_index = lambda: INDEX
    wb = types.SimpleNamespace(columns_config=columns)
    payload = {"workspace_id": "ws-1"}
    if "column_ids" in flags:
        payload["column_ids"] = flags["column_ids"]
    else:
        payload["column_ids"] = [c["id"] for c in columns]
    return g.go_eligibility(None, wb, payload), payload

vectors = []
for name, flags, columns in CASES:
    (ok, reason), payload = run_case(flags, columns)
    vectors.append({"name": name, "flags": flags, "columns": columns, "workspace_id": "ws-1",
                    "column_ids": payload["column_ids"], "ok": ok, "reason": reason})
(HERE / "eligibility_vectors.json").write_text(json.dumps({"manifests": MANIFESTS, "cases": vectors}, indent=1) + "\n")

# ---- payload golden over the real, signed manifests -------------------------------------------------
import importlib
importlib.reload(g)  # restore the real get_settings / load_manifest_index
g.load_manifest_index.cache_clear()
(HERE / "manifests").mkdir(exist_ok=True)
for p in sorted(MANIFESTS_DIR.rglob("*.yaml")):
    shutil.copy(p, HERE / "manifests" / p.name)
columns = [
    {"id": "em", "type": "enrichment", "provider": "leadmagic_email", "target_field": "email", "verify": False},
    {"id": "mob", "type": "waterfall", "waterfall": ["prospeo_mobile", "leadmagic_mobile", "leadmagic_email"],
     "target_field": "mobile_phone"},
    {"id": "cs", "type": "enrichment", "provider": "leadmagic_company", "target_field": "company_size"},
    {"id": "other", "type": "formula", "formula": "1"},
]
wb = types.SimpleNamespace(columns_config=columns)
golden = {"columns": columns, "column_ids": ["em", "mob", "cs"],
          "payload": g.build_go_payload(wb, {"column_ids": ["em", "mob", "cs"]}),
          "sha256": {n: e[1] for n, e in g.load_manifest_index().items()}}
(HERE / "payload_golden.json").write_text(json.dumps(golden, indent=1) + "\n")
print(len(vectors), "cases;", sorted(golden["payload"]["manifests"]))
