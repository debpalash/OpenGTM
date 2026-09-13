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
from typing import Callable, Dict, List, Optional

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
    for ref in col.get("input_columns") or []:
        if isinstance(ref, str) and ref.strip():
            refs.add(ref.strip())
    # http_headers values + http_body (stringified) can also reference columns
    def strings(value):
        if isinstance(value, str):
            yield value
        elif isinstance(value, dict):
            for child in value.values():
                yield from strings(child)
        elif isinstance(value, (list, tuple)):
            for child in value:
                yield from strings(child)

    for f in ("http_headers", "http_body", "destination_config"):
        v = col.get(f)
        if v is not None:
            for value in strings(v):
                refs.update(m.strip() for m in _REF_RE.findall(value))
    return refs


def downstream_columns(
    cols: List[dict],
    changed_fields: set[str],
    eligible: Optional[Callable[[dict], bool]] = None,
) -> List[dict]:
    """Return the transitive dependants of edited fields in execution order.

    An edited key may address an input by id, display name, or lead_field. Each
    matched derived column contributes its own aliases, allowing A -> B -> C
    chains to propagate without requiring an explicit dependency schema.
    """
    tainted = {str(field).strip().lower() for field in changed_fields if str(field).strip()}
    selected: list[dict] = []
    for col in topo_sort_columns(cols):
        refs = {str(ref).strip().lower() for ref in _refs_in(col)}
        if not refs.intersection(tainted):
            continue
        if eligible is not None and not eligible(col):
            continue
        selected.append(col)
        for key in ("id", "name", "lead_field", "target_field"):
            value = col.get(key)
            if value:
                tainted.add(str(value).strip().lower())
    return selected


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
