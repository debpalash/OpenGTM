"""Saved-view naming UI; intercepted APIs only, no real mutations."""
from urllib.parse import urlparse
from pathlib import Path
import gzip
import json
import argparse
from playwright.sync_api import sync_playwright, expect

parser = argparse.ArgumentParser()
parser.add_argument("--url", default="http://127.0.0.1:4399")
parser.add_argument("--dist", type=Path, default=Path(__file__).resolve().parents[3] / "apps/web/dist")
parser.add_argument("--measure-only", action="store_true")
args = parser.parse_args()

views, calls, errors = [], [], []
state = {"fail": True, "list_error": not args.measure_only}

def fixture(route):
    path = urlparse(route.request.url).path
    method = route.request.method
    if not path.startswith(("/api/", "/auth/")):
        return route.continue_()
    if path.startswith("/api/v2/workbooks/views/views"):
        if method == "GET":
            if state["list_error"]:
                return route.fulfill(status=503, json={"detail": "Unavailable"})
            return route.fulfill(json={"views": views, "total": len(views)})
        calls.append((method, route.request.post_data_json if method != "DELETE" else None))
        if state["fail"]:
            return route.fulfill(status=503, json={"detail": "Fixture failure"})
        if method == "POST":
            views.append({"id": "saved", "workbook_id": "views", **route.request.post_data_json})
        elif method == "PUT":
            views[0].update(route.request.post_data_json)
        elif method == "DELETE":
            views.clear()
            return route.fulfill(json={"status": "deleted"})
        else:
            raise AssertionError(f"Unexpected mutation {method}")
        return route.fulfill(json=views[0])
    assert method == "GET", (method, path)
    if path == "/api/events":
        return route.fulfill(content_type="text/event-stream", body=": fixture\n\n")
    data = {
        "/auth/me": {"id": 1, "username": "Fixture", "is_admin": True, "role": "admin"},
        "/api/workspaces": {"workspaces": [{"id": "fixture", "name": "Fixture", "slug": "fixture"}], "active_id": "fixture"},
        "/api/copilotkit/conversations": {"conversations": []},
        "/api/settings/llm-usage": {"providers": [], "today_calls": 0, "today_tokens": 0},
        "/api/jobs": [], "/api/leads": [],
        "/api/workbooks/views": {"workbook": {"id": "views", "name": "Views fixture", "status": "draft", "columns_config": [{"id": "company", "name": "Company", "type": "input", "width": 180}], "total_rows": 0, "completed_rows": 0}, "rows": [], "total_rows": 0, "page": 1},
        "/api/workbooks/meta/providers": {"providers": []},
        "/api/workbooks/meta/ai-column-presets": {"presets": [], "categories": []},
        "/api/workbooks/views/connector-runs": {"runs": []},
        "/api/workbooks/views/cost": {"budget_max_usd": 0, "budget_spent_usd": 0},
        "/api/workbooks/views/run/estimate": {"rows": 0, "best_usd": 0, "worst_usd": 0, "breakdown": [], "note": "Fixture"},
    }
    assert path in data, path
    route.fulfill(json=data[path])

with sync_playwright() as p:
    browser = p.chromium.launch(executable_path="/usr/bin/chromium", headless=True)
    context = browser.new_context(viewport={"width": 1440, "height": 900}, reduced_motion="reduce")
    context.route("**/*", fixture)
    context.route_web_socket("**/*", lambda ws: None)
    context.add_init_script("localStorage.setItem('yupcha_token','fixture-only'); localStorage.setItem('yupcha_workspace_id','fixture')")
    page = context.new_page()
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.goto(args.url + "/workbooks/views")
    expect(page.get_by_role("button", name="All rows", exact=True)).to_be_visible()
    # Count resources this route actually requested, not arbitrary chunk names.
    # Gzip is a reproducible estimate; the local preview may serve uncompressed.
    resource_urls = page.evaluate("performance.getEntriesByType('resource').map(entry => entry.name)")
    dist = args.dist
    asset_paths = sorted({urlparse(url).path for url in resource_urls if urlparse(url).path.startswith("/assets/")})
    metrics = {"js": {"bytes": 0, "gzip_bytes": 0, "files": 0}, "css": {"bytes": 0, "gzip_bytes": 0, "files": 0}}
    for asset in asset_paths:
        kind = asset.rsplit(".", 1)[-1]
        if kind not in metrics:
            continue
        content = (dist / asset.lstrip("/")).read_bytes()
        metrics[kind]["bytes"] += len(content)
        metrics[kind]["gzip_bytes"] += len(gzip.compress(content, mtime=0))
        metrics[kind]["files"] += 1
    print(json.dumps({"workbook_route_payload": metrics, "assets": asset_paths}))
    if args.measure_only:
        assert not errors, errors
        browser.close()
        raise SystemExit(0)
    page.get_by_role("button", name="All rows", exact=True).click()
    expect(page.get_by_role("alert").filter(has_text="Could not load saved views")).to_be_visible(timeout=15000)
    expect(page.get_by_role("menuitem", name="New view")).to_be_disabled()
    state["list_error"] = False
    page.get_by_role("menuitem", name="Retry saved views", exact=True).click()
    page.get_by_role("button", name="All rows", exact=True).click()
    expect(page.get_by_role("menuitem", name="New view")).to_be_enabled()
    page.get_by_role("menuitem", name="New view").click()
    dialog = page.get_by_role("dialog", name="New view", exact=True)
    field = dialog.get_by_label("View name", exact=True)
    field.fill("Partnerships")
    field.evaluate("el => { for (let i = 0; i < 2; i++) el.dispatchEvent(new KeyboardEvent('keydown', {key: 'Enter', bubbles: true})); }")
    expect(dialog.get_by_role("alert")).to_be_visible()
    expect(field).to_have_value("Partnerships")
    assert len(calls) == 1
    state["fail"] = False
    dialog.get_by_role("button", name="Create", exact=True).click()
    expect(dialog).not_to_be_visible()
    expect(page.get_by_role("button", name="Partnerships", exact=True)).to_be_focused()
    page.get_by_role("button", name="Partnerships", exact=True).click()
    page.get_by_role("menuitem", name="Rename view", exact=True).click()
    dialog = page.get_by_role("dialog", name="Rename view", exact=True)
    field = dialog.get_by_label("View name", exact=True)
    expect(field).to_have_value("Partnerships")
    field.fill("Strategic partners")
    state["fail"] = True
    field.press("Enter")
    expect(dialog.get_by_role("alert")).to_be_visible()
    expect(field).to_have_value("Strategic partners")
    state["fail"] = False
    for theme in ("light", "dark"):
        page.evaluate("theme => window.OpenGTMTheme.setPreference(theme)", theme)
        page.set_viewport_size({"width": 375, "height": 667})
        expect(dialog.get_by_role("button", name="Rename", exact=True)).to_be_in_viewport()
    dialog.get_by_role("button", name="Rename", exact=True).click()
    expect(dialog).not_to_be_visible()
    expect(page.get_by_role("button", name="Strategic partners", exact=True)).to_be_visible()
    expect(page.get_by_role("button", name="Strategic partners", exact=True)).to_be_focused()
    assert len(calls) == 4, calls
    page.get_by_role("button", name="Filter", exact=True).click()
    expect(page.get_by_role("dialog", name="View filters", exact=True)).to_be_visible()
    page.get_by_role("button", name="Add filter", exact=True).click()
    page.get_by_label("Filter 1 value", exact=True).fill("Acme")
    state["fail"] = True
    page.get_by_role("button", name="Apply & save", exact=True).click()
    expect(page.get_by_role("alert").filter(has_text="Could not save filters")).to_be_visible()
    expect(page.get_by_label("Filter 1 value", exact=True)).to_have_value("Acme")
    state["fail"] = False
    page.get_by_role("button", name="Apply & save", exact=True).click()
    expect(page.get_by_label("Filter 1 value", exact=True)).not_to_be_visible()
    expect(page.get_by_role("button", name="1 filter", exact=True)).to_be_visible()
    expect(page.get_by_role("button", name="1 filter", exact=True)).to_be_focused()
    assert calls[-1][1]["config"]["filters"] == [{"column": "company", "op": "contains", "value": "Acme"}]
    before_cancel = len(calls)
    page.get_by_role("button", name="1 filter", exact=True).click()
    page.get_by_label("Filter 1 value", exact=True).fill("Unsaved")
    page.keyboard.press("Escape")
    expect(page.get_by_role("button", name="1 filter", exact=True)).to_be_focused()
    page.get_by_role("button", name="1 filter", exact=True).click()
    expect(page.get_by_label("Filter 1 value", exact=True)).to_have_value("Acme")
    page.get_by_role("dialog", name="View filters", exact=True).get_by_role("button", name="Cancel", exact=True).click()
    assert len(calls) == before_cancel
    page.get_by_role("button", name="Strategic partners", exact=True).click()
    page.get_by_role("menuitem", name="Delete view", exact=True).click()
    confirmation = page.get_by_role("alertdialog", name="Delete saved view?", exact=True)
    expect(confirmation).to_contain_text("Strategic partners")
    confirmation.get_by_role("button", name="Cancel", exact=True).click()
    expect(page.get_by_role("button", name="Strategic partners", exact=True)).to_be_focused()
    assert len(calls) == before_cancel
    page.get_by_role("button", name="Strategic partners", exact=True).click()
    page.get_by_role("menuitem", name="Delete view", exact=True).click()
    state["fail"] = True
    confirmation.get_by_role("button", name="Delete view", exact=True).click()
    expect(confirmation.get_by_role("alert")).to_be_visible()
    state["fail"] = False
    confirmation.get_by_role("button", name="Delete view", exact=True).click()
    expect(confirmation).not_to_be_visible()
    expect(page.get_by_role("button", name="All rows", exact=True)).to_be_visible()
    expect(page.get_by_role("button", name="All rows", exact=True)).to_be_focused()
    assert [method for method, _ in calls[-2:]] == ["DELETE", "DELETE"]
    assert not errors, errors
    browser.close()
print("PASS: saved-view create/rename, retained drafts, failure/retry, both themes at mobile width; fixture APIs")
