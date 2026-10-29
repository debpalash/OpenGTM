"""Record classify_status vectors from the REAL Python dsn_parse._classify_status.

Run from legacy/python:
    uv run python ../../apps/backend-go/internal/dsnparse/testdata/gen_classify_vectors.py
Writes classify_vectors.json next to this script (deterministic).
"""
import itertools
import json
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(pathlib.Path.cwd()))

from apps.api.services.outreach import dsn_parse  # noqa: E402

STATUSES = ["", "5.1.1", "5.0.0", "5.", "5", "4.2.2", "4.4.1", "4.", "4", "2.0.0", "2.1.5", "garbage", " 5.1.1", "x5.1.1", "5.1.1 extra", "6.0.0", "0.0.0"]
ACTIONS = ["", "failed", "delayed", "delivered", "relayed", "expanded", "FAILED", "failed ", "unknown", "deferred"]

vectors = [
    {"status": s, "action": a, "kind": dsn_parse._classify_status(s, a)}
    for s, a in itertools.product(STATUSES, ACTIONS)
]
(HERE / "classify_vectors.json").write_text(json.dumps(vectors, indent=2, sort_keys=True) + "\n")
print(len(vectors), "vectors")
