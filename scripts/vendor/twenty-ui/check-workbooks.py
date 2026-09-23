"""Exercise the real workbook list UI with isolated API fixtures, never live data."""

import argparse
import json
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser()
parser.add_argument("--url", default="http://127.0.0.1:4399")
parser.add_argument("--chromium", default="/usr/bin/chromium")
parser.add_argument("--screenshots", type=Path)
args = parser.parse_args()


def workbook(id, name, status="draft"):
    return {"id": id, "name": name, "status": status, "columns_config": [], "total_rows": 12, "completed_rows": 3, "updated_at": "2026-09-22T00:00:00Z"}


records = [workbook("alpha", "Alpha partnerships"), workbook("beta", "Beta hiring", "running")]
state = {"list_error": False, "create_error": True, "delete_error": True, "template_error": True}
created = []
deleted = []
errors = []
unexpected = []


def fixture(route):
    path = urlparse(route.request.url).path.rstrip("/")
    method = route.request.method
    if not path.startswith(("/api/", "/auth/")):
        route.continue_()
        return
    if path == "/api/workbooks" and method == "GET":
        route.fulfill(status=503 if state["list_error"] else 200, json={"detail": "Unavailable"} if state["list_error"] else {"workbooks": records, "total": len(records)})
    elif path == "/api/workbooks" and method == "POST":
        created.append(route.request.post_data_json)
        route.fulfill(status=503 if state["create_error"] else 200, json={"detail": "Unavailable"} if state["create_error"] else workbook("created", "New partnerships"))
    elif path == "/api/workbooks/alpha" and method == "DELETE":
        deleted.append("alpha")
        if not state["delete_error"]:
            records[:] = [record for record in records if record["id"] != "alpha"]
        route.fulfill(status=503 if state["delete_error"] else 200, json={"detail": "Unavailable"} if state["delete_error"] else {"ok": True})
    elif path == "/api/templates":
        route.fulfill(json={"templates": [{"id": "partnerships", "name": "Partnerships template", "description": "Fixture template", "columns": []}]})
    elif path == "/api/templates/partnerships/create":
        route.fulfill(status=403 if state["template_error"] else 200, json={"detail": "Template editor access required"} if state["template_error"] else {"id": "from-template"})
    elif path in ("/api/workbooks/created", "/api/workbooks/from-template"):
        # Editor execution is outside this list test; verify navigation without
        # pretending a mocked successful editor run is an end-to-end outcome.
        route.fulfill(status=404, json={"detail": "Editor fixture not provided"})
    elif path == "/api/events":
        route.fulfill(content_type="text/event-stream", body=": fixture\n\n")
    else:
        payloads = {
            "/auth/me": {"id": 1, "username": "UI fixture", "is_admin": True, "role": "admin"},
            "/api/workspaces": {"workspaces": [{"id": "fixture", "name": "UI fixture", "slug": "fixture"}], "active_id": "fixture"},
            "/api/copilotkit/conversations": {"conversations": []},
            "/api/settings/llm-usage": {"providers": [], "today_calls": 0, "today_tokens": 0},
            "/api/jobs": [], "/api/leads": [],
            "/api/workbooks/meta/providers": [], "/api/workbooks/meta/filter-options": {},
        }
        if path not in payloads:
            unexpected.append(f"{method} {path}")
        route.fulfill(json=payloads.get(path, {}))


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(executable_path=args.chromium, headless=True)
    context = browser.new_context(viewport={"width": 1440, "height": 900}, reduced_motion="reduce")
    context.route("**/*", fixture)
    context.add_init_script("localStorage.setItem('yupcha_token','fixture-only'); localStorage.setItem('yupcha_workspace_id','fixture')")
    page = context.new_page()
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.goto(args.url + "/workbooks", wait_until="domcontentloaded")
    expect(page.get_by_role("link", name="Alpha partnerships", exact=True)).to_be_visible()
    page.get_by_role("textbox", name="Search workbooks").fill("Beta")
    expect(page.get_by_role("link", name="Alpha partnerships", exact=True)).to_have_count(0)
    page.get_by_label("Status", exact=True).select_option("draft")
    expect(page.get_by_text("No matching workbooks")).to_be_visible()
    page.get_by_role("button", name="Clear filters").click()
    page.get_by_label("Sort", exact=True).select_option("name")
    expect(page.locator("tbody tr").first).to_contain_text("Alpha partnerships")

    for theme in ("light", "dark"):
        page.get_by_role("combobox", name="Color theme").select_option(theme)
        for width in (375, 768, 1440):
            page.set_viewport_size({"width": width, "height": 900})
            assert not page.evaluate("document.documentElement.scrollWidth > innerWidth"), (theme, width)
            if args.screenshots:
                args.screenshots.mkdir(parents=True, exist_ok=True)
                page.screenshot(path=str(args.screenshots / f"workbooks-{theme}-{width}.png"), animations="disabled")

    trigger = page.get_by_role("button", name="New workbook", exact=True)
    trigger.click()
    dialog = page.get_by_role("dialog", name="New workbook", exact=True)
    expect(dialog).to_be_visible()
    dialog.get_by_label("Workbook name", exact=True).fill("New partnerships")
    dialog.get_by_label("From my leads", exact=True).check()
    dialog.get_by_label("Score tier", exact=True).select_option("hot")
    dialog.get_by_label("Maximum rows", exact=True).fill("750")
    if args.screenshots:
        page.screenshot(path=str(args.screenshots / "create-dialog.png"), animations="disabled")
    dialog.get_by_role("button", name="Create workbook", exact=True).click()
    expect(dialog.get_by_role("alert")).to_contain_text("Failed to create workbook")
    expect(dialog.get_by_label("Workbook name", exact=True)).to_have_value("New partnerships")
    state["create_error"] = False
    dialog.locator("form").evaluate("form => { form.requestSubmit(); form.requestSubmit(); }")
    expect(page).to_have_url(args.url + "/workbooks/created")
    assert len(created) == 2, created  # One failed attempt, one guarded retry.
    assert created[-1]["source"] == "leads_filter" and created[-1]["max_rows"] == 750
    assert created[-1]["filter_criteria"] == {"score_tier": "hot"}
    assert [column["id"] for column in created[-1]["columns_config"]] == ["company", "website", "email", "phone", "city", "score"]
    page.goto(args.url + "/workbooks", wait_until="domcontentloaded")
    trigger.click()
    page.get_by_role("button", name="Cancel", exact=True).click()
    expect(trigger).to_be_focused()

    page.get_by_role("button", name="Delete Alpha partnerships", exact=True).click()
    alert = page.get_by_role("alertdialog")
    expect(alert).to_contain_text("Delete Alpha partnerships?")
    alert.get_by_role("button", name="Cancel", exact=True).click()
    assert not deleted
    page.get_by_role("button", name="Delete Alpha partnerships", exact=True).click()
    alert.get_by_role("button", name="Delete workbook", exact=True).click()
    expect(alert.get_by_role("alert")).to_contain_text("Failed to delete workbook")
    state["delete_error"] = False
    alert.get_by_role("button", name="Delete workbook", exact=True).click()
    expect(alert).not_to_be_visible()
    expect(page.get_by_role("link", name="Alpha partnerships", exact=True)).to_have_count(0)
    assert deleted == ["alpha", "alpha"]

    page.get_by_role("button", name="Browse templates").click()
    page.get_by_role("button", name="Use Partnerships template").click()
    expect(page.get_by_text("Template editor access required", exact=True)).to_be_visible()
    expect(page).to_have_url(args.url + "/workbooks")
    state["template_error"] = False
    page.get_by_role("button", name="Use Partnerships template").click()
    expect(page).to_have_url(args.url + "/workbooks/from-template")

    state["list_error"] = True
    page.goto(args.url + "/workbooks", wait_until="domcontentloaded")
    expect(page.get_by_role("button", name="Retry workbooks")).to_be_visible(timeout=15000)
    expect(page.get_by_text("Your first workbook starts here")).to_have_count(0)
    state["list_error"] = False
    page.get_by_role("button", name="Retry workbooks").click()
    expect(page.get_by_role("link", name="Beta hiring", exact=True)).to_be_visible()
    records[:] = [workbook(f"batch-{index}", f"Batch {index:03d}") for index in range(105)]
    page.reload(wait_until="domcontentloaded")
    expect(page.locator("tbody tr")).to_have_count(50)
    page.get_by_role("button", name="Next", exact=True).click()
    expect(page.get_by_text("Page 2 of 3", exact=True)).to_be_visible()
    expect(page.locator("tbody tr")).to_have_count(50)
    page.get_by_role("button", name="Next", exact=True).click()
    expect(page.locator("tbody tr")).to_have_count(5)
    expect(page.get_by_role("button", name="Next", exact=True)).to_be_disabled()
    page.get_by_role("textbox", name="Search workbooks").fill("Batch 104")
    expect(page.locator("tbody tr")).to_have_count(1)
    expect(page.get_by_role("link", name="Batch 104", exact=True)).to_be_visible()
    assert "page=" not in page.url
    assert not errors, errors
    assert not unexpected, unexpected
    browser.close()

print(json.dumps({"passed": True, "scope": "workbook list/create/template/delete UI; intercepted APIs; editor execution excluded", "page_errors": errors}, indent=2))
