"""Theme/route smoke against the real app, with all business APIs intercepted."""

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
errors = []
unexpected = []
state = {"active": "ui-fixture"}
workbook_requests = []
lead_requests = []
command_chunks = []


def fixture(route):
    path = urlparse(route.request.url).path.rstrip("/")
    if "/assets/command-menu-content-" in path:
        command_chunks.append(path)
    if path == "/api/leads":
        lead_requests.append(route.request.url)
    if not path.startswith(("/api/", "/auth/")):
        route.continue_()
        return
    if path == "/api/workspaces/switch" and route.request.method == "POST":
        requested = route.request.post_data_json["workspace_id"]
        if requested == "denied-fixture":
            route.fulfill(status=403, json={"detail": "Fixture access denied"})
        else:
            state["active"] = requested
            route.fulfill(status=200, json={"ok": True})
        return
    if route.request.method != "GET":
        unexpected.append(f"{route.request.method} {path}")
    payloads = {
        "/auth/me": {"id": 1, "username": "UI fixture", "is_admin": True, "role": "admin"},
        "/api/workspaces": {"workspaces": [{"id": key, "name": label, "slug": key} for key, label in [("ui-fixture", "UI fixture"), ("second-fixture", "Second fixture"), ("denied-fixture", "Denied fixture")]], "active_id": state["active"]},
        "/api/copilotkit/conversations": {"conversations": []},
        "/api/settings/llm-usage": {"providers": [], "today_calls": 0, "today_tokens": 0},
        "/api/jobs": [],
        "/api/leads": [],
    }
    if path == "/api/workbooks":
        workspace = route.request.headers.get("x-workspace-id")
        workbook_requests.append(workspace)
        route.fulfill(status=200, json={"workbooks": [{"id": workspace + "-book", "name": workspace + " private workbook", "status": "draft", "columns_config": [], "total_rows": 0, "completed_rows": 0, "created_at": "2026-09-22T00:00:00Z"}], "total": 1})
    elif path == "/api/events":
        route.fulfill(status=200, content_type="text/event-stream", body=": fixture\n\n")
    else:
        if path not in payloads:
            unexpected.append(f"Unmapped fixture: {path}")
        route.fulfill(status=200, json=payloads.get(path, {}))


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(executable_path=args.chromium, headless=True)
    context = browser.new_context(viewport={"width": 1280, "height": 800}, color_scheme="light")
    context.route("**/*", fixture)
    context.add_init_script("localStorage.setItem('yupcha_token', 'ui-fixture-only'); localStorage.setItem('yupcha_workspace_id', 'ui-fixture')")
    page = context.new_page()
    def record_error(error):
        errors.append(str(error))
        print(f"Browser error: {error}", flush=True)

    page.on("pageerror", record_error)
    page.goto(args.url + "/workbooks", wait_until="domcontentloaded")
    picker = page.get_by_role("combobox", name="Color theme")
    expect(picker).to_have_value("dark")
    expect(page.locator("html")).to_have_class("dark")
    expect(page.get_by_role("link", name="ui-fixture private workbook")).to_be_visible()
    for label in ("Workbooks", "Campaigns", "Manage workspaces", "Settings"):
        expect(page.get_by_role("link", name=label, exact=True)).to_be_visible()
    assert not lead_requests, "Closed command palette fetched leads"
    assert not command_chunks, "Closed command palette loaded its lazy UI"
    page.get_by_role("button", name="Find anything").click()
    for label in ("Workbooks", "Audiences", "Automations", "Watches", "Manage workspaces"):
        expect(page.get_by_role("option", name=label, exact=True)).to_have_count(1)
    page.keyboard.press("Escape")
    expect(page.get_by_role("button", name="Find anything")).to_be_focused()
    page.get_by_role("button", name="Recent chats").click()
    expect(page.get_by_role("textbox", name="Search recent chats")).to_be_visible()
    page.get_by_role("button", name="Recent chats").click()
    for theme in ("light", "dark"):
        picker.select_option(theme)
        expect(page.locator("html")).to_have_class(theme)
        assert page.evaluate("getComputedStyle(document.body).fontFamily").startswith("Inter")
        for width in (375, 1280):
            page.set_viewport_size({"width": width, "height": 900})
            assert not page.evaluate("document.documentElement.scrollWidth > innerWidth"), (theme, width)
            if args.screenshots:
                args.screenshots.mkdir(parents=True, exist_ok=True)
                page.screenshot(path=str(args.screenshots / f"app-{theme}-{width}.png"))
    picker.select_option("system")
    page.emulate_media(color_scheme="light")
    expect(page.locator("html")).to_have_class("light")
    page.emulate_media(color_scheme="dark")
    expect(page.locator("html")).to_have_class("dark")
    picker.select_option("light")
    page.reload(wait_until="domcontentloaded")
    expect(picker).to_have_value("light")
    expect(page.locator("html")).to_have_class("light")
    other = context.new_page()
    other.goto(args.url + "/workbooks", wait_until="domcontentloaded")
    other.get_by_role("combobox", name="Color theme").select_option("dark")
    expect(page.locator("html")).to_have_class("dark")
    expect(picker).to_have_value("dark")
    other.close()

    page.keyboard.press("Control+k")
    expect(page.get_by_placeholder("Type a command or search...")).to_be_focused()
    page.get_by_role("option", name="Switch to light mode").click()
    assert lead_requests, "Opening the palette did not fetch leads"
    assert command_chunks, "Opening the palette did not load its UI"
    expect(page.locator("html")).to_have_class("light")
    expect(picker).to_have_value("light")
    page.get_by_role("button", name="Find anything").click()
    expect(page.get_by_role("option", name="Switch to dark mode")).to_be_visible()
    page.keyboard.press("Escape")
    expect(page.get_by_role("dialog")).not_to_be_visible()
    expect(page.get_by_role("button", name="Find anything")).to_be_focused()

    # Boot must resolve a saved preference even when the React bundle cannot run.
    boot = context.new_page()
    boot.route("**/assets/*.js", lambda route: route.abort())
    boot.goto(args.url + "/workbooks", wait_until="domcontentloaded")
    expect(boot.locator("html")).to_have_class("light")
    assert boot.evaluate("document.getElementById('root').childElementCount") == 0
    boot.close()

    workspace_picker = page.get_by_role("combobox", name="Active workspace")
    workspace_picker.select_option("denied-fixture")
    expect(page.get_by_text("Fixture access denied", exact=True)).to_be_visible()
    expect(workspace_picker).to_have_value("ui-fixture")
    expect(page.get_by_role("link", name="ui-fixture private workbook")).to_be_visible()
    workspace_picker.select_option("second-fixture")
    expect(page).to_have_url(args.url + "/chat")
    expect(workspace_picker).to_have_value("second-fixture")
    page.get_by_role("link", name="Workbooks", exact=True).click()
    expect(page.get_by_role("link", name="second-fixture private workbook")).to_be_visible()
    expect(page.get_by_role("link", name="ui-fixture private workbook")).to_have_count(0)
    assert "second-fixture" in workbook_requests, workbook_requests
    page.set_viewport_size({"width": 375, "height": 900})
    page.get_by_role("button", name="Toggle Sidebar").click()
    expect(page.get_by_role("navigation", name="Primary navigation")).to_be_visible()
    page.get_by_role("link", name="Workbooks", exact=True).click()
    expect(page.get_by_role("navigation", name="Primary navigation")).not_to_be_visible()
    # A fresh document has not cached the command module. Fail its first fetch.
    recovery = context.new_page()
    recovery.on("pageerror", record_error)
    recovery.route("**/assets/command-menu-content-*.js", lambda route: route.abort())
    recovery.goto(args.url + "/workbooks", wait_until="domcontentloaded")
    recovery.get_by_role("button", name="Find anything").click()
    expect(recovery.get_by_role("alert")).to_contain_text("Could not load commands")
    recovery.keyboard.press("Escape")
    expect(recovery.get_by_role("dialog")).not_to_be_visible()
    expect(recovery.get_by_role("button", name="Find anything")).to_be_focused()
    recovery.unroute("**/assets/command-menu-content-*.js")
    recovery.get_by_role("button", name="Find anything").click()
    expect(recovery.get_by_role("alert")).to_contain_text("save any pending edits")
    recovery.get_by_role("button", name="Reload page", exact=True).click()
    expect(recovery.get_by_role("button", name="Find anything")).to_be_visible()
    recovery.get_by_role("button", name="Find anything").click()
    expect(recovery.get_by_placeholder("Type a command or search...")).to_be_visible()
    recovery.close()
    assert not errors, errors
    assert not unexpected, unexpected
    browser.close()

print(json.dumps({"passed": True, "scope": "real app rendering; intercepted API fixtures; no backend execution", "page_errors": errors}, indent=2))
