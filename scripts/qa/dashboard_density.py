"""Measure workstation density and verify preferences and touch geometry.

Use a synthetic workspace containing at least 100 Companies. Temporarily switches
the account's active workspace; restores it even when a browser check fails.
"""

import argparse
import json
import os
from pathlib import Path
import urllib.parse
import urllib.request

from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument(
    "--baseline-url", help="Optional previous build for the same fixture"
)
parser.add_argument("--workspace-id", required=True)
parser.add_argument("--workbook-id", help="Also verify overview and workbook tool density and touch targets")
parser.add_argument(
    "--engine", choices=["chromium", "webkit", "firefox"], default="chromium"
)
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument("--output", default=".run/dashboard-density")
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
base = args.base_url.rstrip("/")
headers = {}
report = {"engine": args.engine, "measurements": {}, "checks": [], "page_errors": []}


def req(method, path, body=None, form=False):
    data = (
        None
        if body is None
        else (
            urllib.parse.urlencode(body).encode() if form else json.dumps(body).encode()
        )
    )
    request = urllib.request.Request(
        base + path,
        method=method,
        data=data,
        headers={
            **headers,
            "Content-Type": "application/x-www-form-urlencoded"
            if form
            else "application/json",
        },
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def save():
    (out / "results.json").write_text(json.dumps(report, indent=2) + "\n")


def audit(page, name):
    page.add_script_tag(path=str(root / "apps/web/node_modules/axe-core/axe.min.js"))
    result = page.evaluate("async()=>await axe.run(document)")
    report["checks"].append({"name": name, "violations": result["violations"]})
    save()
    assert not result["violations"], name


def geometry(page):
    return page.evaluate("""() => {
      const table = document.querySelector('.records-table');
      const region = document.querySelector('.records-table-scroll').getBoundingClientRect();
      const rows = [...table.querySelectorAll('tbody tr')].map(e=>e.getBoundingClientRect());
      const height=s=>document.querySelector(s).getBoundingClientRect().height;
      return {rowHeight:rows[0].height, fullyVisibleRows:rows.filter(r=>r.top>=region.top&&r.bottom<=region.bottom).length,
        topbar:height('.gtm-topbar'), header:height('.workbench-page-header'), toolbar:height('.workbench-toolbar'),
        sidebar:document.querySelector('[data-slot=sidebar-gap]').getBoundingClientRect().width,
        documentWidth:document.documentElement.scrollWidth};
    }""")


username = os.environ.get("OPENGTM_QA_USERNAME")
password = os.environ.get("OPENGTM_QA_PASSWORD")
if not username or not password:
    parser.error("Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD.")
token = req("POST", "/auth/token", {"username": username, "password": password}, True)[
    "access_token"
]
headers["Authorization"] = "Bearer " + token
prior = req("GET", "/api/workspaces")["active_id"]
try:
    req("POST", "/api/workspaces/switch", {"workspace_id": args.workspace_id})
    with sync_playwright() as p:
        options = {"headless": True}
        if args.engine == "chromium" and args.browser_path:
            options["executable_path"] = args.browser_path
        browser = getattr(p, args.engine).launch(**options)
        context = browser.new_context(
            viewport={"width": 1440, "height": 1000}, reduced_motion="reduce"
        )
        auth_script = (
            'localStorage.setItem("yupcha_token",' + json.dumps(token) + ");"
            'localStorage.setItem("yupcha_workspace_id",'
            + json.dumps(args.workspace_id)
            + ");"
        )
        context.add_init_script(auth_script)
        page = context.new_page()
        page.on("pageerror", lambda e: report["page_errors"].append(str(e)))

        def load(url):
            page.goto(url.rstrip("/") + "/companies")
            expect(page.locator("tbody tr")).to_have_count(100)
            page.evaluate("document.fonts.ready")
            # Clear navigation's initial autofocus without changing data.
            page.mouse.click(1050, 15)

        for label, url in (
            [("before", args.baseline_url)] if args.baseline_url else []
        ) + [("compact", base)]:
            load(url)
            report["measurements"][label] = geometry(page)
            page.screenshot(path=str(out / (label + ".png")))
            audit(page, label)
        compact = report["measurements"]["compact"]
        assert compact["rowHeight"] == 28, compact
        assert (
            compact["topbar"] == 36
            and compact["header"] == 44
            and compact["sidebar"] == 192
        ), compact
        if "before" in report["measurements"]:
            before = report["measurements"]["before"]
            assert compact["fullyVisibleRows"] > before["fullyVisibleRows"]

        # The narrow rail must survive route changes and full reloads, without
        # making navigation inaccessible. Keyboard expansion returns focus.
        toggle = page.locator('.gtm-topbar [data-sidebar="trigger"]')
        toggle.click()
        page.wait_for_function(
            "document.querySelector('[data-slot=sidebar-gap]').getBoundingClientRect().width===52"
        )
        page.reload()
        expect(page.locator("tbody tr")).to_have_count(100)
        assert geometry(page)["sidebar"] == 52
        audit(page, "saved-navigation-rail")
        page.screenshot(path=str(out / "navigation-rail.png"))
        page.keyboard.press("[")
        page.wait_for_function(
            "document.querySelector('[data-slot=sidebar-gap]').getBoundingClientRect().width===192"
        )
        page.reload()
        expect(page.locator("tbody tr")).to_have_count(100)
        assert geometry(page)["sidebar"] == 192
        for width, height in [(1366, 768), (1280, 720), (1920, 1080)]:
            page.set_viewport_size({"width": width, "height": height})
            assert page.evaluate("document.documentElement.scrollWidth===innerWidth")
            measurements = geometry(page)
            assert measurements["fullyVisibleRows"] >= 15, measurements
            report["measurements"][f"desktop-{width}-{height}"] = measurements
            audit(page, f"desktop-{width}-{height}")
            page.screenshot(path=str(out / f"desktop-{width}-{height}.png"))
        page.set_viewport_size({"width": 1440, "height": 1000})

        for theme in ["light", "dark"]:
            page.evaluate("theme=>window.OpenGTMTheme.setPreference(theme)", theme)
            for name, value, height in [
                ("Standard rows", "default", 40),
                ("Comfortable rows", "comfortable", 48),
                ("Compact rows", "compact", 28),
            ]:
                page.get_by_role("button", name="Table density", exact=True).click()
                item = page.get_by_role("menuitemradio", name=name, exact=True)
                item.focus()
                audit(page, theme + "-menu-" + value)
                item.press("Enter")
                expect(page.locator(".records-table")).to_have_attribute(
                    "data-density", value
                )
                assert geometry(page)["rowHeight"] == height
                page.reload()
                expect(page.locator(".records-table")).to_have_attribute(
                    "data-density", value
                )
                page.get_by_role("button", name="Table density", exact=True).click()
                expect(
                    page.get_by_role("menuitemradio", name=name, exact=True)
                ).to_have_attribute("aria-checked", "true")
                page.keyboard.press("Escape")
                expect(
                    page.get_by_role("button", name="Table density", exact=True)
                ).to_be_focused()
            page.screenshot(path=str(out / (theme + ".png")))

        page.evaluate('localStorage.setItem("opengtm-record-density","unknown")')
        page.reload()
        expect(page.locator(".records-table")).to_have_attribute(
            "data-density", "compact"
        )
        page.evaluate('localStorage.removeItem("opengtm-record-density")')

        if args.workbook_id:
            page.goto(base + "/overview")
            metrics = page.locator(".overview-metric")
            expect(metrics).to_have_count(4)
            metrics.first.focus()
            # WebKit's default macOS setting skips links with plain Tab.
            # Option+Tab traverses links without changing their native semantics.
            page.keyboard.press("Alt+Tab" if args.engine == "webkit" else "Tab")
            expect(metrics.nth(1)).to_be_focused()
            audit(page, "overview-keyboard")
            page.screenshot(path=str(out / "overview-keyboard.png"))
            page.goto(base + "/workbooks")
            expect(page.locator(".gtm-workbook-link").first).to_be_visible()
            audit(page, "workbooks-desktop")
            page.goto(base + "/workbooks/" + urllib.parse.quote(args.workbook_id, safe=""))
            expect(page.get_by_role("grid")).to_be_visible()
            toolbar = page.locator(".workbook-toolbar").bounding_box()
            assert toolbar["height"] <= 80, toolbar
            report["measurements"]["workbookToolbarHeight"] = toolbar["height"]
            audit(page, "workbook-desktop")
            page.screenshot(path=str(out / "workbook.png"))

        touch = browser.new_context(
            viewport={"width": 390, "height": 844},
            has_touch=True,
            is_mobile=True,
            reduced_motion="reduce",
        )
        touch.add_init_script(auth_script)
        phone = touch.new_page()
        phone.on("pageerror", lambda e: report["page_errors"].append(str(e)))
        for width in [320, 390, 768]:
            phone.set_viewport_size({"width": width, "height": 844})
            phone.goto(base + "/companies")
            expect(phone.locator("tbody tr")).to_have_count(100)
            assert phone.evaluate("document.documentElement.scrollWidth<=innerWidth")
            targets = phone.evaluate("""()=>{
              const first=s=>document.querySelector(s).getBoundingClientRect();
              const name=first('.records-name'), check=first('tbody .gtm-checkbox-target'), identity=first('tbody .records-identity');
              return {name:name.height, checkbox:check.width, checkboxHeight:check.height, checkRight:check.right, identityLeft:identity.left};
            }""")
            assert (
                targets["name"] >= 44
                and targets["checkbox"] >= 44
                and targets["checkboxHeight"] >= 44
            ), targets
            assert targets["checkRight"] <= targets["identityLeft"], targets
            audit(phone, "touch-" + str(width))
            phone.get_by_role("button", name="Table density", exact=True).click()
            assert (
                phone.get_by_role(
                    "menuitemradio", name="Compact rows", exact=True
                ).bounding_box()["height"]
                >= 44
            )
            phone.keyboard.press("Escape")
            phone.screenshot(path=str(out / ("touch-" + str(width) + ".png")))
            if args.workbook_id:
                phone.goto(base + "/workbooks")
                link = phone.locator(".gtm-workbook-link").first
                expect(link).to_be_visible()
                assert link.bounding_box()["height"] >= 44
                assert phone.evaluate("document.documentElement.scrollWidth<=innerWidth")
                audit(phone, "workbooks-touch-" + str(width))
                phone.goto(base + "/workbooks/" + urllib.parse.quote(args.workbook_id, safe=""))
                back = phone.get_by_role("button", name="Back to workbooks", exact=True)
                expect(back).to_be_visible()
                back_box = back.bounding_box()
                title = phone.locator(".workbook-toolbar-heading").bounding_box()
                assert back_box["height"] >= 44 and back_box["width"] >= 44
                # The title belongs alongside Back, including with touch targets.
                assert title["y"] < back_box["y"] + back_box["height"], (title, back_box)
                assert phone.evaluate("document.documentElement.scrollWidth<=innerWidth")
                audit(phone, "workbook-touch-" + str(width))
                phone.screenshot(path=str(out / ("workbook-touch-" + str(width) + ".png")))
        browser.close()
    assert not report["page_errors"], report["page_errors"]
finally:
    save()
    if prior:
        req("POST", "/api/workspaces/switch", {"workspace_id": prior})
print(
    json.dumps(
        {
            "measurements": report["measurements"],
            "axe_scans": len(report["checks"]),
            "page_errors": report["page_errors"],
        },
        indent=2,
    )
)
