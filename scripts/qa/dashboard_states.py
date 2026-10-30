from playwright.sync_api import sync_playwright, expect
from pathlib import Path
import json, urllib.request, urllib.parse
import argparse, os

parser = argparse.ArgumentParser(
    description="Exercise dashboard failure states, view-only controls, keyboard navigation, resize/reorder, zoom and responsive layout. Uses the synthetic workspace/workbook from dashboard_journey.py and updates its column layout."
)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument(
    "--journey-report", default=".run/dashboard-journey/journey-results.json"
)
parser.add_argument("--output", default=".run/dashboard-states")
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
base = args.base_url.rstrip("/")
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
report = {}
creds = {
    "USERNAME": os.environ.get("OPENGTM_QA_USERNAME"),
    "PASSWORD": os.environ.get("OPENGTM_QA_PASSWORD"),
}
if not all(creds.values()):
    parser.error("Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD.")
headers = {}


def req(method, path, body=None, form=False):
    data = (
        None
        if body is None
        else urllib.parse.urlencode(body).encode()
        if form
        else json.dumps(body).encode()
    )
    with urllib.request.urlopen(
        urllib.request.Request(
            base + path,
            data=data,
            headers={
                **headers,
                "Content-Type": "application/x-www-form-urlencoded"
                if form
                else "application/json",
            },
            method=method,
        )
    ) as r:
        return json.load(r)


headers["Authorization"] = (
    "Bearer "
    + req(
        "POST",
        "/auth/token",
        {"username": creds["USERNAME"], "password": creds["PASSWORD"]},
        True,
    )["access_token"]
)
prior = req("GET", "/api/workspaces")["active_id"]
state = json.loads((root / args.journey_report).read_text())
ws = state["workspace_id"]
wb = state["reviewed_native_enrichment_and_grid_identity"]
req("POST", "/api/workspaces/switch", {"workspace_id": ws})
headers["X-Workspace-Id"] = ws
try:
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True, executable_path=args.browser_path)
        context = browser.new_context(
            viewport={"width": 1440, "height": 1000}, reduced_motion="reduce"
        )
        context.add_init_script(
            'localStorage.setItem("yupcha_token",'
            + json.dumps(headers["Authorization"][7:])
            + ');localStorage.setItem("yupcha_workspace_id",'
            + json.dumps(ws)
            + ');localStorage.setItem("theme","light")'
        )
        page = context.new_page()
        errors = []
        page.on("pageerror", lambda e: errors.append(str(e)))

        def log(key, value="pass"):
            report[key] = value
            (out / "results.json").write_text(json.dumps(report, indent=2))
            print(key, value, flush=True)

        page.goto(base + "/overview")
        expect(page.locator(".overview-metric").first.locator("strong")).to_have_text(
            "2"
        )

        def fail(route):
            route.fulfill(
                status=503,
                content_type="application/json",
                body='{"detail":"Service unavailable for test"}',
            )

        page.route("**/api/records/summary", fail)
        page.get_by_role("button", name="Refresh workspace overview").click()
        expect(page.get_by_role("alert")).to_contain_text(
            "last successful summary", timeout=10000
        )
        expect(page.locator(".overview-metric").first.locator("strong")).to_have_text(
            "2"
        )
        log("overview_stale_data_after_503")
        page.unroute("**/api/records/summary", fail)

        def malformed(route):
            route.fulfill(status=200, content_type="application/json", body="{}")

        page.route("**/api/records/summary", malformed)
        page.reload()
        expect(
            page.get_by_role("heading", name="Your overview is temporarily unavailable")
        ).to_be_visible(timeout=10000)
        expect(page.locator(".overview-metric")).to_have_count(0)
        page.unroute("**/api/records/summary", malformed)
        page.get_by_role("button", name="Try again", exact=True).click()
        expect(page.locator(".overview-metric").first.locator("strong")).to_have_text(
            "2"
        )
        log("malformed_summary_and_retry")
        page.route("**/api/analytics/overview", fail)
        page.goto(base + "/analytics")
        expect(
            page.get_by_role("heading", name="Analytics is temporarily unavailable")
        ).to_be_visible(timeout=10000)
        expect(page.get_by_text("Scope: legacy leads", exact=True)).not_to_be_visible()
        page.unroute("**/api/analytics/overview", fail)
        page.get_by_role("button", name="Try again", exact=True).click()
        expect(page.get_by_text("Scope: legacy leads", exact=True)).to_be_visible(
            timeout=10000
        )
        log("analytics_503_and_retry")

        def readonly(route):
            route.fulfill(
                status=200, content_type="application/json", body='{"can_write":false}'
            )

        page.route("**/api/records/access", readonly)
        page.goto(base + "/companies")
        expect(
            page.get_by_role("button", name="Import from Clay", exact=True)
        ).to_be_disabled()
        expect(
            page.get_by_role("button", name="New company", exact=True)
        ).to_be_disabled()
        page.get_by_role("button", name="Atlas Systems", exact=True).click()
        d = page.get_by_role("dialog")
        expect(d.get_by_role("button", name="Edit record", exact=True)).to_be_disabled()
        d.get_by_role("button", name="Close record", exact=True).click()
        page.unroute("**/api/records/access", readonly)
        log("record_view_only_state")
        page.goto(base + "/workbooks/" + wb)
        grid = page.get_by_role("grid")
        expect(grid).to_be_visible()
        cell = page.locator('[data-grid-row="0"][data-grid-column="2"]')
        cell.focus()
        page.keyboard.press("ArrowRight")
        expect(
            page.locator('[data-grid-row="0"][data-grid-column="3"]')
        ).to_be_focused()
        page.keyboard.press("ArrowLeft")
        expect(
            page.locator('[data-grid-row="0"][data-grid-column="2"]')
        ).to_be_focused()
        page.keyboard.press("ArrowLeft")
        expect(
            page.locator('[data-grid-row="0"][data-grid-column="1"]')
        ).to_be_focused()
        page.keyboard.press("ArrowLeft")
        expect(
            page.get_by_role("checkbox", name="Select row", exact=False).first
        ).to_be_focused()
        page.keyboard.press("ArrowRight")
        expect(
            page.locator('[data-grid-row="0"][data-grid-column="1"]')
        ).to_be_focused()
        log("grid_keyboard_roving_and_checkbox")
        widths = page.locator("thead [data-col-id]").evaluate_all(
            "(elements)=>elements.slice(0,3).map(e=>Math.round(e.getBoundingClientRect().width))"
        )
        assert widths == [36, 50, 240], widths
        log("pinned_column_widths", widths)
        resize = page.get_by_role("separator", name="Resize score column", exact=True)
        before = int(resize.get_attribute("aria-valuenow"))
        resize.focus()
        delta = -10 if before > 590 else 10
        page.keyboard.press("ArrowLeft" if delta < 0 else "ArrowRight")
        expect(resize).to_have_attribute("aria-valuenow", str(before + delta))
        log("keyboard_column_resize")
        before_order = page.locator("thead [data-col-id]").evaluate_all(
            "(nodes)=>nodes.slice(3).map(node=>node.dataset.colId)"
        )
        drag = page.get_by_role("button", name="Move score column", exact=True)
        score_id = drag.locator("xpath=ancestor::th").get_attribute("data-col-id")
        drag.focus()
        page.keyboard.press("Space")
        expect(drag).to_have_attribute("aria-pressed", "true")
        # The keyboard sensor attaches its document listener on the next task.
        # Pressed state alone does not prove that listener is installed yet.
        page.evaluate("()=>new Promise(resolve=>setTimeout(resolve,0))")
        page.keyboard.press(
            "ArrowLeft" if before_order.index(score_id) > 0 else "ArrowRight"
        )
        page.wait_for_timeout(100)
        page.keyboard.press("Space")
        page.wait_for_function(
            '(before)=>JSON.stringify([...document.querySelectorAll("thead [data-col-id]")].slice(3).map(node=>node.dataset.colId))!==JSON.stringify(before)',
            arg=before_order,
        )
        log("keyboard_column_reorder")
        page.goto(base + "/companies")
        page.get_by_role("button", name="Import from Clay", exact=True).click()
        d = page.get_by_role("dialog")
        page.wait_for_timeout(100)
        for _ in range(20):
            page.keyboard.press("Tab")
            page.wait_for_timeout(30)
            assert page.evaluate('!!document.activeElement.closest("[role=dialog]")')
        page.keyboard.press("Escape")
        expect(
            page.get_by_role("button", name="Import from Clay", exact=True)
        ).to_be_focused()
        log("import_keyboard_trap_and_escape")
        geometry = []
        for width in [320, 390, 768, 1024, 1280, 1440, 1920]:
            page.set_viewport_size({"width": width, "height": 900})
            page.goto(base + "/companies")
            expect(
                page.get_by_role("button", name="Atlas Systems", exact=True)
            ).to_be_visible()
            value = page.evaluate(
                '()=>({width:innerWidth,document:document.documentElement.scrollWidth,header:document.querySelector(".workbench-page-header").getBoundingClientRect().height,toolbar:document.querySelector(".workbench-toolbar").getBoundingClientRect().height})'
            )
            assert value["document"] <= width, value
            geometry.append(value)
        log("responsive_geometry", geometry)
        page.set_viewport_size({"width": 1440, "height": 1000})
        page.goto(base + "/overview")
        page.evaluate('document.documentElement.style.fontSize="200%"')
        expect(page.get_by_role("heading", name="Workspace overview")).to_be_visible()
        assert page.evaluate("document.documentElement.scrollWidth<=innerWidth")
        text_fit = page.evaluate("""() => {
          const contained = (inner, outer) => inner.top >= outer.top - 1 && inner.bottom <= outer.bottom + 1;
          const crowded = [...document.querySelectorAll('nav [data-sidebar="menu-button"]')].filter(row => {
            const bounds = row.getBoundingClientRect();
            return [...row.children].some(child => !contained(child.getBoundingClientRect(), bounds));
          }).map(row => row.textContent.trim());
          const workspace = document.querySelector('[title="Switch workspace"]');
          const breadcrumb = document.querySelector('.gtm-breadcrumb').getBoundingClientRect();
          const toolbar = document.querySelector('.gtm-topbar').getBoundingClientRect();
          return {crowdedNavigation: crowded,
            workspaceFits: contained(workspace.querySelector('.grid').getBoundingClientRect(), workspace.getBoundingClientRect()),
            breadcrumbFits: contained(breadcrumb, toolbar)};
        }""")
        assert not text_fit["crowdedNavigation"] and text_fit["workspaceFits"] and text_fit["breadcrumbFits"], text_fit
        log("enlarged_text_fits_navigation_and_workspace", text_fit)
        page.screenshot(path=str(out / "text-zoom-200.png"))
        log("overview_text_zoom_200")
        page.evaluate('document.documentElement.style.fontSize=""')
        page.emulate_media(forced_colors="active")
        page.goto(base + "/companies")
        expect(
            page.get_by_role("button", name="Atlas Systems", exact=True)
        ).to_be_visible()
        page.screenshot(path=str(out / "forced-colors.png"))
        log("forced_colors_rendered")
        page.emulate_media(forced_colors="none")
        reduced = page.evaluate(
            '()=>[...document.querySelectorAll("*")].filter(e=>getComputedStyle(e).animationName!=="none").length'
        )
        assert reduced == 0, reduced
        log("reduced_motion", reduced)
        log("page_errors", errors)
        assert not errors
        browser.close()
finally:
    headers.pop("X-Workspace-Id", None)
    if prior:
        req("POST", "/api/workspaces/switch", {"workspace_id": prior})
