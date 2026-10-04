"""Generate PyYAML/json parity fixtures for the Go pycompat package.

Run from the repository root:

    uv run python apps/server/internal/plugin/pycompat/testdata/gen_yaml_parity.py

For every YAML snippet it records what the Python connector SDK computes:
yaml.safe_load, then the signing canonical form
json.dumps(v, sort_keys=True, separators=(",", ":"), ensure_ascii=False).
It also records str() of the loaded value (used by template rendering) and
json.loads round trips for the Python-only JSON literals.
"""

import json
from pathlib import Path

import yaml

HERE = Path(__file__).resolve().parent

YAML_CASES = {
    # implicit typing (YAML 1.1 as implemented by PyYAML)
    "bool_words": "a: yes\nb: No\nc: ON\nd: off\ne: True\nf: FALSE\ng: y\nh: n\ni: tRue",
    "null_words": "a: ~\nb: null\nc: Null\nd: NULL\ne:\nf: nULL",
    "ints": "a: 0\nb: -0\nc: 017\nd: 0x1F\ne: 0b101\nf: 1_000\ng: +12\nh: -0x_ff\ni: 0_7",
    "int_sexagesimal": "a: 1:20\nb: -1:30:00\nc: 190:20:30",
    "int_big": "a: 12345678901234567890123\nb: -98765432109876543210",
    "int_octal_like": "a: 08\nb: 0o17\nc: 09.5",
    "floats": "a: 0.1\nb: .5\nc: 1.\nd: 1.0e+3\ne: 1.5E-10\nf: 6.02e+23\ng: 100000000000000000.0\nh: 0.00001\ni: 0.0001\nj: 1_0.2_5",
    "floats_not": "a: 1e3\nb: 1.0e3\nc: -.5\nd: 1e-5\ne: .inf.\nf: +.5",
    "float_special": "a: .inf\nb: -.Inf\nc: +.INF\nd: .nan\ne: .NaN",
    "float_sexagesimal": "a: 1:20.5\nb: -1:00:30.25",
    "float_repr_edges": "a: 1234567890123456.0\nb: 12345678901234567.0\nc: 0.000123\nd: 123456789.123456789\ne: 5e-324x\nf: 2.5e-08\ng: -0.0\nh: 1.0e+16\ni: 9999999999999998.0",
    "dates": "a: 2026-01-01",
    "datetime": "a: 2026-01-01T10:20:30Z",
    "date_quoted": "a: '2026-01-01'",
    "date_like_string": "a: 2026-1-1",
    "bad_date": "a: 2026-13-01",
    "strings": "a: hello\nb: 'quoted 1'\nc: \"double\\tescape\"\nd: plain words here\ne: '123'\nf: \"yes\"",
    "unicode": "a: caf\u00e9\nb: \u65e5\u672c\nc: \"\\u2028sep\"\nd: emoji \U0001F600",
    "escape_chars": "a: \"line\\nbreak\"\nb: \"quote\\\" and back\\\\slash\"\nc: \"ctrl\\x01\\x1f\"\nd: \"bell\\b\\f\"",
    "block_scalars": "a: |\n  line one\n  line two\nb: >\n  folded\n  text\nc: |-\n  strip\n",
    "nested": "outer:\n  inner:\n    list: [1, two, 3.0, null, true]\n  empty_map: {}\n  empty_list: []",
    "key_sorting": "b: 1\na: 2\nB: 3\n_: 4\n\u00e9: 5\nA1: 6\na1: 7",
    "nested_sorting": "z: {y: 1, x: 2}\ny: [{b: 1, a: 2}]",
    "dup_keys": "a: 1\nb: 2\na: 3",
    "int_keys": "1: one\n2: two",
    "mixed_keys": "1: x\n'1': y",
    "equal_keys": "{1: a, 1.0: b, true: c}",
    "bool_key": "true: x\nfalse: y",
    "null_key": "~: x",
    "float_key": "1.5: x\n2.5: y",
    "anchors": "base: &b {x: 1, y: 2}\nuse: *b\nlist: [*b, *b]",
    "merge": "base: &b {x: 1, y: 2}\nderived:\n  <<: *b\n  y: 3\n  z: 4",
    "merge_list": "a: &a {x: 1}\nb: &b {x: 2, y: 2}\nc:\n  <<: [*a, *b]\n  z: 3",
    "merge_quoted": "'<<': {x: 1}",
    "merge_bad": "<<: 5",
    "explicit_tags": "a: !!str 12\nb: !!int '42'\nc: !!float 1\nd: !!bool yes\ne: !!null x",
    "binary": "a: !!binary aGVsbG8=",
    "set": "a: !!set {x, y}",
    "omap": "a: !!omap [b: 1, a: 2]",
    "unknown_tag": "a: !foo x",
    "value_tag": "a: =",
    "value_key": "=: x",
    "unhashable_key": "? [1]\n: x",
    "two_docs": "--- 1\n--- 2",
    "empty": "",
    "only_comment": "# nothing\n",
    "scalar_doc": "just a string",
    "list_doc": "- a\n- b",
    "nonprintable": "a: \"\x7f\"",
    "bom": "\ufeffa: 1",
    "tabs_in_value": "a: \"x\ty\"",
    "long_string": "a: " + "x" * 300,
    "manifest_like": (
        "manifest_version: \"1\"\nname: acme_email\ncapability: email\n"
        "default_confidence: 0.8\ncost_per_lookup: 0.05\n"
        "auth:\n  type: bearer\n  env_var: ACME_API_KEY\n"
        "request:\n  method: POST\n  url: https://api.example.com/v1/find\n"
        "  headers:\n    Content-Type: application/json\n"
        "  body:\n    first_name: \"{{input.first_name}}\"\n    n: 5\n  timeout: 20\n"
        "response:\n  mappings:\n    email: \"$.data.email\"\n"
    ),
}

JSON_CASES = {
    "nan_inf": '{"a": NaN, "b": Infinity, "c": -Infinity}',
    "dup": '{"a": 1, "b": 2, "a": 3}',
    "numbers": '{"i": 10, "f": 1.0, "e": 1e2, "E": 2E-3, "big": 123456789012345678901234567890, "neg": -0, "negf": -0.0}',
    "escapes": '{"s": "a\\u00e9\\ud83d\\ude00\\n\\/"}',
    "nested": '[1, [2, {"z": null, "a": true}]]',
    "bad": '{"a": 1,}',
    "trailing": '{"a": 1} x',
}


def describe(fn):
    try:
        return {"ok": True, "value": fn()}
    except Exception as exc:  # noqa: BLE001 - parity records the failure kind
        return {"ok": False, "error_type": type(exc).__name__, "error": str(exc).splitlines()[0]}


def main():
    out = {"yaml": [], "json": []}
    for name, text in YAML_CASES.items():
        case = {"name": name, "input": text}
        try:
            value = yaml.safe_load(text)
        except Exception as exc:  # noqa: BLE001
            case["load_ok"] = False
            case["load_error"] = type(exc).__name__
            out["yaml"].append(case)
            continue
        case["load_ok"] = True
        case["str"] = str(value)
        canon = describe(lambda: json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False))
        case["canonical_ok"] = canon["ok"]
        if canon["ok"]:
            case["canonical"] = canon["value"]
        else:
            case["canonical_error"] = canon["error"]
        pretty = describe(lambda: json.dumps(value, sort_keys=True, indent=2))
        if pretty["ok"]:
            case["pretty_ascii"] = pretty["value"]
        out["yaml"].append(case)
    for name, text in JSON_CASES.items():
        case = {"name": name, "input": text}
        try:
            value = json.loads(text)
            case["ok"] = True
            case["canonical"] = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
            case["str"] = str(value)
        except Exception as exc:  # noqa: BLE001
            case["ok"] = False
            case["error_type"] = type(exc).__name__
        out["json"].append(case)
    target = HERE / "parity.json"
    target.write_text(json.dumps(out, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    print(f"wrote {len(out['yaml'])} yaml and {len(out['json'])} json cases to {target}")


if __name__ == "__main__":
    main()
