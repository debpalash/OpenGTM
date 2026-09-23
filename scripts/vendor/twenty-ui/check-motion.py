"""Verify the macOS motion layer in the real app with fixture APIs (never live data).

Checks: window (dialog) spring open + clean close, section page transition,
dock magnification on the collapsed sidebar, and that prefers-reduced-motion
turns all of it off.
"""

import argparse
import json
from urllib.parse import urlparse

from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser()
parser.add_argument("--url", default="http://127.0.0.1:4399")
parser.add_argument("--chromium", default="/usr/bin/chromium")
args = parser.parse_args()

PAYLOADS = {
    "/auth/me": {"id": 1, "username": "UI fixture", "is_admin": True, "role": "admin"},
    "/api/workspaces": {"workspaces": [{"id": "fixture", "name": "UI fixture", "slug": "fixture"}], "active_id": "fixture"},
    "/api/copilotkit/conversations": {"conversations": []},
    "/api/settings/llm-usage": {"providers": [], "today_calls": 0, "today_tokens": 0},
    "/api/workbooks": {"workbooks": [], "total": 0},
    "/api/jobs": [], "/api/leads": [],
}


def fixture(route):
    path = urlparse(route.request.url).path.rstrip("/")
    if not path.startswith(("/api/", "/auth/")):
        route.continue_()
    elif path == "/api/events":
        route.fulfill(content_type="text/event-stream", body=": fixture\n\n")
    else:
        route.fulfill(json=PAYLOADS.get(path, {}))


def started(page):
    """Animation names that actually started in the page so far."""
    return page.evaluate("window.__anims.filter(n => n.startsWith('gtm-'))")


def icon_scale(page, index):
    return page.locator('[data-sidebar="menu-item"]').nth(index).locator("svg").first.evaluate(
        "e => getComputedStyle(e).scale")


results = {}
with sync_playwright() as playwright:
    browser = playwright.chromium.launch(executable_path=args.chromium, headless=True)
    for reduced in (False, True):
        context = browser.new_context(viewport={"width": 1440, "height": 900},
                                      reduced_motion="reduce" if reduced else "no-preference")
        context.route("**/*", fixture)
        context.add_init_script("localStorage.setItem('yupcha_token','fixture-only'); "
                                "localStorage.setItem('yupcha_workspace_id','fixture'); "
                                # animationstart is the ground truth for "this surface animated"
                                "if (!window.__anims) { window.__anims = []; "
                                "document.addEventListener('animationstart', "
                                "e => window.__anims.push(e.animationName), true) }")
        page = context.new_page()
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.goto(args.url + "/workbooks", wait_until="domcontentloaded")
        expect(page.locator(".gtm-page-enter")).to_be_visible()
        page.wait_for_timeout(300)
        page_anims = started(page)

        # Window: the command menu is a Dialog.
        page.keyboard.press("Control+k")
        expect(page.locator('[data-slot="dialog-content"][data-open]')).to_be_visible()
        page.wait_for_timeout(200)
        window_anims = [n for n in started(page) if n not in page_anims]
        page.keyboard.press("Escape")
        expect(page.locator('[data-slot="dialog-content"]')).to_have_count(0, timeout=3000)  # exit must finish and unmount

        # Dock: collapse the sidebar to its icon rail, hover an icon.
        page.keyboard.press("Control+b")
        expect(page.locator('[data-collapsible="icon"]')).to_have_count(1)
        page.wait_for_timeout(450)
        page.locator('[data-sidebar="menu-item"]').nth(3).locator('[data-sidebar="menu-button"]').hover()
        page.wait_for_timeout(400)
        dock = [icon_scale(page, i) for i in (2, 3, 4, 7)]

        key = "reduced" if reduced else "motion"
        results[key] = {"page": page_anims, "window": window_anims, "dock": dock, "errors": errors}
        if reduced:
            assert page_anims == [] and window_anims == [], results[key]
            assert all(value in ("none", "1") for value in dock), results[key]
        else:
            assert "gtm-page-in" in page_anims, results[key]
            assert window_anims.count("gtm-window-in") == 1, results[key]  # opens once, no replay
            assert dock[1] == "1.45" and dock[0] == "1.2" and dock[2] == "1.2", results[key]
            assert dock[3] in ("none", "1"), results[key]
        assert not errors, errors
        context.close()
    browser.close()

print(json.dumps({"passed": True, "scope": "motion layer; fixture APIs only", **results}, indent=2))
