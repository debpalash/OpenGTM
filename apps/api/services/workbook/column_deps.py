"""
Column dependency ordering for workbook runs.

Ported from eliasstravik/rowbound (core/action-deps.ts). A column that references
another column via {col} in any of its templated fields must run AFTER that
column, so the dependent cell sees the produced value. We build a DAG from the
references and topologically sort; on a cycle we fall back to config order (and
log), never deadlock.
"""

import logging
import re
from typing import Dict, List

logger = logging.getLogger("workbook.column_deps")

_REF_RE = re.compile(r"\{([^}]+)\}")

# Column config fields that may contain {col} references.
_TEMPLATED_FIELDS = ("prompt", "formula", "http_url", "condition", "goal")


def _refs_in(col: dict) -> set:
    """All {placeholder} names referenced anywhere in a column's templated config."""
    refs = set()
    for f in _TEMPLATED_FIELDS:
        v = col.get(f)
        if isinstance(v, str):
            refs.update(m.strip() for m in _REF_RE.findall(v))
    # http_headers values + http_body (stringified) can also reference columns
    for f in ("http_headers", "http_body"):
        v = col.get(f)
        if v is not None:
            import json
            try:
                refs.update(m.strip() for m in _REF_RE.findall(json.dumps(v)))
            except Exception:
                pass
    return refs


def topo_sort_columns(cols: List[dict]) -> List[dict]:
    """Return cols ordered so that a column referencing another comes after it.

    References resolve by column id OR display name (case-insensitive), matching
    the {column} resolver. Columns not in the set (e.g. lead_field inputs) are
    ignored as dependencies — they already exist on the row. Stable: preserves
    config order among independent columns; falls back to config order on cycle.
    """
    if len(cols) <= 1:
        return list(cols)

    # map both id and lowercased name → index
    id_of: Dict[str, int] = {}
    for i, c in enumerate(cols):
        cid = c.get("id")
        if cid:
            id_of[cid] = i
            id_of[cid.lower()] = i
        name = c.get("name")
        if name:
            id_of.setdefault(name.lower(), i)

    n = len(cols)
    # deps[i] = set of indices that i depends on (must run before i)
    deps: List[set] = [set() for _ in range(n)]
    for i, c in enumerate(cols):
        for ref in _refs_in(c):
            j = id_of.get(ref, id_of.get(ref.lower()))
            if j is not None and j != i:
                deps[i].add(j)

    # Kahn topological sort, breaking ties by original index (stable).
    indeg = [len(deps[i]) for i in range(n)]
    ready = sorted([i for i in range(n) if indeg[i] == 0])
    out: List[int] = []
    # successors
    succ: List[set] = [set() for _ in range(n)]
    for i in range(n):
        for j in deps[i]:
            succ[j].add(i)

    while ready:
        i = ready.pop(0)
        out.append(i)
        newly = []
        for k in sorted(succ[i]):
            indeg[k] -= 1
            if indeg[k] == 0:
                newly.append(k)
        # keep `ready` sorted for stable output
        for k in newly:
            ready.append(k)
        ready.sort()

    if len(out) != n:
        logger.warning("column dependency cycle detected — falling back to config order")
        return list(cols)
    return [cols[i] for i in out]


def independent_columns(cols: List[dict]) -> List[dict]:
    """Columns with no dependency edges to/from any other column in the set.

    A column is "independent" iff it neither references another run column nor is
    referenced by one. These are safe to run out-of-band (e.g. as a batch
    pre-pass) without breaking the row-major value-threading the runner relies on
    for derived columns. References resolve by id OR display name, matching the
    {column} resolver.
    """
    if not cols:
        return []

    id_of: Dict[str, int] = {}
    for i, c in enumerate(cols):
        cid = c.get("id")
        if cid:
            id_of[cid] = i
            id_of[cid.lower()] = i
        name = c.get("name")
        if name:
            id_of.setdefault(name.lower(), i)

    n = len(cols)
    refs_out = [set() for _ in range(n)]   # cols i depends on
    refs_in = [False] * n                  # whether some col references i
    for i, c in enumerate(cols):
        for ref in _refs_in(c):
            j = id_of.get(ref, id_of.get(ref.lower()))
            if j is not None and j != i:
                refs_out[i].add(j)
                refs_in[j] = True

    return [cols[i] for i in range(n) if not refs_out[i] and not refs_in[i]]
