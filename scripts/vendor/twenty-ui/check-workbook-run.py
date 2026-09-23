"""Workbook run review/stop interactions against isolated API fixtures."""
import argparse
import json
from pathlib import Path
from urllib.parse import urlparse, parse_qs
from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser()
parser.add_argument("--url", default="http://127.0.0.1:4399")
parser.add_argument("--screenshots", type=Path)
args = parser.parse_args()
state = {"estimate_error": True, "run_error": True, "stop_error": True, "rows": 2, "running": False}
runs, estimates, stops, errors, unexpected = [], [], [], [], []
columns = [{"id": "company", "name": "Company", "type": "input", "lead_field": "company"}, {"id": "push", "name": "CRM export", "type": "output"}]

def fixture(route):
    url = urlparse(route.request.url)
    path = url.path.rstrip("/")
    if not path.startswith(("/api/", "/auth/")):
        route.continue_()
        return
    if path == "/api/workbooks/review":
        rows = [{"row_id": n, "lead_id": None, "data": {"company": f"Acme {n}"}, "lead": {}, "enrichments": {}} for n in range(1, state["rows"] + 1)]
        if state["running"] and len(rows) == 3:
            for row, status in zip(rows, ("complete", "error", "skipped")):
                row["enrichments"] = {"push": {"status": status, "value": 0 if status == "complete" else "old result"}}
        route.fulfill(json={"workbook": {"id": "review", "name": "Run review fixture", "status": "running" if state["running"] else "draft", "source_type": "empty", "columns_config": columns, "total_rows": state["rows"], "completed_rows": 0}, "rows": rows, "total_rows": state["rows"], "query_total_rows": state["rows"], "page": 1, "page_size": 1000})
    elif path == "/api/workbooks/review/run/estimate":
        estimates.append(parse_qs(url.query))
        route.fulfill(status=503 if state["estimate_error"] else 200, json={"detail": "Unavailable"} if state["estimate_error"] else {"rows": state["rows"], "best_usd": 0.01, "worst_usd": 0.08, "breakdown": [], "note": "Fixture catalog estimate", "unknown_providers": ["unpriced_fixture"], "catalog_complete": False})
    elif path == "/api/workbooks/review/run":
        assert route.request.method == "POST"
        runs.append(route.request.post_data_json)
        if state["run_error"]:
            route.fulfill(status=409, json={"detail": "The matching row count changed. Refresh the run review before starting."})
        else:
            state["running"] = True
            route.fulfill(json={"status": "started", "total_jobs": state["rows"], "message": "Fixture run queued"})
    elif path == "/api/workbooks/review/stop":
        stops.append(route.request.method)
        if not state["stop_error"]:
            state["running"] = False
        route.fulfill(status=503 if state["stop_error"] else 200, json={"status": "paused"})
    elif path == "/api/events":
        route.fulfill(content_type="text/event-stream", body=": fixture\n\n")
    else:
        payloads = {
            "/auth/me": {"id": 1, "username": "UI fixture", "is_admin": True, "role": "admin"},
            "/api/workspaces": {"workspaces": [{"id": "fixture", "name": "UI fixture", "slug": "fixture"}], "active_id": "fixture"},
            "/api/copilotkit/conversations": {"conversations": []},
            "/api/settings/llm-usage": {"providers": [], "today_calls": 0, "today_tokens": 0},
            "/api/jobs": [], "/api/leads": [],
            "/api/workbooks/meta/providers": {"providers": []},
            "/api/workbooks/meta/ai-column-presets": {"presets": [], "categories": []},
            "/api/v2/workbooks/review/views": {"views": [{"id": "partnerships", "workbook_id": "review", "name": "Partnerships", "config": {"filters": [], "sort": [], "hidden_columns": []}}]},
            "/api/workbooks/review/connector-runs": {"runs": []},
            "/api/workbooks/review/cost": {"budget_max_usd": 0, "budget_spent_usd": 0},
        }
        if path not in payloads:
            unexpected.append(f"{route.request.method} {path}")
        route.fulfill(json=payloads.get(path, {}))

with sync_playwright() as p:
    browser = p.chromium.launch(executable_path="/usr/bin/chromium", headless=True)
    context = browser.new_context(viewport={"width": 1440, "height": 1000}, reduced_motion="reduce")
    context.route("**/*", fixture)
    context.route_web_socket("**/*", lambda ws: None)
    context.add_init_script("localStorage.setItem('yupcha_token','fixture-only'); localStorage.setItem('yupcha_workspace_id','fixture')")
    page = context.new_page()
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.goto(args.url + "/workbooks/review", wait_until="domcontentloaded")
    page.get_by_role("button", name="All rows", exact=True).click()
    page.get_by_role("menuitem", name="Partnerships", exact=True).click()
    page.get_by_placeholder("Search rows...").fill("Acme")
    page.get_by_role("checkbox", name="Select row 1", exact=True).check()
    trigger = page.get_by_role("button", name="Review run", exact=True)
    trigger.click()
    dialog = page.get_by_role("dialog", name="Review workbook run", exact=True)
    expect(dialog).to_contain_text("Checkbox selection does not limit this run")
    expect(dialog.get_by_role("alert")).to_contain_text("Could not load")
    expect(dialog.get_by_role("button", name="Start run", exact=True)).to_be_disabled()
    assert not runs
    state["estimate_error"] = False
    dialog.get_by_role("button", name="Refresh review", exact=True).click()
    expect(dialog).to_contain_text("$0.0100–$0.0800")
    expect(dialog).to_contain_text("External actions: CRM export")
    assert estimates[-1] == {"view_id": ["partnerships"], "search": ["Acme"]}, estimates
    expect(page.get_by_role("alert").filter(has_text="Unpriced providers: unpriced_fixture")).to_be_visible()
    for theme in ("light", "dark"):
        page.evaluate("theme => window.OpenGTMTheme.setPreference(theme)", theme)
        for width in (375, 1440):
            height = 667 if width == 375 else 1000
            page.set_viewport_size({"width": width, "height": height})
            bounds = dialog.bounding_box()
            assert bounds and bounds["x"] >= 0 and bounds["x"] + bounds["width"] <= width
            assert bounds["y"] >= 0 and bounds["y"] + bounds["height"] <= height
            expect(dialog.get_by_role("button", name="Start run", exact=True)).to_be_in_viewport()
            expect(dialog.get_by_text("Includes external sends from 1 output column.", exact=True)).to_be_in_viewport()
            if args.screenshots:
                args.screenshots.mkdir(parents=True, exist_ok=True)
                page.screenshot(path=str(args.screenshots / f"run-review-{theme}-{width}.png"), animations="disabled")
    dialog.get_by_role("button", name="Cancel", exact=True).click()
    expect(trigger).to_be_focused()
    assert not runs
    page.get_by_role("button", name="Fill missing", exact=True).click()
    dialog = page.get_by_role("dialog", name="Review fill missing", exact=True)
    start = dialog.get_by_role("button", name="Start run", exact=True)
    expect(start).to_be_enabled()
    start.evaluate("button => { button.click(); button.click(); }")
    expect(dialog.get_by_role("alert").filter(has_text="row count changed")).to_be_visible()
    expect(dialog.get_by_role("alert").filter(has_text="row count changed")).to_be_focused()
    expect(start).to_be_disabled()
    assert runs == [{"view_id": "partnerships", "search": "Acme", "expected_rows": 2, "fill_missing": True}], runs
    state["run_error"] = False
    state["rows"] = 3
    dialog.get_by_role("button", name="Refresh review", exact=True).click()
    expect(start).to_be_enabled()
    start.click()
    expect(dialog).not_to_be_visible()
    expect(page.get_by_role("button", name="Stop", exact=True)).to_be_visible()
    assert len(runs) == 2 and runs[-1]["expected_rows"] == 3
    for theme in ("light", "dark"):
        page.evaluate("theme => window.OpenGTMTheme.setPreference(theme)", theme)
        for width in (375, 1440):
            page.set_viewport_size({"width": width, "height": 667 if width == 375 else 1000})
            for label in ("Loaded cells: 1/3 complete", "1 err", "1 skipped", "100% processed"):
                text = page.get_by_text(label, exact=True)
                expect(text).to_be_in_viewport()
                bounds = text.bounding_box()
                assert bounds and bounds["x"] >= 0 and bounds["x"] + bounds["width"] <= width, (label, bounds, width)
    page.get_by_role("button", name="Stop", exact=True).click()
    expect(page.get_by_text("Could not stop the run. Try again.", exact=True)).to_be_visible()
    expect(page.get_by_role("button", name="Stop", exact=True)).to_be_visible()
    state["stop_error"] = False
    page.get_by_role("button", name="Stop", exact=True).click()
    expect(trigger).to_be_visible()
    assert stops == ["POST", "POST"]
    trigger.click()
    normal = page.get_by_role("dialog", name="Review workbook run", exact=True)
    normal.get_by_role("button", name="Start run", exact=True).click()
    expect(normal).not_to_be_visible()
    assert runs[-1] == {"view_id": "partnerships", "search": "Acme", "expected_rows": 3}, runs
    assert not errors, errors
    assert not unexpected, unexpected
    browser.close()
print(json.dumps({"passed": True, "scope": "run review/query/estimate/count conflict/start/stop UI; fixture APIs only", "run_attempts": len(runs)}))
