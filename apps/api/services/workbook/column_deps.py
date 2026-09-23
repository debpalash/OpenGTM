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
from typing import Callable, List, Optional

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


def reference_indexes(ref: str, columns: List[dict]) -> set[int]:
    """Match stable IDs first, then exact/case/space-normalized aliases."""
    exact_ids = {i for i, column in enumerate(columns) if column.get("id") == ref}
    if exact_ids:
        return exact_ids
    for normalize in (lambda value: value, str.lower, lambda value: value.lower().replace("_", " ")):
        matches = {i for i, column in enumerate(columns)
                   if any(isinstance(column.get(key), str) and normalize(column[key]) == normalize(ref)
                          for key in ("id", "name"))}
        if matches:
            return matches
    return set()


def referencing_columns(cols: List[dict], column: dict) -> List[dict]:
    """Direct references by ID/display name, including ambiguous aliases.

    Deletion must not choose a different provider of a duplicated name silently.
    Cycles and config order do not affect this direct-reference check.
    """
    normalize = lambda value: str(value).strip().lower().replace("_", " ")
    aliases = {normalize(column[key]) for key in ("id", "name") if column.get(key)}
    return [candidate for candidate in cols
            if candidate.get("id") != column.get("id")
            and aliases.intersection(normalize(ref) for ref in _refs_in(candidate))]


def broken_rename_references(before: List[dict], after: List[dict]) -> List[dict]:
    """Retained columns referencing a display-name alias removed by a rename.

    ID references stay valid. A duplicate alias elsewhere does not make silently
    redirecting an existing reference safe, so it is not used as a fallback.
    """
    old_by_id = {column.get("id"): column for column in before}
    normalize = lambda value: str(value).strip().lower().replace("_", " ")
    lost = set()
    for column in after:
        old = old_by_id.get(column.get("id"))
        if not old or not old.get("name"):
            continue
        name = normalize(old["name"])
        aliases = {normalize(column[key]) for key in ("id", "name") if column.get(key)}
        if name not in aliases:
            lost.add(name)
    return [column for column in after
            if lost.intersection(normalize(ref) for ref in _refs_in(column))]


def cycle_blocked_columns(columns: List[dict]) -> set:
    """IDs in cycles or downstream of cycles, including self references."""
    executable = {"enrichment", "waterfall", "ai_formula", "research", "agent", "http", "formula", "output"}
    cols = columns
    deps = []
    for current, col in enumerate(cols):
        # A condition may inspect its own existing value (e.g. email == "")
        # to decide whether to run. That is not a circular computation.
        computation_refs = _refs_in({key: value for key, value in col.items() if key != "condition"})
        edges = {index for ref in computation_refs for index in reference_indexes(ref, cols)}
        edges.update(index for ref in _refs_in(col) for index in reference_indexes(ref, cols) if index != current)
        deps.append(edges)
    remaining = {index for index, col in enumerate(cols) if col.get("type") in executable}
    while True:
        ready = {index for index in remaining if not deps[index].intersection(remaining)}
        if not ready:
            return {cols[index].get("id") for index in remaining}
        remaining -= ready


def unavailable_computed_dependencies(column: dict, columns: List[dict], row: dict) -> List[str]:
    """Require a value for referenced executable columns, including partial runs.

    Zero and False are values. Input fields are not computed dependencies; their
    validation belongs to the destination/provider. IDs take precedence over
    display aliases, including an explicitly null ID value.
    """
    executable = {"enrichment", "waterfall", "ai_formula", "research", "agent", "http", "formula", "output"}
    missing = []
    referenced = {index for ref in _refs_in(column) for index in reference_indexes(ref, columns)}
    for index, upstream in enumerate(columns):
        if index not in referenced or upstream.get("type") not in executable or upstream.get("id") == column.get("id"):
            continue
        cid, name = upstream.get("id"), upstream.get("name")
        value = row[cid] if cid in row else row.get(name)
        if value is None:
            missing.append(cid)
    return missing


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
    normalize = lambda value: str(value).strip().lower().replace("_", " ")
    tainted = {normalize(field) for field in changed_fields if str(field).strip()}
    tainted_indexes = {index for field in changed_fields for index in reference_indexes(str(field).strip(), cols)}
    tainted_indexes.update(index for index, col in enumerate(cols)
                           if col.get("lead_field") and normalize(col["lead_field"]) in tainted)
    selected: list[dict] = []
    for col in topo_sort_columns(cols):
        affected = False
        for ref in _refs_in(col):
            matches = reference_indexes(ref, cols)
            if (matches.intersection(tainted_indexes) if matches else normalize(ref) in tainted):
                affected = True
                break
        if not affected:
            continue
        if eligible is not None and not eligible(col):
            continue
        selected.append(col)
        tainted_indexes.update(index for index, candidate in enumerate(cols) if candidate.get("id") == col.get("id"))
        for key in ("id", "name", "lead_field", "target_field"):
            value = col.get(key)
            if value:
                tainted.add(normalize(value))
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

    n = len(cols)
    # deps[i] = set of indices that i depends on (must run before i)
    deps: List[set] = [set() for _ in range(n)]
    for i, c in enumerate(cols):
        for ref in _refs_in(c):
            deps[i].update(j for j in reference_indexes(ref, cols) if j != i)

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

    n = len(cols)
    refs_out = [set() for _ in range(n)]   # cols i depends on
    refs_in = [False] * n                  # whether some col references i
    for i, c in enumerate(cols):
        for ref in _refs_in(c):
            for j in reference_indexes(ref, cols):
                if j != i:
                    refs_out[i].add(j)
                    refs_in[j] = True

    blocked = cycle_blocked_columns(cols)
    return [cols[i] for i in range(n) if not refs_out[i] and not refs_in[i] and cols[i].get("id") not in blocked]
