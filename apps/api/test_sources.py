#!/usr/bin/env python3
"""
Test ALL 91 lead discovery sources with REAL, region-appropriate queries.
Uses the actual ddgs library via uv venv + proxy rotation.
Saves per-source JSON + _summary.json to data/source_tests/

Run: uv run python3 apps/api/test_sources.py
"""

import asyncio
import json
import time
import os
import sys
import traceback
from datetime import datetime
from typing import Dict, List

PROJECT_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "../.."))
sys.path.insert(0, PROJECT_ROOT)

OUTPUT_DIR = os.path.join(PROJECT_ROOT, "data", "source_tests")
os.makedirs(OUTPUT_DIR, exist_ok=True)

# ── Region+Category → (query, city) ──────────────────────────────────
REGION_QUERIES: Dict[str, Dict[str, tuple]] = {
    "india": {
        "b2b_marketplace":  ("packaging machinery manufacturers", "mumbai"),
        "directory":        ("IT services companies", "pune"),
        "review":           ("software companies", "bangalore"),
        "jobs":             ("software engineer hiring", "hyderabad"),
        "government":       ("private limited company", "delhi"),
        "startup":          ("fintech startup", "bangalore"),
        "social":           ("IT company", "chennai"),
        "news":             ("startup funding", "bangalore"),
        "ecommerce":        ("electronics seller", "delhi"),
        "saas_directory":   ("CRM software", ""),
        "freelance":        ("web development agency", "india"),
        "developer":        ("open source company", "india"),
    },
    "us": {
        "directory":        ("plumbing contractors", "chicago"),
        "b2b_marketplace":  ("industrial valves manufacturer", "houston"),
        "review":           ("software company reviews", "san francisco"),
        "jobs":             ("software engineer hiring", "new york"),
        "government":       ("registered company", "delaware"),
        "freelance":        ("marketing agency", "los angeles"),
        "startup":          ("AI startup", "san francisco"),
        "ecommerce":        ("electronics brand", ""),
    },
    "eu": {
        "b2b_marketplace":  ("automotive parts supplier", "germany"),
        "directory":        ("IT consulting firms", "london"),
        "government":       ("technology company", "uk"),
        "startup":          ("fintech startup", "berlin"),
        "review":           ("software company", "london"),
    },
    "global": {
        "directory":        ("digital marketing agency", ""),
        "b2b_marketplace":  ("CNC machining supplier", ""),
        "review":           ("project management software", ""),
        "startup":          ("AI startup funding 2024", ""),
        "jobs":             ("software company hiring", ""),
        "social":           ("tech company", ""),
        "saas_directory":   ("CRM software", ""),
        "freelance":        ("web development agency", ""),
        "developer":        ("open source company", ""),
        "news":             ("tech startup funding", ""),
        "ecommerce":        ("DTC brand", ""),
    },
}


def get_test_query(source: Dict) -> tuple:
    """Best (query, city) pair for a source based on region + category."""
    category = source.get("category", "directory")
    regions = source.get("region", ["global"])
    for region in regions:
        if region in REGION_QUERIES:
            if category in REGION_QUERIES[region]:
                return REGION_QUERIES[region][category]
    return ("software companies", "")


def _ddg_search_sync(query: str, max_results: int = 5) -> list:
    """Synchronous DDG search using ddgs library with proxy fallback."""
    from ddgs import DDGS
    try:
        from apps.api.services.leadgen.proxy_client import get_proxy_url
        proxy = get_proxy_url()
        if proxy:
            with DDGS(proxy=proxy) as d:
                return list(d.text(query, max_results=max_results))
    except Exception:
        pass
    # Fallback: no proxy
    with DDGS() as d:
        return list(d.text(query, max_results=max_results))


async def ddg_search(query: str, max_results: int = 5) -> list:
    """Async wrapper."""
    return await asyncio.to_thread(_ddg_search_sync, query, max_results)


async def test_source(source: Dict) -> Dict:
    """Test a single source with region-appropriate query."""
    from apps.api.services.leadgen.source_registry import build_queries

    name = source["name"]
    label = source["label"]
    enabled = source.get("enabled", True)
    category = source.get("category", "")
    regions = source.get("region", [])

    if not enabled:
        return {
            "source": name, "label": label, "status": "disabled",
            "enabled": False, "category": category, "region": regions,
            "test_query": "", "test_city": "",
            "built_queries": [], "executed_query": "",
            "results_count": 0, "time_ms": 0,
            "results": [],
        }

    test_query, test_city = get_test_query(source)
    built_queries = build_queries(source, test_query, test_city)

    t0 = time.time()
    all_results = []
    errors = []

    # Execute first query template
    if built_queries:
        q = built_queries[0]
        try:
            results = await asyncio.wait_for(ddg_search(q, 5), timeout=20.0)
            if results:
                all_results.extend(results)
        except asyncio.TimeoutError:
            errors.append("Timeout (20s)")
        except Exception as e:
            errors.append(f"{type(e).__name__}: {e}")

    elapsed = round((time.time() - t0) * 1000)

    if errors and not all_results:
        status = "error"
    elif not all_results:
        status = "empty"
    else:
        status = "ok"

    return {
        "source": name, "label": label, "status": status,
        "enabled": True, "category": category, "region": regions,
        "site_domain": source.get("site_domain"),
        "test_query": test_query, "test_city": test_city,
        "built_queries": built_queries,
        "executed_query": built_queries[0] if built_queries else "",
        "results_count": len(all_results), "time_ms": elapsed,
        "errors": errors if errors else None,
        "results": all_results,
    }


async def main():
    from apps.api.services.leadgen.source_registry import SOURCES

    total = len(SOURCES)
    print(f"{'='*72}")
    print(f"  Lead Source Tester — Real Queries + DDG Library")
    print(f"  {datetime.now().isoformat()}")
    print(f"  Sources: {total}  |  ddgs library with proxy fallback")
    print(f"  Output: {OUTPUT_DIR}")
    print(f"{'='*72}\n")

    summaries = []

    # Run ONE at a time with delay to avoid DDG rate limit
    for idx, source in enumerate(SOURCES, 1):
        result = await test_source(source)

        s = result["status"]
        n = result["results_count"]
        ms = result["time_ms"]
        lbl = result["label"]
        qry = result.get("executed_query", "")[:55]

        icons = {"ok": "✅", "empty": "⚠️ ", "error": "❌", "disabled": "⏭ "}
        print(f"[{idx:2d}/{total}] {lbl:30s} {icons.get(s,'?')} {n:2d} results ({ms:5d}ms)  {qry}")

        summaries.append({
            "source": result["source"], "label": result["label"],
            "status": result["status"], "enabled": result["enabled"],
            "category": result["category"], "region": result["region"],
            "test_query": result.get("test_query", ""),
            "test_city": result.get("test_city", ""),
            "executed_query": result.get("executed_query", ""),
            "results_count": result["results_count"],
            "time_ms": result["time_ms"],
            "errors": result.get("errors"),
        })

        # Save per-source JSON
        fname = os.path.join(OUTPUT_DIR, f"{result['source']}.json")
        with open(fname, "w") as f:
            json.dump(result, f, indent=2, default=str)

        # Delay between each query to avoid rate limiting
        if s != "disabled" and idx < total:
            await asyncio.sleep(2.5)

    # Save summary
    summary = {
        "tested_at": datetime.now().isoformat(),
        "total_sources": total,
        "enabled": sum(1 for s in summaries if s["enabled"]),
        "ok": sum(1 for s in summaries if s["status"] == "ok"),
        "empty": sum(1 for s in summaries if s["status"] == "empty"),
        "errors": sum(1 for s in summaries if s["status"] == "error"),
        "disabled": sum(1 for s in summaries if s["status"] == "disabled"),
        "total_results": sum(s["results_count"] for s in summaries),
        "sources": summaries,
    }

    with open(os.path.join(OUTPUT_DIR, "_summary.json"), "w") as f:
        json.dump(summary, f, indent=2)

    # Print results
    print(f"\n{'='*72}")
    print(f"  RESULTS")
    print(f"{'='*72}")
    print(f"  Total:    {summary['total_sources']}")
    print(f"  Enabled:  {summary['enabled']}")
    print(f"  ✅ OK:     {summary['ok']}")
    print(f"  ⚠️  Empty: {summary['empty']}")
    print(f"  ❌ Error:  {summary['errors']}")
    print(f"  ⏭  Skip:   {summary['disabled']}")
    print(f"  Results:  {summary['total_results']}")

    cats = {}
    for s in summaries:
        c = s["category"]
        if c not in cats:
            cats[c] = {"ok": 0, "empty": 0, "error": 0, "disabled": 0}
        cats[c][s["status"]] = cats[c].get(s["status"], 0) + 1
    print(f"\n  By Category:")
    for c, v in sorted(cats.items()):
        print(f"    {c:20s}  ✅{v.get('ok',0):2d}  ⚠️{v.get('empty',0):2d}  ❌{v.get('error',0):2d}  ⏭{v.get('disabled',0):2d}")

    # List failed sources
    failed = [s for s in summaries if s["status"] in ("error", "empty") and s["enabled"]]
    if failed:
        print(f"\n  Failed/Empty Sources ({len(failed)}):")
        for s in failed:
            err = (s.get("errors") or [""])[0][:60] if s.get("errors") else "no results"
            print(f"    {s['source']:25s} [{s['status']}] {err}")

    print(f"\n  Saved to: {OUTPUT_DIR}/")
    print(f"{'='*72}")


if __name__ == "__main__":
    asyncio.run(main())
