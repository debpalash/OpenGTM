"""Record what the Python DeclarativeProvider (compiler.py) sends and returns.

Run from the repository root:

    uv run python apps/server/internal/plugin/declarative/testdata/gen_provider_parity.py

Each scenario runs DeclarativeProvider.enrich() with httpx replaced by a
MockTransport that captures the outgoing request and replies with a canned
response, and with the lead-context builder replaced by the scenario inputs.
The Go test replays the same response through a fake egress transport and
compares request method/URL/headers/body and the result.
"""

import asyncio
import json
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[5]
sys.path.insert(0, str(ROOT))

import httpx  # noqa: E402
import yaml  # noqa: E402

from apps.api.services.leadgen.enrichment.declarative import compiler  # noqa: E402
from apps.api.services.leadgen.enrichment.declarative.manifest import ProviderManifest, _coerce  # noqa: E402

LEADMAGIC = (ROOT / "apps/api/services/leadgen/enrichment/declarative/manifests/email/leadmagic.yaml").read_text()
MOBILE = (ROOT / "apps/api/services/leadgen/enrichment/declarative/manifests/phone/leadmagic_mobile.yaml").read_text()
PROSPEO = (ROOT / "apps/api/services/leadgen/enrichment/declarative/manifests/phone/prospeo_mobile.yaml").read_text()

LEAD = {
    "company": "Acme Corp", "company_name": "Acme Corp", "website": "https://www.acme.example/about",
    "domain": "acme.example", "email": "ada@acme.example", "linkedin_url": "https://linkedin.com/in/ada",
    "city": "London", "contact_person": "Ada King Lovelace", "full_name": "Ada King Lovelace",
    "first_name": "Ada", "last_name": "Lovelace", "phone": "",
}

GENERIC = """manifest_version: "1"
name: generic_provider
capability: email
default_confidence: 0.65
cost_per_lookup: 0.02
auth: {auth}
request:
  method: {method}
  url: "{url}"
  headers: {headers}
  query: {query}
  {body}
  timeout: 7
response:
  error_path: {error_path}
  error_message_path: {error_message_path}
  mappings: {mappings}
"""


def generic(auth="{type: none}", method="POST", url="https://api.vendor.example/v1/find", headers="{}", query="{}",
            body="body: {first: '{{input.first_name}}'}", error_path="error", error_message_path="message",
            mappings="{email: $.data.email}"):
    return GENERIC.format(auth=auth, method=method, url=url, headers=headers, query=query, body=body,
                          error_path=error_path, error_message_path=error_message_path, mappings=mappings)


SCENARIOS = [
    {"name": "leadmagic_email_hit", "manifest": LEADMAGIC, "env": {"LEADMAGIC_API_KEY": "lm_secret_1"},
     "response": {"status": 200, "body": {"email": "ada@acme.example", "status": "valid"}}},
    {"name": "leadmagic_email_miss", "manifest": LEADMAGIC, "env": {"LEADMAGIC_API_KEY": "lm_secret_1"},
     "response": {"status": 200, "body": {"email": None, "message": "not found"}}},
    {"name": "leadmagic_missing_key", "manifest": LEADMAGIC, "env": {}, "response": None},
    {"name": "mobile_normalized", "manifest": MOBILE, "env": {"LEADMAGIC_API_KEY": "k"},
     "response": {"status": 200, "body": {"mobile_number": "+1 (415) 555-0132", "credits_consumed": 1}}},
    {"name": "prospeo_error_envelope", "manifest": PROSPEO, "env": {"PROSPEO_API_KEY": "pk"},
     "response": {"status": 200, "body": {"error": True, "message": "NO_MATCH — " + "x" * 200}}},
    {"name": "prospeo_hit_00_prefix", "manifest": PROSPEO, "env": {"PROSPEO_API_KEY": "pk"},
     "response": {"status": 200, "body": {"error": False, "response": {"raw_format": "0044 20 7946 0958"}}}},
    {"name": "http_404", "manifest": generic(), "env": {}, "response": {"status": 404, "body": {"detail": "nope"}}},
    {"name": "http_500", "manifest": generic(), "env": {}, "response": {"status": 500, "raw": "oops"}},
    {"name": "non_json", "manifest": generic(), "env": {}, "response": {"status": 200, "raw": "<html>hi</html>"}},
    {"name": "envelope_without_message_path", "manifest": generic(error_message_path="null"), "env": {},
     "response": {"status": 200, "body": {"error": "yes"}}},
    {"name": "envelope_message_number", "manifest": generic(), "env": {},
     "response": {"status": 200, "body": {"error": 1, "message": 42.0}}},
    {"name": "envelope_falsy", "manifest": generic(), "env": {},
     "response": {"status": 200, "body": {"error": 0, "data": {"email": "x@y.z"}}}},
    {"name": "no_data", "manifest": generic(), "env": {}, "response": {"status": 200, "body": {"data": {}}}},
    {"name": "bearer_and_query_merge",
     "manifest": generic(auth="{type: bearer, env_var: VENDOR_KEY}", method="get",
                         url="https://api.vendor.example/v1/find?domain=old&keep=1&q={{input.company}}",
                         query="{domain: '{{input.domain}}', page: 2, flag: true}", body="body: null"),
     "env": {"VENDOR_KEY": "vk_123"}, "response": {"status": 200, "body": {"data": {"email": "a@acme.example"}}}},
    {"name": "query_auth",
     "manifest": generic(auth="{type: query, env_var: VENDOR_KEY}", method="GET", body="body: null"),
     "env": {"VENDOR_KEY": "q s+cret&x"}, "response": {"status": 200, "body": {"data": {"email": "q@acme.example"}}}},
    {"name": "query_auth_custom_param",
     "manifest": generic(auth="{type: query, param: key, env_var: VENDOR_KEY}", method="GET", body="body: null"),
     "env": {"VENDOR_KEY": "vk"}, "response": {"status": 200, "body": {"data": {"email": "q@acme.example"}}}},
    {"name": "header_auth_value_template",
     "manifest": generic(auth="{type: header, param: X-Token, env_var: VENDOR_KEY, value: 'Token ${env:VENDOR_KEY}'}",
                         headers="{X-Version: 2, X-Flag: true, X-Name: '{{input.full_name}}'}"),
     "env": {"VENDOR_KEY": "tok"}, "response": {"status": 200, "body": {"data": {"email": "h@acme.example"}}}},
    {"name": "header_auth_default_param", "manifest": generic(auth="{type: header, env_var: VENDOR_KEY}"),
     "env": {"VENDOR_KEY": "raw"}, "response": {"status": 200, "body": {"data": {"email": "h@acme.example"}}}},
    {"name": "body_types_and_defaults",
     "manifest": generic(body="body: {n: 5, f: 1.5, b: true, z: null, list: ['{{input.city}}', 3], nested: {d: '{{input.missing | default: none}}', u: 'café {{input.last_name}}'}}"),
     "env": {}, "response": {"status": 200, "body": {"data": {"email": "b@acme.example"}}}},
    {"name": "mapping_prefix_and_types",
     "manifest": generic(mappings="{email: $.data.email, linkedin_url: 'https://linkedin.com/in/$.handle', company_size: $.size, verified: $.ok, phone: '$.phones[0]', mobile_phone: '$.phones[]'}"),
     "env": {}, "response": {"status": 200, "body": {"data": {"email": "m@acme.example"}, "handle": "ada", "size": 250, "ok": False, "phones": ["415.555.0132", "+44 20 7946 0958"]}}},
    {"name": "env_in_input_expands",
     "manifest": generic(auth="{type: bearer, env_var: VENDOR_KEY}", body="body: {q: '{{input.company}}'}"),
     "inputs": {**LEAD, "company": "${env:VENDOR_KEY}"},
     "env": {"VENDOR_KEY": "vk_secret"}, "response": {"status": 200, "body": {"data": {"email": "e@acme.example"}}}},
]


class Capture:
    request = None


def run(scenario):
    raw = yaml.safe_load(scenario["manifest"])
    manifest = ProviderManifest(**_coerce(raw))
    inputs = scenario.get("inputs", LEAD)
    env = scenario["env"]
    cap = Capture()

    def handler(request: httpx.Request):
        cap.request = request
        resp = scenario["response"]
        if resp is None:
            raise AssertionError("no request expected")
        if "raw" in resp:
            return httpx.Response(resp["status"], text=resp["raw"])
        return httpx.Response(resp["status"], json=resp["body"])

    real_client = httpx.AsyncClient

    def mock_client(*args, **kwargs):
        kwargs["transport"] = httpx.MockTransport(handler)
        return real_client(*args, **kwargs)

    compiler.httpx.AsyncClient = mock_client
    compiler._lead_input_ctx = lambda lead: dict(lead)
    import apps.api.core.url_guard as guard
    guard.check_url = lambda url, **kw: url
    try:
        provider = compiler.DeclarativeProvider(manifest, env_resolver=lambda name: env.get(name, ""))
        result = asyncio.run(provider.enrich(inputs))
    finally:
        compiler.httpx.AsyncClient = real_client
    out = {
        "name": scenario["name"],
        "manifest": scenario["manifest"],
        "inputs": inputs,
        "env": env,
        "response": scenario["response"],
        "result": {
            "success": result.success,
            "fields": json.dumps(result.fields, sort_keys=True, separators=(",", ":"), ensure_ascii=False),
            "confidence": result.confidence,
            "error": result.error,
        },
        "request": None,
    }
    if cap.request is not None:
        r = cap.request
        out["request"] = {
            "method": r.method,
            "url": str(r.url),
            "headers": {k: v for k, v in r.headers.items() if k.lower() not in {"accept", "accept-encoding", "connection", "user-agent", "host", "content-length"}},
            "body": r.content.decode("utf-8"),
        }
    return out


def main():
    results = [run(s) for s in SCENARIOS]
    (HERE / "provider_parity.json").write_text(json.dumps(results, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    print(f"wrote {len(results)} provider scenarios")


if __name__ == "__main__":
    main()
