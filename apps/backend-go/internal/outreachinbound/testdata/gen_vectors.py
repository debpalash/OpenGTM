"""Generates backoff_vectors.json from the Python reference formulas in
legacy/python/apps/api/services/outreach/inbound.py (_reschedule, inbound_fire_key).
Run: python3 gen_vectors.py > backoff_vectors.json"""
import json
from datetime import datetime, timezone

def backoff(interval, n):
    return min(interval * (2 ** (n - 1)), 86400)

out = {"backoff": [], "fire_keys": []}
for interval in (1, 60, 900, 3600, 40000):
    for n in range(1, 13):
        out["backoff"].append({"interval": interval, "failures": n, "seconds": backoff(interval, n)})
for ts in (datetime(2026, 10, 26, 10, 0, 0, tzinfo=timezone.utc),
           datetime(2026, 10, 26, 10, 0, 0, 123456, tzinfo=timezone.utc),
           datetime(2026, 1, 2, 3, 4, 5, 100, tzinfo=timezone.utc)):
    out["fire_keys"].append({"unix_micro": int(ts.timestamp()) * 1_000_000 + ts.microsecond,
                             "key": f"inbound:ws-1:{ts.isoformat()}"})
print(json.dumps(out, indent=1))
