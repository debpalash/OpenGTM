"""
Data-source reachability tester.

For each registry source it runs the SAME path production uses:
  build_queries(source, query, city)[0]  ->  _ddg_search(dq, max_results=8)
and reports how many results came back. One retry on empty/error to soften
transient DDG rate-limiting.

Usage:
  python scripts/test_sources.py name1 name2 ...      # test specific sources
  python scripts/test_sources.py --all                # test every registry source
Output: a JSON array (one object per source) on stdout. All log noise on stderr.
"""

import asyncio
import json
import os
import sys

# Ensure the project root is importable when run as a script file.
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))

# Quiet the app's noisy loggers so stdout stays pure JSON.
import logging
logging.disable(logging.CRITICAL)

from apps.api.services.leadgen.source_registry import get_source, build_queries, SOURCES
from apps.api.services.leadgen.job_runner import _ddg_search

# Diverse phrases spanning industries + geographies (India/US/EU) so we don't
# only measure one query type. A source "works" if ≥1 phrase returns data.
PHRASES = [
    ("software companies", "Bangalore"),
    ("marketing agency", "New York"),
    ("manufacturing suppliers", "Mumbai"),
    ("consulting firms", "London"),
]


async def test_one(name: str) -> dict:
    src = get_source(name)
    if not src:
        return {"name": name, "works": False, "ok_phrases": 0,
                "total_phrases": len(PHRASES), "per": [], "note": "NO_DEF"}

    per = []
    for q, city in PHRASES:
        queries = build_queries(src, q, city)
        dq = queries[0] if queries else None
        if not dq:
            per.append({"phrase": q, "status": "NO_QUERY", "results": 0})
            continue
        try:
            results = await _ddg_search(dq, max_results=8)
            per.append({"phrase": q, "status": "OK" if results else "EMPTY",
                        "results": len(results)})
        except Exception as e:
            per.append({"phrase": q, "status": "FAIL", "results": 0, "error": str(e)[:80]})

    ok = sum(1 for p in per if p["status"] == "OK")
    return {"name": name, "works": ok > 0, "ok_phrases": ok,
            "total_phrases": len(PHRASES), "per": per}


async def main(names):
    out = []
    for n in names:
        res = await test_one(n)
        out.append(res)
        print(f"  {('WORKS' if res['works'] else 'NONE'):6} {n:24} "
              f"{res['ok_phrases']}/{res['total_phrases']} phrases", file=sys.stderr)
    print(json.dumps(out))


if __name__ == "__main__":
    args = sys.argv[1:]
    if args == ["--all"]:
        names = [s["name"] for s in SOURCES]
    else:
        names = args
    asyncio.run(main(names))
