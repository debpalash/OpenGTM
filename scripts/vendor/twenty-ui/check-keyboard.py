"""Keyboard shortcuts and Quick Look in the real app, with fixture APIs only."""

import argparse
import json
from urllib.parse import urlparse

from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser()
parser.add_argument("--url", default="http://127.0.0.1:4399")
parser.add_argument("--chromium", default="/usr/bin/chromium")
args = parser.parse_args()


def workbook(id, name, rows):
    return {"id": id, "name": name, "status": "draft", "columns_config": [], "total_rows": rows,
            "completed_rows": 0, "updated_at": "2026-09-22T00:00:00Z"}


WORKBOOKS = [workbook("alpha", "Alpha partnerships", 12), workbook("beta", "Beta hiring", 7),
             workbook("gamma", "Gamma accounts", 3)]
PAYLOADS = {
    "/auth/me": {"id": 1, "username": "UI fixture", "is_admin": True, "role": "admin"},
    "/api/workspaces": {"workspaces": [{"id": "fixture", "name": "UI fixture", "slug": "fixture"}], "active_id": "fixture"},
    "/api/copilotkit/conversations": {"conversations": []},
    "/api/settings/llm-usage": {"providers": [], "today_calls": 0, "today_tokens": 0},
    "/api/workbooks": {"workbooks": WORKBOOKS, "total": len(WORKBOOKS)},
    "/api/jobs": [], "/api/leads": [],
}


GRID_COLUMNS = [
    {"id": "full_name", "name": "Person", "type": "lead_field", "lead_field": "full_name", "width": 180},
    {"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company", "width": 160},
    {"id": "title", "name": "Title", "type": "lead_field", "lead_field": "title", "width": 180},
    {"id": "website", "name": "Website", "type": "lead_field", "lead_field": "website", "width": 160},
]
GRID_PEOPLE = [("Jane Doe", "PayPal", "Head of Partnerships", "paypal.com"),
               ("Sam Lee", "Stripe", "VP Sales", "stripe.com"),
               ("Ann Wu", "Acme", "Director", "acme.com")]
GRID = {"id": "grid", "name": "Grid fixture", "status": "draft", "source_type": "empty",
        "columns_config": GRID_COLUMNS, "total_rows": 3, "completed_rows": 0, "filter_criteria": None,
        "updated_at": "2026-09-22T00:00:00Z"}
GRID_ROWS = [{"row_id": i + 1, "lead_id": None, "lead": {}, "enrichments": {},
              "data": {"full_name": n, "company": c, "title": t, "website": w}}
             for i, (n, c, t, w) in enumerate(GRID_PEOPLE)]
PAYLOADS.update({
    "/api/workbooks/meta/providers": {"providers": []},
    "/api/workbooks/meta/ai-column-presets": {"presets": [], "categories": []},
    "/api/v2/workbooks/grid/views": {"views": []},
    "/api/workbooks/grid/connector-runs": {"runs": []},
    "/api/workbooks/grid/cost": {"budget_max_usd": 0, "budget_spent_usd": 0},
    "/api/workbooks/grid/run/estimate": {"best_usd": 0, "worst_usd": 0, "rows": 3, "breakdown": [], "note": "Fixture only"},
})


def fixture(route):
    path = urlparse(route.request.url).path.rstrip("/")
    if not path.startswith(("/api/", "/auth/")):
        route.continue_()
    elif path == "/api/workbooks/grid":
        route.fulfill(json={"workbook": GRID, "rows": GRID_ROWS, "total_rows": 3, "query_total_rows": 3, "page": 1,
                            "page_size": 1000, "next_cursor": None, "has_more": False})
    elif path == "/api/events":
        route.fulfill(content_type="text/event-stream", body=": fixture\n\n")
    elif path.startswith("/api/workbooks/") and path.count("/") == 3 and path in PAYLOADS:
        route.fulfill(json=PAYLOADS[path])
    elif path.startswith("/api/workbooks/") and path.count("/") == 3:
        route.fulfill(status=404, json={"detail": "Editor fixture not provided"})
    else:
        route.fulfill(json=PAYLOADS.get(path, {}))


def focused_row(page):
    return page.evaluate("document.activeElement?.closest('[data-nav-item]')?.textContent?.slice(0, 40) ?? null")


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(executable_path=args.chromium, headless=True)
    context = browser.new_context(viewport={"width": 1440, "height": 900}, reduced_motion="reduce")
    context.route("**/*", fixture)
    context.add_init_script("localStorage.setItem('yupcha_token','fixture-only'); "
                            "localStorage.setItem('yupcha_workspace_id','fixture'); localStorage.setItem('theme','light')")
    page = context.new_page()
    errors = []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.goto(args.url + "/workbooks", wait_until="domcontentloaded")
    expect(page.get_by_text("Alpha partnerships")).to_be_visible()
    page.locator("body").click(position={"x": 700, "y": 600})

    # ? — help overlay
    page.keyboard.press("Shift+?")
    help_dialog = page.get_by_role("dialog", name="Keyboard shortcuts")
    expect(help_dialog).to_be_visible()
    expect(help_dialog.get_by_text("Workbooks", exact=True)).to_be_visible()
    # Reduced motion must not break dialog positioning (Tailwind centers with translate).
    box = help_dialog.bounding_box()
    assert box and abs((box["x"] + box["width"] / 2) - 720) < 4 and abs((box["y"] + box["height"] / 2) - 450) < 4, box
    page.keyboard.press("Escape")
    expect(help_dialog).to_have_count(0)

    # g-sequences navigate
    page.keyboard.press("g"); page.keyboard.press("l")
    expect(page).to_have_url(args.url + "/leads")
    page.keyboard.press("g"); page.keyboard.press("w")
    expect(page).to_have_url(args.url + "/workbooks")
    expect(page.get_by_text("Alpha partnerships")).to_be_visible()
    # Let the route transition settle: a page still committing its navigation
    # can be replaced a few ms later (faster than a person can press "/").
    page.wait_for_load_state("networkidle")
    page.wait_for_timeout(250)

    # / focuses search; typing there never triggers shortcuts
    page.keyboard.press("/")
    search = page.get_by_placeholder("Search workbooks")
    expect(search).to_be_focused()
    page.keyboard.type("gl")
    expect(page).to_have_url(args.url + "/workbooks?q=gl")
    # Fast typing must not drop characters (the field once rebound to the URL).
    page.keyboard.type("partnerships")
    expect(search).to_have_value("glpartnerships")
    expect(page).to_have_url(args.url + "/workbooks?q=glpartnerships")
    search.fill("")
    page.keyboard.press("Escape")
    page.locator("body").click(position={"x": 700, "y": 700})

    # j / k move between rows
    page.keyboard.press("j")
    assert "Alpha" in (focused_row(page) or ""), focused_row(page)
    page.keyboard.press("j")
    assert "Beta" in (focused_row(page) or ""), focused_row(page)
    page.keyboard.press("k")
    assert "Alpha" in (focused_row(page) or ""), focused_row(page)

    # Space → Quick Look, → browses, Space closes onto the row last shown
    page.keyboard.press(" ")
    panel = page.locator("[data-quicklook-panel]")
    expect(panel).to_be_visible()
    expect(panel.get_by_role("heading", name="Alpha partnerships")).to_be_visible()
    expect(panel.get_by_text("1 of 3")).to_be_visible()
    page.keyboard.press("ArrowRight")
    expect(panel.get_by_role("heading", name="Beta hiring")).to_be_visible()
    expect(panel.get_by_text("Processed", exact=True)).to_be_visible()
    page.keyboard.press(" ")
    expect(panel).to_have_count(0)
    page.wait_for_function("document.activeElement?.closest('[data-nav-item]')?.textContent?.includes('Beta')")

    # Enter opens the focused row's workbook
    page.keyboard.press("Enter")
    expect(page).to_have_url(args.url + "/workbooks/beta")
    page.go_back()
    expect(page.get_by_text("Alpha partnerships")).to_be_visible()

    # [ toggles the sidebar; ⇧T toggles the theme
    page.locator("body").click(position={"x": 700, "y": 700})
    page.keyboard.press("[")
    expect(page.locator('[data-collapsible="icon"]')).to_have_count(1)
    page.keyboard.press("[")
    expect(page.locator('[data-collapsible="icon"]')).to_have_count(0)
    page.keyboard.press("Shift+T")
    expect(page.locator("html")).to_have_class("dark")
    page.keyboard.press("Shift+T")
    expect(page.locator("html")).to_have_class("light")

    # Grid: Space on a focused cell previews its row; → follows to the next row
    page.goto(args.url + "/workbooks/grid", wait_until="domcontentloaded")
    first = page.locator('[data-grid-row="0"][data-grid-column="3"]')
    expect(first).to_be_visible()
    first.click()
    page.keyboard.press(" ")
    panel = page.locator("[data-quicklook-panel]")
    expect(panel.get_by_role("heading", name="Jane Doe")).to_be_visible()
    expect(panel.get_by_text("Head of Partnerships · PayPal")).to_be_visible()
    page.keyboard.press("ArrowRight")
    expect(panel.get_by_role("heading", name="Sam Lee")).to_be_visible()
    page.keyboard.press(" ")
    expect(panel).to_have_count(0)
    page.wait_for_function("document.activeElement?.getAttribute('data-grid-row') === '1'")

    assert not errors, errors
    browser.close()

print(json.dumps({"passed": True, "scope": "keyboard shortcuts + Quick Look; fixture APIs only"}))
