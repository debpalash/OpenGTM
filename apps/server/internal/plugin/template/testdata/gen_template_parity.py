"""Generate parity fixtures for the Go port of declarative/template.py.

Run from the repository root:

    uv run python apps/server/internal/plugin/template/testdata/gen_template_parity.py

Inputs are stored as JSON text so the Go test decodes them with the same
types Python's json.loads produces (int vs float matters for str()).
Results are stored as canonical JSON (sort_keys, compact, ensure_ascii=False).
"""

import json
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[5]
sys.path.insert(0, str(ROOT))

from apps.api.services.leadgen.enrichment.declarative.template import (  # noqa: E402
    project_response,
    project_value,
    render_string,
    render_template,
)

ENV = {"API_KEY": "sk_live_123", "EMPTY": "", "TOKEN_2": "t2"}

CTX = {
    "input": {
        "first_name": "Ada",
        "last_name": "Lovelace",
        "domain": "example.com",
        "empty": "",
        "none": None,
        "zero": 0,
        "false": False,
        "true": True,
        "int": 42,
        "float": 1.0,
        "float2": 2.5,
        "big": 12345678901234567890,
        "exp": 1e20,
        "small": 0.00001,
        "list": ["a", 1, None, True],
        "dict": {"k": "v", "n": 1},
        "quote": "it's",
        "both": "it's \"x\"",
        "unicode": "café 日",
        "nested": {"deep": {"value": "found"}},
        "spaced key": "sp",
        "env_like": "${env:API_KEY}",
    },
    "top": "level",
}

RENDER_CASES = [
    "{{input.first_name}}",
    "{{ input.first_name }}",
    "{{input.first_name}} {{input.last_name}}",
    "{{input.missing}}",
    "{{input.missing | default: fallback}}",
    "{{input.missing|default:fallback}}",
    "{{input.empty | default: 'quoted'}}",
    "{{input.empty | default: \"dq\"}}",
    "{{input.none | default:   spaced out   }}",
    "{{input.zero}}",
    "{{input.false}}",
    "{{input.true}}",
    "{{input.int}}",
    "{{input.float}}",
    "{{input.float2}}",
    "{{input.big}}",
    "{{input.exp}}",
    "{{input.small}}",
    "{{input.list}}",
    "{{input.dict}}",
    "{{input.quote}}",
    "{{input.both}}",
    "{{input.unicode}}",
    "{{input.nested.deep.value}}",
    "{{input.nested.deep.missing | default: d}}",
    "{{input.first_name.length}}",
    "{{top}}",
    "{{input.spaced key}}",
    "{{input.first_name | default: x}}",
    "{{input.missing | default: }}",
    "{{input.missing | defaulted: x}}",
    "{{ }}",
    "{{input.first_name}",
    "{input.first_name}}",
    "{{input.a}b}}",
    "{{input.first_name|default:a|b}}",
    "${env:API_KEY}",
    "Bearer ${env:API_KEY}",
    "${env:MISSING}",
    "${env:EMPTY}x",
    "${env:BAD-NAME}",
    "${ env:API_KEY }",
    "{{input.env_like}}",
    "https://{{input.domain}}/v1?key=${env:TOKEN_2}&q={{input.first_name}}",
    "no templates here",
    "",
    "{{input.first_name}}{{input.last_name}}",
    "{{\tinput.first_name\n}}",
    "{{input.missing|default: nbsp }}",
]

RENDER_TEMPLATE_CASES = [
    {"q": "{{input.first_name}}", "n": 5, "f": 1.5, "b": True, "z": None, "tags": ["{{input.domain}}", 3, ["{{input.int}}"]]},
    ["{{input.missing | default: x}}", {"k": "${env:API_KEY}"}],
    "plain {{input.last_name}}",
    42,
    None,
]

DATA = {
    "data": {"work_email": "a@x.com", "n": 0, "f": 1.0, "empty": "", "null": None, "t": True, "false": False},
    "results": [{"email": "b@x.com"}, {"email": "c@x.com"}],
    "empty_list": [],
    "nested": [[1, 2], [3]],
    "handle": "spacex",
    "num": 7,
    "float": 2.50,
    "obj": {"a": 1, "b": [1, 2]},
    "list_of_str": ["x", "y"],
    "error": True,
    "message": "quota exceeded — retry later" + "!" * 150,
    "with_underscore": {"a_b": "u"},
    "k1": {"k2": "v"},
    "": "empty-key",
}

PROJECT_CASES = [
    "$.data.work_email",
    "$.results[].email",
    "$.results[0].email",
    "$.results[1].email",
    "$.results[2].email",
    "$.results[-1].email",
    "$.empty_list[]",
    "$.empty_list[0]",
    "$.nested[0]",
    "$.nested[0][1]",
    "$.nested[]",
    "$.missing.key",
    "$.data.n",
    "$.data.f",
    "$.data.empty",
    "$.data.null",
    "$.data.t",
    "$.data.false",
    "$.num",
    "$.float",
    "$.obj",
    "$.obj.b",
    "$.list_of_str",
    "https://linkedin.com/company/$.handle",
    "count: $.num",
    "f=$.float",
    "obj=$.obj",
    "t=$.data.t",
    "none=$.data.null",
    "$.",
    "x$.",
    "static literal",
    "",
    "$.with_underscore.a_b",
    "$.k1.k2",
    "$.k1..k2",
    "$.data.work-email",
    "$.results.email",
    "$.handle[0]",
    "$.results[١].email",
    "$.results[0x1].email",
    "$.results[ 0].email",
    "$$.handle",
    "a $.handle and $.num",
    "$.handle\n",
    "$.obj.a.b",
]

MAPPING_CASES = [
    {"email": "$.data.work_email", "linkedin_url": "https://linkedin.com/in/$.handle"},
    {"empty": "$.data.empty", "zero": "$.data.n", "false": "$.data.false", "null": "$.data.null", "missing": "$.nope"},
    {"lit": "constant", "blank": ""},
    {},
]


def canon(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def main():
    env = lambda name: ENV.get(name, "")  # noqa: E731
    out = {
        "env": ENV,
        "ctx_json": json.dumps(CTX),
        "data_json": json.dumps(DATA),
        "render_string": [{"template": t, "result": render_string(t, CTX, env)} for t in RENDER_CASES],
        "render_template": [
            {"value_json": json.dumps(v), "result": canon(render_template(v, CTX, env))} for v in RENDER_TEMPLATE_CASES
        ],
        "project_value": [{"expr": e, "result": canon(project_value(DATA, e))} for e in PROJECT_CASES],
        "project_response": [
            {"mappings_json": json.dumps(m), "result": canon(project_response(DATA, m))} for m in MAPPING_CASES
        ],
        "error_envelope": {
            "flag": canon(project_value(DATA, "$.error")),
            "message": str(project_value(DATA, "$.message"))[:120],
        },
    }
    target = HERE / "parity.json"
    target.write_text(json.dumps(out, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    print(f"wrote {len(RENDER_CASES)} render, {len(PROJECT_CASES)} projection cases to {target}")


if __name__ == "__main__":
    main()
