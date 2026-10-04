"""Build the manifest v1 parity corpus and record the Python verdicts.

Run from the repository root:

    uv run python apps/server/internal/plugin/manifest/testdata/gen_v1_parity.py

It (re)writes testdata/v1/cases/<case>/... (valid and deliberately invalid
connector manifests, some signed with a throwaway test-only Ed25519 key) and
testdata/v1/expected.json, which holds, per case, the exact report produced by
validate_manifest_directory() plus the pydantic error locations for schema
failures. It also records the report for the bundled production manifests.
The Go test (parity_v1_test.go) asserts the same decisions and messages.
"""

import base64
import hashlib
import json
import shutil
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[5]
sys.path.insert(0, str(ROOT))

from cryptography.hazmat.primitives import serialization  # noqa: E402
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey  # noqa: E402
from pydantic import ValidationError  # noqa: E402

from apps.api.services.leadgen.enrichment.declarative.manifest import (  # noqa: E402
    MANIFESTS_DIR,
    TRUST_STORE,
    load_manifest,
    validate_manifest_directory,
)
from apps.api.services.leadgen.enrichment.declarative.signing import sign_manifest  # noqa: E402

V1 = HERE / "v1"
CASES = V1 / "cases"

# Throwaway, test-only keys derived from fixed seeds (Ed25519 is deterministic,
# so regenerating yields identical signatures). Never trust these anywhere.
TRUSTED_KEY = Ed25519PrivateKey.from_private_bytes(hashlib.sha256(b"opengtm-parity-trusted").digest())
ROGUE_KEY = Ed25519PrivateKey.from_private_bytes(hashlib.sha256(b"opengtm-parity-rogue").digest())

BASE = (
    'manifest_version: "1"\n'
    "name: {name}\n"
    "capability: email\n"
    "request:\n"
    "  url: https://api.example.com/find\n"
    "response:\n"
    "  mappings:\n"
    "    email: $.email\n"
)


def base(name="test_email", **extra):
    text = BASE.format(name=name)
    for key, value in extra.items():
        text += f"{key}: {value}\n"
    return text


FULL = """# A complete connector, like docs/connectors/connector.template.yaml
manifest_version: "1"
name: acme_email
display_name: Acme Email Finder
author: your-github-handle
homepage: https://provider.example
license: Apache-2.0
tags: [email, enrichment, byok]
capability: email
capabilities: [email, email_status]
description: Find a work email from a name and company domain — café ✓.
default_confidence: 0.8
cost_per_lookup: 0.05
auth:
  type: bearer
  env_var: ACME_API_KEY
request:
  method: POST
  url: https://api.provider.example/v1/email/find
  headers:
    Content-Type: application/json
  body:
    first_name: "{{input.first_name}}"
    last_name: "{{input.last_name}}"
    domain: "{{input.domain}}"
  timeout: 20
response:
  error_path: error
  error_message_path: message
  mappings:
    email: "$.data.email"
    email_status: "$.data.status"
input_fields: [first_name, last_name, domain]
"""

LEADMAGIC = (ROOT / "apps/api/services/leadgen/enrichment/declarative/manifests/email/leadmagic.yaml").read_text(encoding="utf-8")

# case name -> {"files": {relpath: text|bytes|None(dir)}, "policy": str, "trust": "store"|"missing"|"rogue", "sign": {...}}
CASES_DEF = {
    # ---- accepted ----
    "valid_minimal": {"files": {"one.yaml": base()}},
    "valid_full": {"files": {"acme.yaml": FULL}},
    "valid_leadmagic_copy": {"files": {"email/leadmagic.yaml": LEADMAGIC}},
    "valid_two_capability_dirs": {"files": {"phone/b.yaml": base("b_phone"), "email/a.yaml": base("a_email")}},
    "valid_method_lowercase": {"files": {"m.yaml": base().replace("  url:", "  method: get\n  url:")}},
    "valid_method_long_s": {"files": {"m.yaml": base().replace("  url:", "  method: poſt\n  url:")}},
    "valid_method_ligature": {"files": {"m.yaml": base().replace("  url:", "  method: poﬅ\n  url:")}},
    "valid_templated_host": {"files": {"t.yaml": base().replace("https://api.example.com/find", "https://{{input.domain}}/find")}},
    "valid_body_and_body_template": {"files": {"b.yaml": base().replace("  url:", "  body: {a: 1}\n  body_template: {b: 2}\n  url:")}},
    "valid_coerced_numbers": {"files": {"c.yaml": base(default_confidence='"0.5"', cost_per_lookup='"1_0"').replace("  url:", "  timeout: ' 30 '\n  url:")}},
    "valid_bool_as_number": {"files": {"c.yaml": base(default_confidence="true", cost_per_lookup="no")}},
    "valid_title_case": {"files": {"t.yaml": base("abc2def_x9y")}},
    "valid_display_name": {"files": {"t.yaml": base(display_name="'  Spaced  '")}},
    "valid_extra_fields_ignored": {"files": {"x.yaml": base(unknown_field="yes", nested="{a: [1, 2.50, null]}")}},
    "valid_yml_and_odd_extensions": {"files": {"a.yml": base("first_one"), "b.yaxml": base("second_one"), "c.yaml.bak": "not: scanned", "notes.txt": "ignored"}},
    "valid_hidden_file": {"files": {".hidden.yaml": base("hidden_one")}},
    "valid_auth_modes": {
        "files": {
            "a.yaml": base("auth_header", auth="{type: header, param: X-Key, env_var: K1}"),
            "b.yaml": base("auth_query", auth="{type: query, env_var: K2, value: '${env:K2}'}"),
            "c.yaml": base("auth_none", auth="{type: none}"),
            "d.yaml": base("auth_bearer", auth="{type: bearer, env_var: K3}"),
        }
    },
    "valid_capabilities_dedupe": {"files": {"d.yaml": base(capabilities="[email, phone, email, '']")}},
    "valid_cost_edges": {"files": {"e.yaml": base(cost_per_lookup="0.10", default_confidence="1")}},
    "valid_cost_nan": {"files": {"e.yaml": base(cost_per_lookup=".nan")}},
    "valid_sort_order": {"files": {"a-b.yaml": base("dash_one"), "a/b.yaml": base("slash_one")}},
    "valid_anchor_merge": {"files": {"m.yaml": 'defaults: &d {author: anchors}\n<<: *d\n' + base("merged_one")}},
    "set_tags_not_canonical": {"files": {"t.yaml": base(tags="!!set {a, b}")}},
    "binary_name_not_canonical": {"files": {"b.yaml": base().replace("name: test_email", "name: !!binary dGVzdF9lbWFpbA==")}},
    # ---- rejected: compatibility rules ----
    "duplicate_ids": {"files": {"one.yaml": base("duplicate_id"), "two.yaml": base("duplicate_id")}},
    "duplicate_after_later_failure": {
        "files": {"a.yaml": base("claimed_id").replace("https://", "http://"), "b.yaml": base("claimed_id")}
    },
    "http_endpoint": {"files": {"h.yaml": base().replace("https://", "http://")}},
    "uppercase_scheme": {"files": {"h.yaml": base().replace("https://", "HTTPS://")}},
    "unknown_auth_mode": {"files": {"a.yaml": base(auth="{type: basic, env_var: K}")}},
    "auth_mode_case": {"files": {"a.yaml": base(auth="{type: Bearer, env_var: K}")}},
    "missing_credential_ref": {"files": {"a.yaml": base(auth="{type: header, param: X-Key}")}},
    "empty_credential_ref": {"files": {"a.yaml": base(auth="{type: bearer, env_var: ''}")}},
    "schema_version_2": {"files": {"v.yaml": base().replace('manifest_version: "1"', 'manifest_version: "2"')}},
    "schema_version_int": {"files": {"v.yaml": base().replace('manifest_version: "1"', "manifest_version: 1")}},
    "schema_version_float": {"files": {"v.yaml": base().replace('manifest_version: "1"', "manifest_version: 1.0")}},
    "method_delete": {"files": {"m.yaml": base().replace("  url:", "  method: DELETE\n  url:")}},
    "method_int": {"files": {"m.yaml": base().replace("  url:", "  method: 5\n  url:")}},
    "empty_mappings": {"files": {"m.yaml": base().replace("  mappings:\n    email: $.email\n", "  mappings: {}\n")}},
    "missing_mappings": {"files": {"m.yaml": base().replace("  mappings:\n    email: $.email\n", "  error_path: e\n")}},
    "missing_response": {"files": {"m.yaml": base().replace("response:\n  mappings:\n    email: $.email\n", "")}},
    "name_too_short": {"files": {"n.yaml": base("ab")}},
    "name_uppercase": {"files": {"n.yaml": base("Acme_email")}},
    "name_dash": {"files": {"n.yaml": base("acme-email")}},
    "name_too_long": {"files": {"n.yaml": base("a" * 65)}},
    "confidence_high": {"files": {"c.yaml": base(default_confidence="1.5")}},
    "confidence_nan": {"files": {"c.yaml": base(default_confidence="'nan'")}},
    "cost_negative": {"files": {"c.yaml": base(cost_per_lookup="-0.01")}},
    "timeout_zero": {"files": {"t.yaml": base().replace("  url:", "  timeout: 0\n  url:")}},
    "timeout_high": {"files": {"t.yaml": base().replace("  url:", "  timeout: 120.5\n  url:")}},
    "timeout_max_ok_then_dup": {"files": {"t.yaml": base("t_max").replace("  url:", "  timeout: 120\n  url:")}},
    # ---- rejected: schema (pydantic) ----
    "missing_name": {"files": {"n.yaml": base().replace("name: test_email\n", "")}},
    "missing_capability": {"files": {"n.yaml": base().replace("capability: email\n", "")}},
    "missing_request": {"files": {"r.yaml": base().replace("request:\n  url: https://api.example.com/find\n", "")}},
    "request_null": {"files": {"r.yaml": base().replace("request:\n  url: https://api.example.com/find\n", "request: null\n")}},
    "request_string": {"files": {"r.yaml": base().replace("request:\n  url: https://api.example.com/find\n", "request: abc\n")}},
    "request_int": {"files": {"r.yaml": base().replace("request:\n  url: https://api.example.com/find\n", "request: 5\n")}},
    "request_pairs": {"files": {"r.yaml": base().replace("request:\n  url: https://api.example.com/find\n", "request: [[url, 'https://api.example.com/x']]\n")}},
    "request_bad_pairs": {"files": {"r.yaml": base().replace("request:\n  url: https://api.example.com/find\n", "request: [[1, 2, 3]]\n")}},
    "name_int": {"files": {"n.yaml": base().replace("name: test_email", "name: 123")}},
    "name_date": {"files": {"n.yaml": base().replace("name: test_email", "name: 2026-01-01")}},
    "tags_string": {"files": {"t.yaml": base(tags="email")}},
    "tags_int_item": {"files": {"t.yaml": base(tags="[a, 1]")}},
    "mapping_int_value": {"files": {"m.yaml": base().replace("    email: $.email", "    email: 5")}},
    "mapping_int_key": {"files": {"m.yaml": base().replace("    email: $.email", "    1: $.email")}},
    "headers_null": {"files": {"h.yaml": base().replace("  url:", "  headers:\n  url:")}},
    "headers_int_key": {"files": {"h.yaml": base().replace("  url:", "  headers: {1: x}\n  url:")}},
    "auth_null": {"files": {"a.yaml": base(auth="null")}},
    "auth_string": {"files": {"a.yaml": base(auth="bearer")}},
    "auth_param_int": {"files": {"a.yaml": base(auth="{type: header, param: 5, env_var: K}")}},
    "confidence_string": {"files": {"c.yaml": base(default_confidence="high")}},
    "confidence_null": {"files": {"c.yaml": base(default_confidence="~")}},
    "cost_hex_string": {"files": {"c.yaml": base(cost_per_lookup="'0x10'")}},
    "cost_huge_int": {"files": {"c.yaml": base(cost_per_lookup="1" + "0" * 400)}},
    "timeout_string": {"files": {"t.yaml": base().replace("  url:", "  timeout: soon\n  url:")}},
    "multiple_schema_errors": {"files": {"m.yaml": "name: 1\ncapability: [x]\nrequest: {timeout: x}\nresponse: {mappings: []}\n"}},
    "homepage_null": {"files": {"h.yaml": base(homepage="~")}},
    "error_path_int": {"files": {"e.yaml": base().replace("  mappings:", "  error_path: 3\n  mappings:")}},
    # ---- rejected: document shape ----
    "top_level_list": {"files": {"l.yaml": "- a\n- b\n"}},
    "empty_file": {"files": {"e.yaml": ""}},
    "scalar_file": {"files": {"s.yaml": "just text\n"}},
    "invalid_yaml": {"files": {"i.yaml": "name: [unclosed\n"}},
    "two_documents": {"files": {"d.yaml": base() + "---\nname: other\n"}},
    "int_top_level_key": {"files": {"k.yaml": base() + "1: x\n"}},
    "unknown_tag": {"files": {"u.yaml": base(extra="!custom x")}},
    "value_tag": {"files": {"u.yaml": base(extra="=")}},
    "date_in_unknown_field": {"files": {"d.yaml": base(released="2026-01-01")}},
    "binary_in_unknown_field": {"files": {"d.yaml": base(blob="!!binary aGk=")}},
    "mixed_key_types_nested": {"files": {"d.yaml": base(extra="{1: a, b: c}")}},
    "nonprintable_char": {"files": {"n.yaml": base(description='"\x7f"')}},
    "directory_named_yaml": {"files": {"dir.yaml/": None}},
    "invalid_utf8": {"files": {"bad.yaml": b'manifest_version: "1"\nname: \xff\xfe\n'}},
    "mixed_good_and_bad": {"files": {"good.yaml": base("good_one"), "bad.yaml": base("bad_one").replace("https://", "http://")}},
    # ---- signatures ----
    "signed_trusted_optional": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}},
    "signed_trusted_required": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "policy": "required"},
    "unsigned_required": {"files": {"u.yaml": base("unsigned_email")}, "policy": "required"},
    "unsigned_required_missing_store": {"files": {"u.yaml": base("unsigned_email")}, "policy": "required", "trust": "missing"},
    "signed_untrusted_key": {"files": {"s.yaml": base("rogue_email")}, "sign": {"s.yaml": "rogue"}},
    "signed_missing_store": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "trust": "missing"},
    "signed_then_tampered": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "after_sign": {"s.yaml": base("signed_email").replace("$.email", "$.work_email")}},
    "signed_then_reformatted": {
        "files": {"s.yaml": base("signed_email")},
        "sign": {"s.yaml": "trusted"},
        "after_sign": {"s.yaml": "# reformatted, same canonical form\nresponse: {mappings: {email: $.email}}\nrequest: {url: 'https://api.example.com/find'}\ncapability: email\nname: signed_email\nmanifest_version: '1'\n"},
    },
    "signature_bad_version": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "patch_sig": {"signature_version": "2"}},
    "signature_bad_algorithm": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "patch_sig": {"algorithm": "RSA"}},
    "signature_wrong_bytes": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "patch_sig": {"signature": base64.b64encode(b"\x00" * 64).decode()}},
    "signature_bad_base64": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "patch_sig": {"signature": "not base64!"}},
    "signature_bad_padding": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "patch_sig": {"signature": "YQ"}},
    "signature_not_json": {"files": {"s.yaml": base("signed_email"), "s.yaml.sig": "{not json"}},
    "signature_json_list": {"files": {"s.yaml": base("signed_email"), "s.yaml.sig": "[1, 2]"}},
    "signature_key_id_int": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "patch_sig": {"key_id": 7}},
    "signature_key_id_list": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "patch_sig": {"key_id": [1]}},
    "signature_missing_signature": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "patch_sig": {"signature": None}, "drop_sig_keys": ["signature"]},
    "signature_policy_unknown": {"files": {"u.yaml": base("unsigned_email")}, "policy": "sometimes"},
    "signed_date_canonical": {"files": {"s.yaml": base("dated_email", released="2026-01-01"), "s.yaml.sig": json.dumps({"signature_version": "1", "algorithm": "Ed25519", "key_id": "parity-trusted", "manifest_sha256": "0" * 64, "signature": ""})}},
    "trust_store_bad_version": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "trust": "bad_version"},
    "trust_store_true_version": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "trust": "true_version"},
    "trust_store_short_key": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "trust": "short_key"},
    "trust_store_no_publisher": {"files": {"s.yaml": base("signed_email")}, "sign": {"s.yaml": "trusted"}, "trust": "no_publisher"},
}


def pub_b64(key):
    raw = key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    return base64.b64encode(raw).decode()


def write_trust_stores():
    stores = {
        "store": {"version": 1, "keys": [{"key_id": "parity-trusted", "publisher": "Parity Test Publisher", "public_key": pub_b64(TRUSTED_KEY)}]},
        "bad_version": {"version": 2, "keys": []},
        "true_version": {"version": True, "keys": [{"key_id": "parity-trusted", "publisher": "Bool Version", "public_key": pub_b64(TRUSTED_KEY)}]},
        "short_key": {"version": 1, "keys": [{"key_id": "parity-trusted", "public_key": base64.b64encode(b"short").decode()}]},
        "no_publisher": {"version": 1, "keys": [{"key_id": "parity-trusted", "public_key": pub_b64(TRUSTED_KEY)}]},
    }
    for name, body in stores.items():
        (V1 / f"trust-{name}.json").write_text(json.dumps(body, indent=2) + "\n", encoding="utf-8")
    for name, key in (("trusted", TRUSTED_KEY), ("rogue", ROGUE_KEY)):
        pem = key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption())
        (V1 / f"test-only-{name}.pem").write_bytes(pem)


def trust_path(kind):
    if kind == "missing":
        return V1 / "trust-missing.json"
    return V1 / f"trust-{kind}.json"


def materialize(name, spec):
    case_dir = CASES / name
    for rel, body in spec["files"].items():
        target = case_dir / rel
        if body is None:
            target.mkdir(parents=True, exist_ok=True)
            continue
        target.parent.mkdir(parents=True, exist_ok=True)
        if isinstance(body, bytes):
            target.write_bytes(body)
        else:
            target.write_text(body, encoding="utf-8")
    for rel, who in spec.get("sign", {}).items():
        key_id = "parity-trusted" if who == "trusted" else "parity-rogue"
        sign_manifest(case_dir / rel, V1 / f"test-only-{who}.pem", key_id)
        sig_path = case_dir / f"{rel}.sig"
        patch = spec.get("patch_sig")
        if patch:
            env = json.loads(sig_path.read_text(encoding="utf-8"))
            env.update(patch)
            for k in spec.get("drop_sig_keys", []):
                env.pop(k, None)
            sig_path.write_text(json.dumps(env, sort_keys=True, indent=2) + "\n", encoding="utf-8")
    for rel, body in spec.get("after_sign", {}).items():
        (case_dir / rel).write_text(body, encoding="utf-8")
    return case_dir


def relativize(value, case_dir):
    prefix = str(case_dir)
    if isinstance(value, str):
        return value.replace(prefix, "<DIR>")
    if isinstance(value, list):
        return [relativize(v, case_dir) for v in value]
    if isinstance(value, dict):
        return {k: relativize(v, case_dir) for k, v in value.items()}
    return value


# Parser-originated messages (PyYAML / json / codec wording) are compared by
# verdict and prefix only; every rule and schema message is compared exactly.
INEXACT = {
    "invalid_yaml", "two_documents", "unknown_tag", "value_tag", "nonprintable_char",
    "invalid_utf8", "signature_not_json",
}


def pydantic_issues(path):
    try:
        load_manifest(path)
    except ValidationError as exc:
        return [{"loc": list(e["loc"]), "type": e["type"]} for e in exc.errors()]
    except Exception:  # noqa: BLE001
        return None
    return None


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def main():
    if CASES.exists():
        shutil.rmtree(CASES)
    CASES.mkdir(parents=True)
    write_trust_stores()
    expected = {"cases": {}, "real": None}
    for name, spec in CASES_DEF.items():
        case_dir = materialize(name, spec)
        policy = spec.get("policy", "optional")
        trust = spec.get("trust", "store")
        report = validate_manifest_directory(case_dir, signature_policy=policy if policy in {"optional", "required"} else None, trust_store=trust_path(trust)) if policy in {"optional", "required"} else None
        if report is None:
            import os
            os.environ["CONNECTOR_SIGNATURE_POLICY"] = policy
            try:
                report = validate_manifest_directory(case_dir, trust_store=trust_path(trust))
            finally:
                del os.environ["CONNECTOR_SIGNATURE_POLICY"]
        errors = []
        for err in report["errors"]:
            path = Path(err["path"])
            issues = pydantic_issues(path) if path.is_file() else None
            errors.append({
                "path": str(path.relative_to(case_dir)),
                "error": relativize(err["error"], case_dir),
                "pydantic": issues,
            })
        expected["cases"][name] = {
            "exact_messages": name not in INEXACT,
            "policy": policy,
            "trust": trust,
            "ok": report["ok"],
            "signature_policy": report["signature_policy"],
            "count": report["count"],
            "connectors": [canonical(c) for c in report["connectors"]],
            "errors": errors,
            "json": relativize(json.dumps(report, indent=2), case_dir),
        }
    real = validate_manifest_directory(MANIFESTS_DIR, signature_policy="optional", trust_store=TRUST_STORE)
    expected["real"] = {
        "ok": real["ok"],
        "count": real["count"],
        "connectors": [canonical(c) for c in real["connectors"]],
        "errors": real["errors"],
    }
    (V1 / "expected.json").write_text(json.dumps(expected, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    accepted = sum(1 for c in expected["cases"].values() if c["ok"])
    print(f"wrote {len(expected['cases'])} cases ({accepted} accepted) + real catalog ({real['count']} connectors)")


if __name__ == "__main__":
    main()
