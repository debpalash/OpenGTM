from playwright.sync_api import sync_playwright, expect
from pathlib import Path
import json, urllib.request, urllib.parse, time
import argparse, os

parser = argparse.ArgumentParser(
    description="Verify dashboard import, inspection, and enrichment against a local stack. Creates an isolated synthetic workspace; never sends external outreach."
)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument("--axe-path", default="apps/web/node_modules/axe-core/axe.min.js")
parser.add_argument("--output", default=".run/dashboard-journey")
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
base = args.base_url.rstrip("/")
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
shots = out / "captures"
shots.mkdir(exist_ok=True)
creds = {
    "USERNAME": os.environ.get("OPENGTM_QA_USERNAME"),
    "PASSWORD": os.environ.get("OPENGTM_QA_PASSWORD"),
}
if not all(creds.values()):
    parser.error("Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD in the environment.")
headers = {}


def req(method, path, body=None, form=False):
    data = (
        None
        if body is None
        else urllib.parse.urlencode(body).encode()
        if form
        else json.dumps(body).encode()
    )
    request = urllib.request.Request(
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
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        raise Exception(str(error.code) + " " + error.read().decode())


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
ws = req(
    "POST",
    "/api/workspaces",
    {"name": "Workbench UI verification " + str(int(time.time()))},
)["id"]
headers["X-Workspace-Id"] = ws
req("POST", "/api/workspaces/switch", {"workspace_id": ws})
report = {"workspace_id": ws}
errors = []


def log(key, value="pass"):
    report[key] = value
    (out / "journey-results.json").write_text(json.dumps(report, indent=2))
    print(key, value, flush=True)


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
        page.on("pageerror", lambda error: errors.append(str(error)))

        def dialog():
            return page.get_by_role("dialog").last

        def axe(label):
            page.add_script_tag(path=str(root / args.axe_path))
            value = page.evaluate(
                "async()=>await axe.run(document,{runOnly:{type:'tag',values:['wcag2a','wcag2aa','wcag21a','wcag21aa','wcag22aa','best-practice']}})"
            )
            (out / (label + "-axe.json")).write_text(json.dumps(value, indent=2))
            log(
                label + "-axe",
                [(v["id"], len(v["nodes"])) for v in value["violations"]],
            )
            page.screenshot(path=str(shots / (label + ".png")))
            assert not value["violations"], f"Accessibility failures in {label}"

        def open_import(path):
            page.goto(base + path)
            page.get_by_role("button", name="Import from Clay", exact=True).click()
            expect(
                dialog().get_by_role(
                    "heading",
                    name="Import companies · Choose file"
                    if path == "/companies"
                    else "Import people · Choose file",
                    exact=True,
                )
            ).to_be_visible()
            return dialog()

        def upload(d, name, body, accepted, rejected):
            d.get_by_label("Import file").set_input_files(
                {
                    "name": name,
                    "mimeType": "application/json"
                    if name.endswith(".json")
                    else "text/csv",
                    "buffer": body.encode(),
                }
            )
            d.get_by_role("button", name="Map columns", exact=True).click()
            expect(d.get_by_role("heading", name="Match your columns")).to_be_visible(
                timeout=30000
            )
            d.get_by_role("button", name="Validate rows", exact=True).click()
            expect(d.get_by_role("heading", name="Review your import")).to_be_visible(
                timeout=30000
            )
            assert d.locator("dd").all_text_contents() == [
                str(accepted),
                str(rejected),
                str(accepted + rejected),
            ]

        d = open_import("/companies")
        axe("import-upload")
        upload(
            d,
            "clay-companies.csv",
            "Row ID,Company Name,Company Domain,Industry\natlas,Atlas Systems,atlas-ui.test,Software\nbeacon,Beacon Labs,beacon-ui.test,Research\n",
            2,
            0,
        )
        axe("import-review")
        d.get_by_role("button", name="Import 2 rows").click()
        expect(
            d.get_by_role("heading", name="Import complete")
        ).to_be_visible(timeout=30000)
        d.get_by_role("button", name="Done", exact=True).click()
        log("guided_csv_import")
        search = page.get_by_role("searchbox", name="Search companies")
        search.press_sequentially("Atlas Systems", delay=0)
        expect(search).to_have_value("Atlas Systems")
        expect(
            page.get_by_role("button", name="Beacon Labs", exact=True)
        ).not_to_be_visible()
        search.fill("")
        expect(
            page.get_by_role("button", name="Beacon Labs", exact=True)
        ).to_be_visible()
        log("rapid_search_input")
        page.get_by_role("button", name="Atlas Systems", exact=True).click()
        d = dialog()
        expect(d.get_by_role("heading", name="Accepted facts")).to_be_visible()
        axe("company-inspector")
        d.get_by_role("tab", name="Sources", exact=True).click()
        expect(
            d.get_by_role("heading", name="Where your facts come from")
        ).to_be_visible()
        expect(d.get_by_text("Atlas Systems", exact=True).last).to_be_visible()
        expect(d.get_by_text("Update time unavailable",exact=False)).not_to_be_visible()
        axe("company-provenance")
        d.get_by_role("tab", name="Overview", exact=True).click()
        d.get_by_role("button", name="Edit record", exact=True).click()
        d.get_by_label("Industry", exact=True).fill("Life sciences")
        d.get_by_role("button", name="Save shared record", exact=True).click()
        expect(d).not_to_be_visible()
        expect(page.get_by_text("Life sciences", exact=True)).to_be_visible()
        log("inspector_edit_and_provenance")
        page.get_by_role("button", name="Atlas Systems", exact=True).click()
        d = dialog()
        d.get_by_role("button", name="Edit record", exact=True).click()
        d.get_by_label("City", exact=True).fill("Unsaved city")
        page.keyboard.press("Escape")
        confirm = dialog()
        expect(
            confirm.get_by_role("heading", name="Discard your changes?")
        ).to_be_visible()
        confirm.get_by_role("button", name="Keep editing").click()
        expect(d.get_by_label("City", exact=True)).to_have_value("Unsaved city")
        d.get_by_role("button", name="Cancel", exact=True).click()
        dialog().get_by_role("button", name="Discard changes").click()
        expect(
            page.get_by_role("button", name="Atlas Systems", exact=True)
        ).to_be_focused()
        log("dirty_draft_and_focus_return")
        raw = '[{"Row ID":"ana","Full Name":"Ana Rivera","Work Email":"ana@atlas-ui.test","Company Domain":"atlas-ui.test","Job Title":"Director","Custom Identifier":9007199254740993},{"Row ID":"bo","Full Name":"Bo Chen","Work Email":"bo@atlas-ui.test","Company Domain":"atlas-ui.test","Job Title":"Analyst"},{"Row ID":"bad","Full Name":"Invalid person","Work Email":"not-email"}]'
        d = open_import("/people")
        upload(d, "clay-people.json", raw, 2, 1)

        def lost_response(route):
            response = route.fetch()
            assert response.status == 200
            route.fulfill(
                status=503,
                content_type="application/json",
                body='{"detail":"Simulated lost response after commit"}',
            )

        page.route("**/api/record-imports", lost_response)
        d.get_by_role("button", name="Import 2 rows").click()
        expect(d.get_by_role("alert")).to_contain_text("Simulated lost response")
        page.unroute("**/api/record-imports", lost_response)
        d.get_by_role("button", name="Cancel", exact=True).click()
        dialog().get_by_role("button", name="Discard changes", exact=True).click()
        expect(page.get_by_role("dialog")).to_have_count(0)
        page.reload()
        page.get_by_role("button", name="Import from Clay", exact=True).click()
        d = dialog()
        d.get_by_role("button", name="Check batch status").click()
        expect(
            d.get_by_role("heading", name="Completed batch recovered")
        ).to_be_visible()
        d.get_by_role("button", name="Done", exact=True).click()
        assert req("GET", "/api/records/person")["total"] == 2
        log("partial_rejection_and_lost_response_recovery")
        page.get_by_role("button", name="Ana Rivera", exact=True).click()
        d = dialog()
        expect(d.get_by_text("9007199254740993", exact=True)).to_be_visible()
        d.get_by_role("tab", name="Employment", exact=True).click()
        expect(
            d.get_by_role("tabpanel", name="Employment", exact=True).get_by_text(
                "Atlas Systems", exact=True
            )
        ).to_be_visible()
        axe("person-employment")
        d.get_by_role("tab", name="Overview", exact=True).click()
        d.get_by_role("button", name="Edit record", exact=True).click()
        d.get_by_label("Phone", exact=True).fill("+1 555 0100")
        d.get_by_role("button", name="Save shared record", exact=True).click()
        expect(d).not_to_be_visible()
        person = req("GET", "/api/records/person?search=Ana")["records"][0]
        assert person["fields"]["Custom Identifier"] == 9007199254740993
        log("large_identifier_display_and_edit_precision")
        page.get_by_role("button", name="More people actions").click()
        page.get_by_role("menuitem", name="Enrichment workflow", exact=True).click()
        d = dialog()
        d.get_by_label("Enrichment provider", exact=True).select_option("lead_scorer")
        d.get_by_label("Target field", exact=True).select_option("score")
        d.get_by_role("button", name="Add", exact=True).click()
        d.get_by_label("Target field", exact=True).select_option("score_tier")
        d.get_by_role("button", name="Add", exact=True).click()
        d.get_by_role("button", name="Save workflow", exact=True).click()
        expect(d).not_to_be_visible()
        log("workflow_configuration")
        page.get_by_role(
            "checkbox", name="Select all records on this page", exact=True
        ).check()
        page.get_by_role("button", name="Enrich selected", exact=True).click()
        page.wait_for_url("**/workbooks/*")
        wb = page.url.split("/workbooks/")[1].split("?")[0]
        d = dialog()
        expect(d.get_by_role("heading", name="Review workbook run")).to_be_visible(
            timeout=30000
        )
        assert req("GET", "/api/workbooks/" + wb)["workbook"]["status"] != "running"
        expect(d.get_by_role("button", name="Start run", exact=True)).to_be_enabled(
            timeout=30000
        )
        axe("enrichment-review")
        d.get_by_role("button", name="Start run", exact=True).click()
        expect(d).not_to_be_visible(timeout=30000)
        for _ in range(120):
            detail = req("GET", "/api/workbooks/" + wb)
            if detail["workbook"]["status"] == "complete":
                break
            page.wait_for_timeout(250)
        assert detail["workbook"]["status"] == "complete", detail["workbook"]
        page.wait_for_timeout(1800)
        expect(
            page.locator(".workbook-record-identity").filter(has_text="Ana Rivera")
        ).to_be_visible(timeout=10000)
        assert page.get_by_role("grid").get_attribute("aria-rowcount") == "3"
        axe("workbook-identity")
        log("reviewed_native_enrichment_and_grid_identity", wb)
        page.goto(base + "/people")
        page.get_by_role("button", name="More people actions").click()
        with page.expect_download() as download:
            page.get_by_role("menuitem", name="Export table CSV", exact=True).click()
        assert "9007199254740993" in Path(download.value.path()).read_text()
        log("csv_export_precision")
        page.get_by_role("button", name="More people actions").click()
        with page.expect_download() as download:
            page.get_by_role(
                "menuitem", name="Back up records JSON", exact=True
            ).click()
        snapshot = Path(download.value.path()).read_text()
        assert "9007199254740993" in snapshot
        log("backup_precision")
        page.goto(base + "/overview")
        expect(page.get_by_role("heading", name="Workspace overview")).to_be_visible()
        expect(
            page.locator(".overview-metric")
            .filter(has_text="Companies")
            .locator("strong")
        ).to_have_text("2")
        expect(page.locator(".overview-metric").nth(1).locator("strong")).to_have_text(
            "2"
        )
        axe("overview-populated")
        log("canonical_overview_counts")
        for width in [390, 320]:
            page.set_viewport_size({"width": width, "height": 844})
            page.goto(base + "/people")
            expect(
                page.get_by_role("button", name="Ana Rivera", exact=True)
            ).to_be_visible()
            assert page.evaluate("document.documentElement.scrollWidth<=innerWidth")
            page.get_by_role("button", name="Ana Rivera", exact=True).click()
            expect(dialog()).to_be_visible()
            axe("inspector-mobile-" + str(width))
            dialog().get_by_role("button", name="Close record", exact=True).click()
        log("mobile_320_390_reflow")
        log("page_errors", errors)
        assert not errors
        browser.close()
except Exception:
    try:
        page.screenshot(path=str(shots / "journey-failure.png"))
        (out / "journey-failure.txt").write_text(page.locator("body").inner_text())
    except:
        pass
    raise
finally:
    headers.pop("X-Workspace-Id", None)
    if prior:
        req("POST", "/api/workspaces/switch", {"workspace_id": prior})
