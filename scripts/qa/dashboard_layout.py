from playwright.sync_api import sync_playwright, expect
from pathlib import Path
import json, urllib.request, urllib.parse
import argparse, os

parser = argparse.ArgumentParser(
    description="Verify non-drag column layout and activity controls against an isolated journey fixture. Saves only that synthetic workbook layout."
)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument(
    "--journey-report", default=".run/dashboard-journey/journey-results.json"
)
parser.add_argument("--output", default=".run/dashboard-layout")
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
try:
    req("POST", "/api/workspaces/switch", {"workspace_id": ws})
    headers["X-Workspace-Id"] = ws
    initial = req("GET", "/api/workbooks/" + wb)
    columns = initial["workbook"]["columns_config"]
    assert len(columns) >= 2
    first, second = columns[:2]
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
        scans = []
        page.on("pageerror", lambda e: errors.append(str(e)))

        def log(name):
            report[name] = "pass"
            (out / "results.json").write_text(json.dumps(report, indent=2))
            print(name, "pass", flush=True)

        def audit(name):
            page.add_script_tag(
                path=str(root / "apps/web/node_modules/axe-core/axe.min.js")
            )
            result = page.evaluate("async()=>await axe.run(document)")
            scans.append({"name": name, "violations": result["violations"]})
            report["axe"] = scans
            assert not result["violations"], [
                (v["id"], [n["html"] for n in v["nodes"]]) for v in result["violations"]
            ]

        def load():
            page.goto(base + "/workbooks/" + wb)
            expect(
                page.get_by_role(
                    "button", name="Configure " + first["name"] + " column", exact=True
                )
            ).to_be_visible()

        def settings():
            page.get_by_role(
                "button", name="Configure " + first["name"] + " column", exact=True
            ).click()
            return page.get_by_role("dialog", name=first["name"] + " column settings")

        def order():
            return [
                c["id"]
                for c in req("GET", "/api/workbooks/" + wb)["workbook"][
                    "columns_config"
                ]
            ]

        load()
        dialog = settings()
        expect(
            dialog.get_by_role("button", name="Move left", exact=True)
        ).to_be_disabled()
        dialog.get_by_role("button", name="Move right", exact=True).click()
        expect(dialog.get_by_role("status")).to_contain_text("Column order saved")
        assert order()[:2] == [second["id"], first["id"]]
        dialog.get_by_label("Column position", exact=True).select_option(second["id"])
        dialog.get_by_role("button", name="Save position", exact=True).click()
        expect(dialog.get_by_role("status")).to_contain_text("position 1")
        assert order()[:2] == [first["id"], second["id"]]
        log("pointer_column_move_and_position_persist")
        route_pattern = "**/api/workbooks/" + wb + "/columns/order"

        def fail(route):
            route.fulfill(
                status=503,
                content_type="application/json",
                body='{"detail":"Unavailable"}',
            )

        page.route(route_pattern, fail)
        dialog.get_by_role("button", name="Move right", exact=True).click()
        expect(dialog.get_by_role("alert")).to_contain_text("not saved")
        expect(dialog.get_by_label("Column position", exact=True)).to_have_value(
            second["id"]
        )
        assert order()[:2] == [first["id"], second["id"]]
        page.unroute(route_pattern, fail)
        dialog.get_by_role("button", name="Save position", exact=True).click()
        expect(dialog.get_by_role("status")).to_contain_text("position 2")

        def conflict(route):
            route.fulfill(
                status=409,
                content_type="application/json",
                body='{"detail":"Changed elsewhere"}',
            )

        page.route(route_pattern, conflict)
        dialog.get_by_role("button", name="Move left", exact=True).click()
        expect(dialog.get_by_role("alert")).to_contain_text("Column order changed")
        assert order()[:2] == [second["id"], first["id"]]
        page.unroute(route_pattern, conflict)
        dialog.get_by_role("button", name="Save position", exact=True).click()
        expect(dialog.get_by_role("status")).to_contain_text("position 1")
        log("failed_and_conflicted_order_retains_choice")
        dialog.get_by_role("button", name="Close column settings", exact=True).click()
        for resize_key in ["Home", "End", "Home"]:
            before_order = order()
            handle = page.get_by_role(
                "separator", name="Resize " + first["name"] + " column", exact=True
            )
            handle.focus()
            handle.press(resize_key)
            if resize_key == "Home":
                expect(handle).to_have_attribute("aria-valuenow", "80")
                targets = page.evaluate(
                    """id=>[...document.querySelector('th[data-col-id="'+id+'"]').querySelectorAll('button')].map(node=>{const r=node.getBoundingClientRect();return {left:r.left,right:r.right,width:r.width,height:r.height}})""",
                    first["id"],
                )
                assert all(
                    target["width"] >= 24 and target["height"] >= 24
                    for target in targets
                ), targets
                assert all(
                    targets[index]["right"] <= targets[index + 1]["left"] + 1
                    for index in range(len(targets) - 1)
                ), targets
            drag = page.get_by_role(
                "button", name="Move " + first["name"] + " column", exact=True
            )
            drag.focus()
            drag.press("Space")
            expect(drag).to_have_attribute("aria-pressed", "true")
            # KeyboardSensor installs its document listener on the next task.
            # After pickup is ready, Arrow and drop still share one task below.
            page.evaluate("()=>new Promise(resolve=>setTimeout(resolve,0))")
            index = before_order.index(first["id"])
            direction = "ArrowRight" if index == 0 else "ArrowLeft"
            expected = before_order.copy()
            moved = expected.pop(index)
            expected.insert(index + (1 if direction == "ArrowRight" else -1), moved)
            with page.expect_response(
                lambda response: response.request.method == "PATCH"
                and response.url.endswith("/columns/order")
            ) as saved_order:
                # Both events in one browser task deterministically expose stale
                # collision state; this used to lose every immediate keyboard drop.
                page.evaluate(
                    """direction=>{const element=document.activeElement;for(const code of [direction,'Space']) element.dispatchEvent(new KeyboardEvent('keydown',{key:code==='Space'?' ':code,code,bubbles:true,cancelable:true}))}""",
                    direction,
                )
            assert saved_order.value.status == 200
            page.wait_for_function(
                "expected=>JSON.stringify([...document.querySelectorAll('thead [data-col-id]')].slice(3).map(node=>node.dataset.colId))===JSON.stringify(expected)",
                arg=expected,
            )
            assert order() == expected
            expect(
                page.get_by_role(
                    "button", name="Move " + first["name"] + " column", exact=True
                )
            ).to_be_focused()
        log("immediate_keyboard_drop_after_resize")
        before_pointer = order()
        source = page.get_by_role(
            "button", name="Move " + second["name"] + " column", exact=True
        )
        target = page.get_by_role(
            "button", name="Move " + first["name"] + " column", exact=True
        ).locator("xpath=ancestor::th")
        source.hover()
        page.mouse.down()
        bounds = target.bounding_box()
        page.mouse.move(
            bounds["x"] + bounds["width"] / 2,
            bounds["y"] + bounds["height"] / 2,
            steps=8,
        )
        expect(source).to_have_attribute("aria-pressed", "true")
        with page.expect_response(
            lambda response: response.request.method == "PATCH"
            and response.url.endswith("/columns/order")
        ) as saved_pointer:
            page.mouse.up()
        assert saved_pointer.value.status == 200
        expected_pointer = before_pointer.copy()
        pointer_target = expected_pointer.index(first["id"])
        expected_pointer.remove(second["id"])
        expected_pointer.insert(pointer_target, second["id"])
        page.wait_for_function(
            "expected=>JSON.stringify([...document.querySelectorAll('thead [data-col-id]')].slice(3).map(node=>node.dataset.colId))===JSON.stringify(expected)",
            arg=expected_pointer,
        )
        log("pointer_drag_after_keyboard_drag")
        before_cancel = order()
        canceled_requests = []

        def collect_order(request):
            if request.method == "PATCH" and request.url.endswith("/columns/order"):
                canceled_requests.append(request.url)

        page.on("request", collect_order)
        drag = page.get_by_role(
            "button", name="Move " + first["name"] + " column", exact=True
        )
        drag.focus()
        drag.press("Space")
        expect(drag).to_have_attribute("aria-pressed", "true")
        drag.press(
            "ArrowRight" if before_cancel.index(first["id"]) == 0 else "ArrowLeft"
        )
        drag.press("Escape")
        expect(drag).not_to_have_attribute("aria-pressed", "true")
        assert not canceled_requests and order() == before_cancel
        page.remove_listener("request", collect_order)
        log("keyboard_drag_cancel_does_not_save")
        dialog = settings()
        width = dialog.get_by_label("Column width (pixels)", exact=True)
        width.fill("79")
        dialog.get_by_role("button", name="Save width", exact=True).click()
        expect(width).to_have_attribute("aria-invalid", "true")
        width.fill("222")
        dialog.get_by_role("button", name="Save width", exact=True).click()
        expect(dialog.get_by_text("Column width saved.", exact=True)).to_be_visible()
        for theme in ["light", "dark"]:
            page.evaluate("value=>window.OpenGTMTheme.setPreference(value)", theme)
            audit("settings-" + theme)
            page.screenshot(path=str(out / ("settings-" + theme + ".png")))
        dialog.get_by_role("button", name="Close column settings", exact=True).click()
        expect(
            page.get_by_role(
                "button", name="Configure " + first["name"] + " column", exact=True
            )
        ).to_be_focused()
        load()
        dialog = settings()
        expect(dialog.get_by_label("Column width (pixels)", exact=True)).to_have_value(
            "222"
        )
        dialog.get_by_role("button", name="Close column settings", exact=True).click()
        log("numeric_width_validation_and_reload")

        def open_activity():
            page.get_by_role("button", name="Activity", exact=True).click()
            return page.get_by_role("region", name="Workbook activity", exact=True)

        panel = open_activity()
        panel.get_by_label("Activity panel height", exact=True).select_option("160")
        expect(panel).to_have_css("height", "160px")
        separator = panel.get_by_role("separator", name="Resize activity panel")
        separator.focus()
        separator.press("ArrowUp")
        expect(separator).to_have_attribute("aria-valuenow", "170")
        panel.get_by_label("Activity panel height", exact=True).select_option("260")
        panel.get_by_role("button", name="Close activity panel", exact=True).click()
        expect(page.get_by_role("button", name="Activity", exact=True)).to_be_focused()
        load()
        panel = open_activity()
        expect(panel).to_have_css("height", "260px")
        log("panel_pointer_keyboard_size_and_persistence")
        for theme in ["light", "dark"]:
            page.evaluate("value=>window.OpenGTMTheme.setPreference(value)", theme)
            for tab in ["Changes", "Columns", "Errors"]:
                panel.get_by_role("tab", name=tab, exact=True).click()
                audit("panel-" + theme + "-" + tab.lower())
            panel.get_by_role("tab", name="Columns", exact=True).click()
            page.screenshot(path=str(out / ("panel-" + theme + ".png")))
        panel.get_by_role("tab", name="Changes", exact=True).focus()
        page.keyboard.press("ArrowRight")
        expect(panel.get_by_role("tab", name="Columns", exact=True)).to_be_focused()
        page.keyboard.press("Escape")
        expect(panel).not_to_be_visible()
        expect(page.get_by_role("button", name="Activity", exact=True)).to_be_focused()
        log("activity_tabs_keyboard_and_close_focus")
        pattern = "**/api/workbooks/" + wb + "?*"

        many_errors = False

        def persisted_failure(route):
            result = route.fetch().json()
            result["workbook"]["status"] = "complete"
            result["rows"][0]["enrichments"][first["id"]] = {
                "status": "error",
                "value": None,
                "error": "QA persisted rate limit",
            }
            result["rows"][0]["enrichments"][second["id"]] = {
                "status": "skipped",
                "value": None,
                "error": "QA already populated",
            }
            if many_errors:
                original = result["rows"][0]
                result["rows"] = [
                    dict(original, row_id=1000000 + index, position=index + 1)
                    for index in range(75)
                ]
                result["total_rows"] = 75
                result["query_total_rows"] = 75
                result["workbook"]["total_rows"] = 75
            route.fulfill(
                status=200, content_type="application/json", body=json.dumps(result)
            )

        page.route(pattern, persisted_failure)
        load()
        panel = open_activity()
        panel.get_by_role("tab", name="Errors (1)", exact=True).click()
        expect(panel.get_by_text("QA persisted rate limit", exact=True)).to_be_visible()
        panel.get_by_role("tab", name="Columns", exact=True).click()
        expect(
            panel.get_by_role("group", name=first["name"] + " progress", exact=True)
        ).to_contain_text("1 failed")
        expect(
            panel.get_by_role("group", name=second["name"] + " progress", exact=True)
        ).to_contain_text("1 skipped")
        panel.get_by_role("tab", name="Changes", exact=True).click()
        expect(panel.locator(".activity-event")).to_have_count(0)
        log("reloaded_error_and_skips_are_honest")
        many_errors = True
        load()
        panel = open_activity()
        panel.get_by_role("tab", name="Errors (75)", exact=True).click()
        expect(panel.locator(".activity-event")).to_have_count(50)
        expect(panel.get_by_role("status")).to_contain_text("1–50 of 75")
        panel.get_by_role("button", name="Next error page", exact=True).click()
        expect(panel.locator(".activity-event")).to_have_count(25)
        expect(panel.get_by_role("status")).to_contain_text("51–75 of 75")
        expect(
            panel.get_by_role("button", name="Next error page", exact=True)
        ).to_be_disabled()
        panel.get_by_role("button", name="Previous error page", exact=True).click()
        expect(panel.locator(".activity-event")).to_have_count(50)
        log("large_error_list_is_bounded_and_paginated")
        for width in [320, 390, 768]:
            page.set_viewport_size({"width": width, "height": 844})
            assert page.evaluate("document.documentElement.scrollWidth<=innerWidth")
            audit("panel-narrow-" + str(width))
            page.screenshot(path=str(out / ("panel-" + str(width) + ".png")))
        page.set_viewport_size({"width": 1440, "height": 600})
        panel.get_by_label("Activity panel height", exact=True).select_option("330")
        expect(panel).to_have_css("height", "330px")
        assert page.get_by_role("button", name="Review run", exact=True).is_visible()
        log("short_height_cap")
        page.unroute(pattern, persisted_failure)
        touch = browser.new_context(
            viewport={"width": 320, "height": 844},
            has_touch=True,
            is_mobile=True,
            reduced_motion="reduce",
        )
        touch.add_init_script(
            'localStorage.setItem("yupcha_token",'
            + json.dumps(headers["Authorization"][7:])
            + ');localStorage.setItem("yupcha_workspace_id",'
            + json.dumps(ws)
            + ');localStorage.setItem("theme","light")'
        )
        page = touch.new_page()
        page.on("pageerror", lambda error: errors.append(str(error)))
        load()
        dialog = settings()
        assert page.evaluate("document.documentElement.scrollWidth<=innerWidth")
        assert (
            dialog.get_by_role("button", name="Move right", exact=True).bounding_box()[
                "height"
            ]
            >= 44
        )
        audit("settings-touch-320")
        page.screenshot(path=str(out / "settings-touch-320.png"))
        dialog.get_by_role("button", name="Close column settings", exact=True).click()
        panel = open_activity()
        for tab in ["Changes", "Columns", "Errors"]:
            panel.get_by_role("tab", name=tab, exact=True).click()
            assert (
                panel.get_by_role("tab", name=tab, exact=True).bounding_box()["height"]
                >= 44
            )
            audit("panel-touch-320-" + tab.lower())
        log("touch_settings_and_activity_targets")
        report["page_errors"] = errors
        assert not errors, errors
        browser.close()
finally:
    (out / "results.json").write_text(json.dumps(report, indent=2))
    headers.pop("X-Workspace-Id", None)
    if prior:
        req("POST", "/api/workspaces/switch", {"workspace_id": prior})
