"""Exercise only the local, fictional-data Twenty migration preview."""

import argparse
import json
from pathlib import Path

from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser()
parser.add_argument("--url", default="http://127.0.0.1:4399/ui-preview.html")
parser.add_argument("--chromium", default="/usr/bin/chromium")
parser.add_argument("--screenshots", type=Path)
args = parser.parse_args()
errors = []
requests = []

with sync_playwright() as playwright:
    browser = playwright.chromium.launch(executable_path=args.chromium, headless=True)
    page = browser.new_page(viewport={"width": 1280, "height": 800}, reduced_motion="reduce")
    page.add_init_script("if (!localStorage.getItem('theme')) localStorage.setItem('theme', 'light')")
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.on("request", lambda request: requests.append(request.url))
    page.goto(args.url, wait_until="networkidle")
    review = page.get_by_role("button", name="Review selection")
    expect(review).to_be_disabled()
    page.get_by_label("Select Alex Morgan").check()
    page.get_by_label("Select Taylor Reed").check()
    page.get_by_role("textbox", name="Search contacts", exact=True).fill("Jordan")
    expect(page.get_by_text("2 selected · 1 shown")).to_be_visible()
    review.click()
    dialog = page.get_by_role("alertdialog")
    expect(dialog).to_be_visible()
    expect(dialog.get_by_text("Alex Morgan", exact=True)).to_be_visible()
    expect(dialog.get_by_text("Taylor Reed", exact=True)).to_be_visible()
    expect(dialog.get_by_text("Jordan Lee", exact=True)).to_have_count(0)
    page.get_by_role("button", name="Cancel", exact=True).click()
    expect(review).to_be_focused()
    review.press("Enter")
    page.get_by_role("button", name="Confirm preview").click()
    expect(page.get_by_text("Preview reviewed: 2 contacts. No actions executed.")).to_be_visible()
    page.get_by_role("textbox", name="Search contacts", exact=True).fill("")
    page.get_by_role("button", name="View options").click()
    page.get_by_role("menuitemcheckbox", name="Comfortable rows").click()
    expect(page.locator("table")).to_have_attribute("data-comfortable", "true")
    page.keyboard.press("Escape")
    expect(page.get_by_role("button", name="View options")).to_be_focused()
    page.get_by_role("button", name="View options").click()
    page.get_by_role("menuitem", name="Clear selection").click()
    expect(review).to_be_disabled()
    page.get_by_role("textbox", name="Search contacts", exact=True).fill("no matching sample")
    expect(page.get_by_text("No contacts match. Try another name or company.")).to_be_visible()
    page.get_by_role("textbox", name="Search contacts", exact=True).fill("")
    page.get_by_role("link", name="Test workbook navigation").click()
    expect(page).to_have_url(args.url + "#/workbooks")

    for theme in ("light", "dark"):
        if theme == "dark":
            page.get_by_role("button", name="Dark theme").click()
        expect(page.locator("html")).to_have_class(theme)
        for width in (375, 768, 1280, 1440):
            page.set_viewport_size({"width": width, "height": 900})
            assert not page.evaluate("document.documentElement.scrollWidth > innerWidth"), (theme, width)
            if args.screenshots:
                args.screenshots.mkdir(parents=True, exist_ok=True)
                page.screenshot(path=str(args.screenshots / f"{theme}-{width}.png"), full_page=True)
        page.get_by_label("Select Alex Morgan").check()
        review.click()
        expect(dialog).to_be_visible()
        page.get_by_role("button", name="Cancel", exact=True).click()
        expect(review).to_be_focused()
        page.get_by_label("Select Alex Morgan").uncheck()

    assert not errors, errors
    theme_select = page.get_by_role("combobox", name="Color theme")
    theme_select.select_option("system")
    page.emulate_media(color_scheme="light")
    expect(page.locator("html")).to_have_class("light")
    page.emulate_media(color_scheme="dark")
    expect(page.locator("html")).to_have_class("dark")
    theme_select.select_option("light")
    page.reload(wait_until="networkidle")
    expect(page.locator("html")).to_have_class("light")
    expect(theme_select).to_have_value("light")
    assert not errors, errors
    assert not any("/api/" in url or "/auth/" in url for url in requests), requests
    assert not any("worker" in url or "monaco" in url for url in requests), requests
    browser.close()

print(json.dumps({"passed": True, "themes": 2, "viewport_widths": [375, 768, 1280, 1440], "page_errors": errors}, indent=2))
