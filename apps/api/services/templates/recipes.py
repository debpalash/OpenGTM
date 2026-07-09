"""
Recipe gallery — curated, importable workbook recipes.

Each recipe is a YAML file in ``recipes/`` (same declarative-data style as the
enrichment provider manifests in services/leadgen/enrichment/declarative/
manifests). A recipe is a complete, runnable workbook definition: metadata
(slug / name / description / category / icon) + a ``workbook.columns`` list in
the exact ColumnConfig shape the workbook engine consumes
(apps/api/services/workbook/schemas.py:ColumnConfig).

Everything is validated at LOAD time — column types against COLUMN_TYPES,
columns against the ColumnConfig schema (unknown keys rejected), waterfall /
enrichment provider names against the engine's DEFAULT_WATERFALLS registry,
lead/target fields against LEAD_FIELD_MAP, and source-column channels against
the 91-source leadgen registry — so a bad recipe fails tests, not runtime.

Output columns are intentionally shipped WITHOUT ``destination_config``:
destination credentials (HubSpot token, webhook URL, SMTP) are user-specific
and configured per workspace after import.
"""

from __future__ import annotations

import functools
from pathlib import Path
from typing import Any, Dict, List, Optional

import yaml

RECIPES_DIR = Path(__file__).parent / "recipes"

# Gallery categories — how new users browse recipes.
RECIPE_CATEGORIES: Dict[str, Dict[str, str]] = {
    "list-building": {"label": "List Building", "icon": "radar", "color": "text-blue-400"},
    "crm-hygiene": {"label": "CRM Hygiene", "icon": "sparkles", "color": "text-emerald-400"},
    "outbound": {"label": "Outbound", "icon": "send", "color": "text-violet-400"},
    "research": {"label": "Research", "icon": "search", "color": "text-amber-400"},
    "signals": {"label": "Signals", "icon": "activity", "color": "text-rose-400"},
}

_OUTPUT_DESTINATIONS = {"webhook", "crm", "sequencer"}


class RecipeValidationError(ValueError):
    """A shipped recipe is malformed — raised at load time so tests catch it."""


# ── Reference data (lazy so importing this module stays cheap) ────────────

def _column_types() -> dict:
    from apps.api.services.workbook.models import COLUMN_TYPES
    return COLUMN_TYPES


def _lead_fields() -> dict:
    from apps.api.services.workbook.models import LEAD_FIELD_MAP
    return LEAD_FIELD_MAP


@functools.lru_cache(maxsize=1)
def known_providers() -> frozenset:
    """Every provider name the enrichment engine's default chains reference."""
    from apps.api.services.workbook.enrichment import DEFAULT_WATERFALLS
    names: set = set()
    for chain in DEFAULT_WATERFALLS.values():
        names.update(chain)
    return frozenset(names)


@functools.lru_cache(maxsize=1)
def known_sources() -> frozenset:
    """Source names from the 91-source leadgen registry."""
    from apps.api.services.leadgen.source_registry import SOURCES
    return frozenset(s["name"] for s in SOURCES)


@functools.lru_cache(maxsize=1)
def known_source_categories() -> frozenset:
    from apps.api.services.leadgen.source_registry import SOURCES
    return frozenset(s["category"] for s in SOURCES)


# ── Column validation ─────────────────────────────────────────────────────

def _fail(recipe: str, msg: str) -> None:
    raise RecipeValidationError(f"recipe '{recipe}': {msg}")


def _validate_column(recipe: str, col: Any) -> None:
    """Validate one recipe column against COLUMN_TYPES + the ColumnConfig schema."""
    from apps.api.services.workbook.schemas import ColumnConfig

    if not isinstance(col, dict):
        _fail(recipe, f"column must be a mapping, got {type(col).__name__}")

    col_id = col.get("id") or "?"
    ctype = col.get("type")
    if ctype not in _column_types():
        _fail(recipe, f"column '{col_id}': unknown type '{ctype}' "
                      f"(must be one of {sorted(_column_types())})")

    # Reject unknown keys — pydantic would silently drop them, hiding typos.
    allowed = set(ColumnConfig.model_fields)
    extra = set(col) - allowed
    if extra:
        _fail(recipe, f"column '{col_id}': unknown keys {sorted(extra)}")

    # Schema validation (types, shapes).
    try:
        ColumnConfig.model_validate(col)
    except Exception as e:
        _fail(recipe, f"column '{col_id}': invalid ColumnConfig: {e}")

    # Per-type semantic checks — real provider / field / destination names only.
    if ctype == "lead_field":
        if col.get("lead_field") not in _lead_fields():
            _fail(recipe, f"column '{col_id}': lead_field '{col.get('lead_field')}' "
                          f"is not a Lead field")

    elif ctype == "source":
        icp = col.get("icp") or {}
        if not str(icp.get("description") or "").strip():
            _fail(recipe, f"column '{col_id}': source column needs icp.description")
        channels = col.get("channels") or {}
        bad_src = set(channels.get("explicit_sources") or []) - known_sources()
        if bad_src:
            _fail(recipe, f"column '{col_id}': unknown sources {sorted(bad_src)}")
        bad_cat = set(channels.get("categories") or []) - known_source_categories()
        if bad_cat:
            _fail(recipe, f"column '{col_id}': unknown source categories {sorted(bad_cat)}")

    elif ctype == "enrichment":
        if col.get("provider") not in known_providers():
            _fail(recipe, f"column '{col_id}': unknown provider '{col.get('provider')}'")
        if col.get("target_field") and col["target_field"] not in _lead_fields():
            _fail(recipe, f"column '{col_id}': target_field '{col['target_field']}' "
                          f"is not a Lead field")

    elif ctype == "waterfall":
        chain = col.get("waterfall") or []
        if not chain:
            _fail(recipe, f"column '{col_id}': waterfall column needs a provider chain")
        bad = set(chain) - known_providers()
        if bad:
            _fail(recipe, f"column '{col_id}': unknown waterfall providers {sorted(bad)}")
        if col.get("target_field") and col["target_field"] not in _lead_fields():
            _fail(recipe, f"column '{col_id}': target_field '{col['target_field']}' "
                          f"is not a Lead field")

    elif ctype in ("ai_formula", "research", "agent"):
        if not str(col.get("prompt") or "").strip():
            _fail(recipe, f"column '{col_id}': {ctype} column needs a prompt")
        if ctype == "research":
            steps = col.get("max_steps")
            if steps is not None and not (1 <= int(steps) <= 6):
                _fail(recipe, f"column '{col_id}': max_steps must be 1-6")
            if col.get("output_format") not in (None, "text", "json"):
                _fail(recipe, f"column '{col_id}': output_format must be text|json")

    elif ctype == "output":
        if col.get("destination") not in _OUTPUT_DESTINATIONS:
            _fail(recipe, f"column '{col_id}': destination must be one of "
                          f"{sorted(_OUTPUT_DESTINATIONS)}")

    elif ctype == "formula":
        if not str(col.get("formula") or "").strip():
            _fail(recipe, f"column '{col_id}': formula column needs a formula")

    elif ctype == "http":
        if not str(col.get("http_url") or "").strip():
            _fail(recipe, f"column '{col_id}': http column needs http_url")

    elif ctype == "conditional":
        if not str(col.get("condition") or "").strip():
            _fail(recipe, f"column '{col_id}': conditional column needs a condition")


def validate_recipe(recipe: Any, origin: str = "?") -> Dict[str, Any]:
    """Validate a full recipe dict. Returns it on success, raises otherwise."""
    if not isinstance(recipe, dict):
        raise RecipeValidationError(f"{origin}: recipe must be a mapping")

    slug = recipe.get("slug") or origin
    for key in ("slug", "name", "description", "category", "icon"):
        if not str(recipe.get(key) or "").strip():
            _fail(slug, f"missing required field '{key}'")
    if recipe["category"] not in RECIPE_CATEGORIES:
        _fail(slug, f"unknown category '{recipe['category']}' "
                    f"(must be one of {sorted(RECIPE_CATEGORIES)})")

    workbook = recipe.get("workbook")
    if not isinstance(workbook, dict):
        _fail(slug, "missing 'workbook' definition")
    columns = workbook.get("columns")
    if not isinstance(columns, list) or not columns:
        _fail(slug, "workbook needs a non-empty 'columns' list")

    seen_ids: set = set()
    for col in columns:
        _validate_column(slug, col)
        cid = col["id"]
        if cid in seen_ids:
            _fail(slug, f"duplicate column id '{cid}'")
        seen_ids.add(cid)

    return recipe


# ── Loading ───────────────────────────────────────────────────────────────

@functools.lru_cache(maxsize=1)
def _load() -> tuple:
    recipes: List[Dict[str, Any]] = []
    seen_slugs: set = set()
    for path in sorted(RECIPES_DIR.glob("*.yaml")):
        try:
            raw = yaml.safe_load(path.read_text(encoding="utf-8"))
        except yaml.YAMLError as e:
            raise RecipeValidationError(f"{path.name}: invalid YAML: {e}") from e
        recipe = validate_recipe(raw, origin=path.name)
        if recipe["slug"] in seen_slugs:
            raise RecipeValidationError(f"{path.name}: duplicate slug '{recipe['slug']}'")
        seen_slugs.add(recipe["slug"])
        recipes.append(recipe)
    return tuple(recipes)


def load_recipes(category: Optional[str] = None) -> List[Dict[str, Any]]:
    """All shipped recipes (validated), optionally filtered by category."""
    recipes = list(_load())
    if category:
        recipes = [r for r in recipes if r["category"] == category]
    return recipes


def get_recipe(slug: str) -> Optional[Dict[str, Any]]:
    """One recipe by slug, or None."""
    for r in _load():
        if r["slug"] == slug:
            return r
    return None


def recipe_summary(recipe: Dict[str, Any]) -> Dict[str, Any]:
    """Gallery-card view of a recipe — metadata + column-mix stats, no full config."""
    columns = recipe["workbook"]["columns"]
    type_counts: Dict[str, int] = {}
    for c in columns:
        type_counts[c["type"]] = type_counts.get(c["type"], 0) + 1
    return {
        "slug": recipe["slug"],
        "name": recipe["name"],
        "description": recipe["description"],
        "category": recipe["category"],
        "icon": recipe["icon"],
        "columns_count": len(columns),
        "column_types": type_counts,
    }
