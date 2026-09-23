"""Run history UI with explicitly isolated API and socket fixtures."""
import argparse
import json
from pathlib import Path
from urllib.parse import urlparse, parse_qs
from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser()
parser.add_argument("--url", default="http://127.0.0.1:4399")
parser.add_argument("--screenshots", type=Path)
args = parser.parse_args()
state = {"error": True, "finished": False}
errors, unexpected, cursors = [], [], []

def base_receipt(jid, status, result=None):
    return {"job_id": jid, "run_id": f"durable-run-{jid}", "status": status, "row_count": 2, "column_count": 2, "selected_cell_count": 2 if jid == 301 else 4, "retry_count": 0, "fill_missing": True, "force": False, "error": "Worker failed" if status == "failed" else None, "created_at": "2026-09-22T00:00:00+00:00", "started_at": None, "completed_at": None, "last_heartbeat": None, "next_run_at": None, "result": result}

def receipt(jid, status, result=None):
    record = base_receipt(jid, status, result)
    if jid == 301:
        record["output_attempts"] = {"total": 2, "succeeded": 1, "failed": 0, "awaiting_receipt": 1, "unknown": 0}
    return record

result = {"completed": 3, "errors": 1, "total": 4, "rows": 2, "stopped": False, "recorded_at": "2026-09-22T00:01:00+00:00"}

def fixture(route):
    url = urlparse(route.request.url)
    path = url.path.rstrip("/")
    if not path.startswith(("/api/", "/auth/")):
        route.continue_()
        return
    if route.request.method != "GET":
        unexpected.append(f"Unexpected mutation {route.request.method} {path}")
        route.abort()
        return
    if path == "/api/workbooks/history/runs":
        cursor = parse_qs(url.query).get("before_id", [None])[0]
        cursors.append(cursor)
        if state["error"]:
            route.fulfill(status=503, json={"detail": "Unavailable"})
        elif cursor:
            route.fulfill(json={"runs": [receipt(300, "failed")], "has_more": False, "next_before_id": None})
        else:
            route.fulfill(json={"runs": [receipt(302, "completed" if state["finished"] else "pending", result if state["finished"] else None), receipt(301, "completed")], "has_more": True, "next_before_id": 301})
    elif path == "/api/events":
        route.fulfill(content_type="text/event-stream", body=": fixture\n\n")
    else:
        payloads = {
            "/auth/me": {"id": 1, "username": "UI fixture", "is_admin": True, "role": "admin"},
            "/api/workspaces": {"workspaces": [{"id": "fixture", "name": "UI fixture", "slug": "fixture"}], "active_id": "fixture"},
            "/api/copilotkit/conversations": {"conversations": []},
            "/api/settings/llm-usage": {"providers": [], "today_calls": 0, "today_tokens": 0},
            "/api/jobs": [], "/api/leads": [],
            "/api/workbooks/history": {"workbook": {"id": "history", "name": "History fixture", "status": "failed", "columns_config": [], "total_rows": 0, "completed_rows": 0}, "rows": [], "total_rows": 0, "page": 1},
            "/api/workbooks/meta/providers": {"providers": []},
            "/api/workbooks/meta/ai-column-presets": {"presets": [], "categories": []},
            "/api/v2/workbooks/history/views": {"views": []},
            "/api/workbooks/history/connector-runs": {"runs": []},
            "/api/workbooks/history/cost": {"budget_max_usd": 0, "budget_spent_usd": 0},
            "/api/workbooks/history/run/estimate": {"rows": 0, "best_usd": 0, "worst_usd": 0, "breakdown": [], "note": "Fixture"},
        }
        if path not in payloads:
            unexpected.append(path)
        route.fulfill(json=payloads.get(path, {}))

with sync_playwright() as p:
    browser = p.chromium.launch(executable_path="/usr/bin/chromium", headless=True)
    context = browser.new_context(viewport={"width": 1440, "height": 900}, reduced_motion="reduce")
    context.route("**/*", fixture)
    context.route_web_socket("**/*", lambda ws: None)
    context.add_init_script("localStorage.setItem('yupcha_token','fixture-only'); localStorage.setItem('yupcha_workspace_id','fixture')")
    page = context.new_page()
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.goto(args.url + "/workbooks/history", wait_until="domcontentloaded")
    expect(page.get_by_role("alert").filter(has_text="Run needs attention")).to_contain_text("avoid re-sending completed outputs")
    trigger = page.get_by_role("button", name="Run history", exact=True)
    trigger.click()
    dialog = page.get_by_role("dialog", name="Workbook run history", exact=True)
    expect(dialog.get_by_role("alert")).to_contain_text("Could not load run history")
    expect(dialog.get_by_text("No workbook runs recorded yet.", exact=True)).to_have_count(0)
    state["error"] = False
    dialog.get_by_role("button", name="Retry run history", exact=True).click()
    expect(dialog.get_by_text("Queued", exact=True)).to_be_visible()
    expect(dialog.get_by_text("Worker finished", exact=True)).to_be_visible()
    expect(dialog.get_by_text("2 selected cells", exact=True)).to_be_visible()
    expect(dialog.get_by_text("Output receipts: 1 succeeded · 0 failed · 1 awaiting receipt · 0 unknown", exact=True)).to_be_visible()
    expect(dialog.get_by_text("Check the destination before re-sending.", exact=False)).to_be_visible()
    expect(dialog.get_by_text("Output delivery tracking not recorded.").first).to_be_visible()
    dialog.get_by_role("button", name="Load older runs", exact=True).click()
    expect(dialog.get_by_role("heading", name="Run #300", exact=True)).to_be_visible()
    assert "301" in cursors
    state["finished"] = True
    expect(dialog.get_by_text("Finished with cell errors", exact=True)).to_be_visible(timeout=10000)
    expect(dialog.get_by_text("Reported cells: 3 complete · 1 error · 4 planned", exact=True)).to_be_visible()
    dialog.locator("summary").first.click()
    expect(dialog.get_by_text("durable-run-302", exact=True)).to_be_visible()
    for theme in ("light", "dark"):
        page.evaluate("theme => window.OpenGTMTheme.setPreference(theme)", theme)
        for width in (375, 1440):
            height = 667 if width == 375 else 900
            page.set_viewport_size({"width": width, "height": height})
            expect(dialog.get_by_role("button", name="Close", exact=True)).to_be_in_viewport()
            bounds = dialog.bounding_box()
            assert bounds and bounds["x"] >= 0 and bounds["x"] + bounds["width"] <= width
            if args.screenshots:
                args.screenshots.mkdir(parents=True, exist_ok=True)
                page.screenshot(path=str(args.screenshots / f"history-{theme}-{width}.png"), animations="disabled")
    dialog.get_by_role("button", name="Close", exact=True).click()
    expect(trigger).to_be_focused()
    page.reload(wait_until="domcontentloaded")
    trigger.click()
    expect(dialog.get_by_text("Finished with cell errors", exact=True)).to_be_visible()
    assert not errors, errors
    assert not unexpected, unexpected
    browser.close()
print(json.dumps({"passed": True, "scope": "run history/error/pagination/polling/reload UI; fixture APIs only"}))
