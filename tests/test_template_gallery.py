"""Template/recipe gallery tests (SQLite, offline).

Covers:
  - every shipped recipe loads + validates against COLUMN_TYPES and the
    ColumnConfig schema (bad recipes fail HERE, not at runtime),
  - recipe reference-data checks (real provider / source / lead-field names),
  - the validator rejects malformed recipes,
  - GET /api/templates/gallery (list + categories, category filter),
  - GET /api/templates/gallery/{slug} (detail, 404),
  - POST /api/templates/gallery/{slug}/instantiate creates a workbook with the
    recipe's columns in the caller's workspace (name override, 404).
"""

import copy

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.database import Base, get_db
from apps.api.routers.templates import router as templates_router
from apps.api.services.templates.recipes import (
    RECIPE_CATEGORIES,
    RecipeValidationError,
    get_recipe,
    known_providers,
    known_sources,
    load_recipes,
    validate_recipe,
)
from apps.api.services.workbook.models import (
    COLUMN_TYPES,
    LEAD_FIELD_MAP,
    Workbook,
    WorkbookEnrichment,
    WorkbookRow,
)
from apps.api.services.workbook.schemas import ColumnConfig

WS = "ws_gallery"

RECIPES = load_recipes()
SLUGS = [r["slug"] for r in RECIPES]


# ── Recipe catalog: load + validate ───────────────────────────────────────

def test_catalog_has_at_least_15_recipes():
    assert len(RECIPES) >= 15


def test_slugs_unique():
    assert len(SLUGS) == len(set(SLUGS))


def test_every_gallery_category_has_a_recipe():
    used = {r["category"] for r in RECIPES}
    assert used == set(RECIPE_CATEGORIES)


@pytest.mark.parametrize("slug", SLUGS)
def test_recipe_validates(slug):
    """Each shipped recipe passes full validation (raises on any problem)."""
    recipe = get_recipe(slug)
    validate_recipe(recipe, origin=slug)


@pytest.mark.parametrize("slug", SLUGS)
def test_recipe_columns_match_column_config_schema(slug):
    """Every column is a known type and round-trips through ColumnConfig
    without losing any keys (i.e. the pydantic schema fully covers it)."""
    recipe = get_recipe(slug)
    for col in recipe["workbook"]["columns"]:
        assert col["type"] in COLUMN_TYPES, f"{slug}: bad type {col['type']}"
        parsed = ColumnConfig.model_validate(col)
        dumped = parsed.model_dump(exclude_none=True)
        for key, value in col.items():
            assert dumped.get(key) == value, (
                f"{slug}: column '{col['id']}' key '{key}' lost/changed by schema"
            )


@pytest.mark.parametrize("slug", SLUGS)
def test_recipe_references_real_names(slug):
    """Providers, sources and lead fields in recipes exist in the codebase."""
    recipe = get_recipe(slug)
    for col in recipe["workbook"]["columns"]:
        if col["type"] == "lead_field":
            assert col["lead_field"] in LEAD_FIELD_MAP
        if col.get("target_field"):
            assert col["target_field"] in LEAD_FIELD_MAP
        for p in col.get("waterfall") or []:
            assert p in known_providers(), f"{slug}: unknown provider {p}"
        if col["type"] == "enrichment":
            assert col["provider"] in known_providers()
        if col["type"] == "source":
            channels = col.get("channels") or {}
            for s in channels.get("explicit_sources") or []:
                assert s in known_sources(), f"{slug}: unknown source {s}"


def _valid_recipe_dict():
    return copy.deepcopy(get_recipe(SLUGS[0]))


def test_validator_rejects_unknown_column_type():
    bad = _valid_recipe_dict()
    bad["workbook"]["columns"][0]["type"] = "not_a_type"
    with pytest.raises(RecipeValidationError, match="unknown type"):
        validate_recipe(bad)


def test_validator_rejects_unknown_provider():
    bad = _valid_recipe_dict()
    bad["workbook"]["columns"].append({
        "id": "x", "name": "X", "type": "waterfall",
        "target_field": "email", "waterfall": ["definitely_fake_provider"],
    })
    with pytest.raises(RecipeValidationError, match="unknown waterfall providers"):
        validate_recipe(bad)


def test_validator_rejects_unknown_keys():
    bad = _valid_recipe_dict()
    bad["workbook"]["columns"][0]["watrfall"] = ["deep_scraper"]  # typo'd key
    with pytest.raises(RecipeValidationError, match="unknown keys"):
        validate_recipe(bad)


def test_validator_rejects_bad_category():
    bad = _valid_recipe_dict()
    bad["category"] = "growth-hax"
    with pytest.raises(RecipeValidationError, match="unknown category"):
        validate_recipe(bad)


def test_validator_rejects_duplicate_column_ids():
    bad = _valid_recipe_dict()
    bad["workbook"]["columns"].append(dict(bad["workbook"]["columns"][1]))
    with pytest.raises(RecipeValidationError, match="duplicate column id"):
        validate_recipe(bad)


def test_output_columns_ship_without_destination_config():
    """Destination credentials are user-specific — recipes must not embed them."""
    for r in RECIPES:
        for col in r["workbook"]["columns"]:
            if col["type"] == "output":
                assert "destination_config" not in col, r["slug"]
                assert col["destination"] in ("webhook", "crm", "sequencer")


# ── API: gallery endpoints ────────────────────────────────────────────────

class _User:
    id = "user-1"


@pytest.fixture()
def client():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[
        Workbook.__table__, WorkbookRow.__table__, WorkbookEnrichment.__table__,
    ])
    Session = sessionmaker(bind=engine)

    app = FastAPI()
    app.include_router(templates_router)

    def _override_db():
        s = Session()
        try:
            yield s
        finally:
            s.close()

    def _override_ws():
        return WorkspaceCtx(user=_User(), workspace_id=WS, slug="gallery")

    app.dependency_overrides[get_db] = _override_db
    app.dependency_overrides[current_workspace] = _override_ws
    return TestClient(app), Session


def test_gallery_list(client):
    c, _ = client
    res = c.get("/api/templates/gallery")
    assert res.status_code == 200
    data = res.json()
    assert data["total"] == len(RECIPES)
    assert set(data["categories"]) == set(RECIPE_CATEGORIES)
    card = data["recipes"][0]
    assert {"slug", "name", "description", "category", "icon",
            "columns_count", "column_types"} <= set(card)
    # summaries never include full column configs
    assert "workbook" not in card


def test_gallery_list_category_filter(client):
    c, _ = client
    res = c.get("/api/templates/gallery", params={"category": "outbound"})
    assert res.status_code == 200
    data = res.json()
    assert data["total"] >= 1
    assert all(r["category"] == "outbound" for r in data["recipes"])

    res = c.get("/api/templates/gallery", params={"category": "nope"})
    assert res.status_code == 404


def test_gallery_detail(client):
    c, _ = client
    res = c.get(f"/api/templates/gallery/{SLUGS[0]}")
    assert res.status_code == 200
    data = res.json()
    assert data["slug"] == SLUGS[0]
    assert data["workbook"]["columns"]


def test_gallery_detail_unknown_slug_404(client):
    c, _ = client
    assert c.get("/api/templates/gallery/does-not-exist").status_code == 404


def test_instantiate_creates_workbook_in_workspace(client):
    c, Session = client
    slug = "saas-founders-india"
    recipe = get_recipe(slug)
    res = c.post(f"/api/templates/gallery/{slug}/instantiate", json={})
    assert res.status_code == 201, res.text
    data = res.json()
    assert data["name"] == recipe["name"]

    s = Session()
    wb = s.query(Workbook).filter(Workbook.id == data["id"]).one()
    assert wb.workspace_id == WS
    got_cols = wb.columns_config
    want_cols = recipe["workbook"]["columns"]
    assert [c_["id"] for c_ in got_cols] == [c_["id"] for c_ in want_cols]
    assert [c_["type"] for c_ in got_cols] == [c_["type"] for c_ in want_cols]
    # Full configs survive the creation path (waterfalls, prompts, source ICP).
    by_id = {c_["id"]: c_ for c_ in got_cols}
    for want in want_cols:
        got = by_id[want["id"]]
        for key, value in want.items():
            assert got.get(key) == value, f"column {want['id']} lost '{key}'"
    # No rows yet — recipes start empty (source columns materialize rows).
    assert s.query(WorkbookRow).filter(WorkbookRow.workbook_id == wb.id).count() == 0
    s.close()


def test_instantiate_with_name_override(client):
    c, Session = client
    res = c.post(
        "/api/templates/gallery/cold-email-personalization/instantiate",
        json={"name": "My outbound run"},
    )
    assert res.status_code == 201, res.text
    assert res.json()["name"] == "My outbound run"
    s = Session()
    wb = s.query(Workbook).filter(Workbook.id == res.json()["id"]).one()
    assert wb.name == "My outbound run"
    assert wb.workspace_id == WS
    s.close()


def test_instantiate_unknown_slug_404(client):
    c, _ = client
    res = c.post("/api/templates/gallery/does-not-exist/instantiate", json={})
    assert res.status_code == 404


def test_instantiate_body_optional(client):
    c, _ = client
    res = c.post("/api/templates/gallery/founder-outreach/instantiate")
    assert res.status_code == 201, res.text


# ── Legacy templates endpoint still works alongside the gallery ──────────

def test_legacy_templates_list_unaffected(client):
    c, _ = client
    res = c.get("/api/templates")
    assert res.status_code == 200
    assert res.json()["total"] > 0
