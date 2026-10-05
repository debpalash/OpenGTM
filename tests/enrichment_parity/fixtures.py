"""Connectors, workbooks and run steps shared by both sides of the parity test.

Everything here is data: the harness seeds it identically into two databases,
the Python runner and the Go driver each execute the same ``steps`` against
them, and the resulting states are compared.
"""
from __future__ import annotations

import json
from typing import Any

SECRET = "sim-secret-key"
WS1, WS2 = "ws-par-1", "ws-par-2"

# provider timeout used by every run (slow.example takes longer)
PROVIDER_TIMEOUT = 2.0


def _manifest(name: str, *, path: str, capability: str, mappings: dict, cost: float = 0.0, method: str = "GET",
              auth: str = "", confidence: float = 0.7, body: dict | None = None, query: dict | None = None) -> str:
    lines = [
        'manifest_version: "1"',
        f"name: {name}",
        f"capability: {capability}",
        f"default_confidence: {confidence}",
        f"cost_per_lookup: {cost}",
    ]
    if auth == "header":
        lines += ["auth:", "  type: header", "  param: X-Api-Key", "  env_var: PAR_KEY"]
    elif auth == "query":
        lines += ["auth:", "  type: query", "  param: key", "  env_var: PAR_KEY"]
    lines += ["request:", f"  method: {method}", f"  url: BASE{path}", "  timeout: 5"]
    if query is not None:
        lines.append("  query:")
        lines += [f'    {k}: "{v}"' for k, v in query.items()]
    if body is not None:
        lines.append("  body:")
        lines += [f'    {k}: "{v}"' for k, v in body.items()]
    lines += ["response:", "  error_path: error", "  error_message_path: message", "  mappings:"]
    lines += [f'    {k}: "{v}"' for k, v in mappings.items()]
    return "\n".join(lines) + "\n"


def connectors(base: str) -> dict[str, str]:
    q = {"domain": "{{input.domain}}"}
    texts = {
        "par_free_email": _manifest("par_free_email", path="/free/email", capability="email", query=q,
                                    mappings={"email": "$.data.email"}),
        "par_free_phone": _manifest("par_free_phone", path="/free/phone", capability="phone", query=q,
                                    mappings={"phone": "$.data.phone"}),
        "par_free_both": _manifest("par_free_both", path="/free/both", capability="email", query=q,
                                   mappings={"email": "$.data.email", "phone": "$.data.phone", "city": "$.data.city"}),
        "par_blob": _manifest("par_blob", path="/free/blob", capability="people_json", query=q,
                              mappings={"people_json": "$.data.people_json"}),
        "par_keyed": _manifest("par_keyed", path="/keyed/email", capability="email", query=q, auth="header",
                               mappings={"email": "$.data.email"}),
        "par_query": _manifest("par_query", path="/qk/email", capability="email", query=q, auth="query",
                               mappings={"email": "$.data.email"}),
        "par_timeout": _manifest("par_timeout", path="/free/email", capability="email", query=q,
                                 mappings={"email": "$.data.email"}),
        "par_cooled": _manifest("par_cooled", path="/free/phone", capability="phone", query=q,
                                mappings={"phone": "$.data.phone"}),
        "par_paid": _manifest("par_paid", path="/paid/lookup", capability="email", method="POST", cost=0.05,
                              confidence=0.9, body={"domain": "{{input.domain}}"},
                              mappings={"email": "$.data.email"}),
    }
    return {name: text.replace("BASE", base) for name, text in texts.items()}


def _cols(*extra: dict) -> list[dict]:
    return [
        {"id": "company", "name": "Company", "type": "lead_field"},
        {"id": "website", "name": "Website", "type": "lead_field"},
        *extra,
    ]


def _enrich(cid: str, name: str, provider: str, target: str, **kw) -> dict:
    return {"id": cid, "name": name, "type": "enrichment", "provider": provider, "target_field": target,
            "verify": False, **kw}


def _chain(cid: str, name: str, chain: list[str], target: str, **kw) -> dict:
    return {"id": cid, "name": name, "type": "waterfall", "waterfall": chain, "target_field": target,
            "verify": False, **kw}


BASIC_SITES = [
    "https://www.ok.example/about", "nomatch.example", "empty.example", "err500.example", "ratelimit.example",
    "badjson.example", "envelope.example", "flaky.example", "", 123,
    "https://WWW.Ünï.example/x?y=1", "http://acme.example/path", "ok.example",
]


def _row(i: int, site: Any, **extra) -> dict:
    return {"company": f"Company {i}", "website": site, "contact_person": f"Person {i} Smith", "city": "", **extra}


def workbooks() -> list[dict]:
    basic_cols = _cols(
        _enrich("email", "Email", "par_free_email", "email"),
        _chain("phone", "Phone", ["par_free_phone"], "phone"),
        _chain("chain", "Chain", ["par_free_email", "par_free_both"], "city"),
        _enrich("people", "People", "par_blob", "people_json"),
        _enrich("keyed", "Keyed", "par_keyed", "email"),
        _enrich("qk", "Query key", "par_query", "email"),
    )
    fill_cols = _cols(_enrich("email", "Email", "par_free_email", "email"),
                      _chain("phone", "Phone", ["par_free_phone"], "phone"))
    paid_cols = _cols(_enrich("paid", "Paid", "par_paid", "email"))
    cool_cols = _cols(_chain("only", "Only", ["par_cooled"], "phone"),
                      _chain("second", "Second", ["par_cooled", "par_free_both"], "phone"))
    slow_cols = _cols(_enrich("slow", "Slow", "par_timeout", "email"),
                      _chain("after", "After", ["par_timeout", "par_free_both"], "city"))
    return [
        {"id": "wb-basic", "ws": WS1, "columns": basic_cols, "budget": 0.0,
         "rows": [_row(i, s) for i, s in enumerate(BASIC_SITES)]},
        {"id": "wb-fill", "ws": WS1, "columns": fill_cols, "budget": 0.0, "rows": [
            _row(0, "ok.example", enrichments={
                "email": {"value": "kept@ok.example", "status": "complete", "provider": "hunter_io", "error": None},
                "phone": {"value": None, "status": "error", "provider": None, "error": "no_data"}}),
            _row(1, "nomatch.example", enrichments={
                "email": {"value": None, "status": "error", "provider": None, "error": "timeout"}}),
            _row(2, "acme.example"),
            _row(3, "err500.example", enrichments={
                "phone": {"value": "+1 555 0000", "status": "complete", "provider": "x", "error": None}}),
        ]},
        {"id": "wb-scope", "ws": WS1, "columns": fill_cols, "budget": 0.0,
         "rows": [_row(i, f"scope{i}.example") for i in range(4)]},
        {"id": "wb-paid-capped", "ws": WS1, "columns": paid_cols, "budget": 0.12,
         "rows": [_row(i, s) for i, s in enumerate(["ok.example", "acme.example", "beta.example", "gamma.example"])]},
        {"id": "wb-unicode", "ws": WS1, "budget": 0.0,
         "columns": _cols(_enrich("emäil-ü", "Émail ☃ \"q\"", "par_paid", "email")),
         "rows": [_row(0, "ünï.example", company="Ünï ☃ GmbH", contact_person="Zoë \u00c4 Smith"),
                  _row(1, "https://www.日本.example/ü?x=1", company="日本 KK")]},
        {"id": "wb-budget-edge", "ws": WS1, "columns": paid_cols, "budget": 0.2, "spent": 0.15,
         "rows": [_row(i, f"edge{i}.example") for i in range(3)]},
        {"id": "wb-paid", "ws": WS1, "columns": paid_cols, "budget": 0.0,
         "rows": [_row(i, s) for i, s in enumerate(["ok.example", "err500.example", "nomatch.example", "slow.example"])]},
        {"id": "wb-cool", "ws": WS1, "columns": cool_cols, "budget": 0.0,
         "rows": [_row(i, f"cool{i}.example") for i in range(3)]},
        {"id": "wb-paused", "ws": WS1, "columns": fill_cols, "budget": 0.0, "status": "paused",
         "rows": [_row(i, f"paused{i}.example") for i in range(3)]},
        {"id": "wb-empty", "ws": WS1, "columns": fill_cols, "budget": 0.0, "rows": [_row(0, "e.example")]},
        {"id": "wb-slow", "ws": WS1, "columns": slow_cols, "budget": 0.0,
         "rows": [_row(i, s) for i, s in enumerate(["slow.example", "ok.example", "acme.example"])]},
        {"id": "wb-iso", "ws": WS2, "columns": basic_cols, "budget": 0.0,
         "rows": [_row(i, s) for i, s in enumerate(["ok.example", "nomatch.example", "acme.example"])]},
        {"id": "wb-iso-untouched", "ws": WS2, "columns": fill_cols, "budget": 0.0,
         "rows": [_row(0, "untouched.example")]},
    ]


def provider_stats() -> list[dict]:
    """A benched provider (cooldown in the future) and one with history."""
    return [
        {"provider": "par_cooled", "field": "phone", "attempts": 10, "hits": 2, "cooldown_hours": 1},
        {"provider": "par_free_email", "field": "email", "attempts": 4, "hits": 4, "cooldown_hours": None},
    ]


def steps() -> list[dict]:
    """Ordered job runs: (workbook, payload overrides). Job ids are assigned by the harness."""
    common = {"concurrency": 3, "retry_passes": 1, "provider_timeout": PROVIDER_TIMEOUT, "provider_workers": 4}
    return [
        {"name": "basic", "workbook": "wb-basic", **common},
        {"name": "fill missing", "workbook": "wb-fill", "fill_missing": True, **common},
        {"name": "row/column scope", "workbook": "wb-scope", "scope": True, **common},
        {"name": "paid, capped budget", "workbook": "wb-paid-capped", **common, "concurrency": 1},
        {"name": "paid, non-ASCII identities", "workbook": "wb-unicode", **common, "concurrency": 1},
        {"name": "paid, budget edge (spent 0.15 of 0.2)", "workbook": "wb-budget-edge", **common, "concurrency": 1},
        {"name": "paid, unlimited budget", "workbook": "wb-paid", **common, "concurrency": 1},
        {"name": "cooldown", "workbook": "wb-cool", **common},
        {"name": "paused workbook", "workbook": "wb-paused", **common},
        {"name": "empty row scope", "workbook": "wb-empty", "row_ids": [], **common},
        {"name": "other tenant", "workbook": "wb-iso", **common},
        {"name": "missing workbook", "workbook": "wb-does-not-exist", **common},
        {"name": "no retry, max one provider", "workbook": "wb-iso-untouched", "retry_passes": 0, "max_providers": 1,
         "concurrency": 2, "provider_timeout": PROVIDER_TIMEOUT, "provider_workers": 2},
        # Last: timeouts bench a provider for later cells (planner circuit breaker), so run it
        # alone, one row at a time, after everything that shares those providers.
        {"name": "timeouts and the circuit breaker", "workbook": "wb-slow", **common, "concurrency": 1},
    ]


def dump(obj: Any) -> str:
    return json.dumps(obj, sort_keys=True, default=str)
