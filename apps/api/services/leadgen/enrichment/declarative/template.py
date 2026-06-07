"""
Template + response-mapping mini-language for declarative provider manifests.

Two small, self-contained renderers (ported from YALC's compiler.ts semantics):

1. render_template(value, ctx) — substitutes, in any string within a nested
   structure:
     - {{input.field}}  / {{input.field | default: foo}}  (Mustache-lite)
     - ${env:VAR}       (environment / settings lookup, via env_resolver)
   Non-string leaves pass through unchanged.

2. project_response(data, mappings) — JSONPath-lite projection of a provider's
   JSON response onto the capability's flat output fields:
     - "$.data.work_email"        dot path from root
     - "$.results[0].email"       explicit index
     - "$.results[].email"        array fan-out → first non-empty
     - "https://$.handle"         literal prefix + path
"""

import re
from typing import Any, Callable, Dict, Optional

_VAR_RE = re.compile(r"\{\{\s*([^}|]+?)\s*(?:\|\s*default:\s*([^}]*?)\s*)?\}\}")
_ENV_RE = re.compile(r"\$\{env:([A-Za-z0-9_]+)\}")


def _lookup(path: str, ctx: Dict[str, Any]) -> Any:
    """Resolve a dotted path like `input.first_name` against ctx."""
    cur: Any = ctx
    for part in path.strip().split("."):
        if isinstance(cur, dict):
            cur = cur.get(part)
        else:
            cur = getattr(cur, part, None)
        if cur is None:
            return None
    return cur


def render_string(s: str, ctx: Dict[str, Any], env_resolver: Callable[[str], str]) -> str:
    """Render {{input.x|default:y}} and ${env:VAR} in a single string."""
    def _sub_var(m: re.Match) -> str:
        path, default = m.group(1), m.group(2)
        val = _lookup(path, ctx)
        if val is None or val == "":
            return (default or "").strip().strip("'\"")
        return str(val)

    def _sub_env(m: re.Match) -> str:
        return env_resolver(m.group(1)) or ""

    s = _VAR_RE.sub(_sub_var, s)
    s = _ENV_RE.sub(_sub_env, s)
    return s


def render_template(value: Any, ctx: Dict[str, Any], env_resolver: Callable[[str], str]) -> Any:
    """Recursively render every string leaf in a nested dict/list structure."""
    if isinstance(value, str):
        return render_string(value, ctx, env_resolver)
    if isinstance(value, dict):
        return {k: render_template(v, ctx, env_resolver) for k, v in value.items()}
    if isinstance(value, list):
        return [render_template(v, ctx, env_resolver) for v in value]
    return value


def _walk_path(data: Any, path: str) -> Any:
    """Walk a JSONPath-lite path (no leading `$.`). Supports `[]` fan-out and `[i]`."""
    cur = data
    for raw in path.split("."):
        if cur is None:
            return None
        # token may be `key`, `key[]`, `key[2]`, or `[2]`
        m = re.match(r"^([A-Za-z0-9_]*)(?:\[(\d*)\])?$", raw)
        if not m:
            return None
        key, idx = m.group(1), m.group(2)
        if key:
            cur = cur.get(key) if isinstance(cur, dict) else None
        if m.group(0).endswith("]") or idx is not None or raw.endswith("[]"):
            if not isinstance(cur, list):
                return None
            if idx == "" or idx is None:
                # fan-out: return first non-empty element later; for scalar
                # projection we take the first element
                cur = cur[0] if cur else None
            else:
                i = int(idx)
                cur = cur[i] if 0 <= i < len(cur) else None
    return cur


def project_value(data: Any, expr: str) -> Optional[Any]:
    """Resolve one mapping expression against the response JSON.

    Handles a literal prefix before the `$.` path, e.g. "https://$.handle".
    """
    if not isinstance(expr, str):
        return expr
    idx = expr.find("$.")
    if idx == -1:
        return expr  # static literal
    prefix = expr[:idx]
    path = expr[idx + 2:]
    val = _walk_path(data, path) if path else data
    if val is None:
        return None
    if prefix:
        return f"{prefix}{val}"
    return val


def project_response(data: Any, mappings: Dict[str, str]) -> Dict[str, Any]:
    """Project the response onto {output_field: value} using the mappings."""
    out: Dict[str, Any] = {}
    for field_name, expr in mappings.items():
        val = project_value(data, expr)
        if val is not None and val != "":
            out[field_name] = val
    return out
