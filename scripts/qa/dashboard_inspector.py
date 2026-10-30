from playwright.sync_api import sync_playwright, expect
from pathlib import Path
import argparse, json, os, urllib.request, urllib.parse, time

parser = argparse.ArgumentParser(
    description="Verify inspector drafts, real version conflicts, typed fields, navigation protection and company people using an isolated workspace."
)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument("--output", default=".run/dashboard-inspector")
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
base = args.base_url.rstrip("/")
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
headers = {}
creds = {
    "USERNAME": os.environ.get("OPENGTM_QA_USERNAME"),
    "PASSWORD": os.environ.get("OPENGTM_QA_PASSWORD"),
}
report = {}
if not all(creds.values()):
    parser.error("Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD.")


def req(method, path, body=None, form=False):
    data = (
        None
        if body is None
        else urllib.parse.urlencode(body).encode()
        if form
        else json.dumps(body).encode()
    )
    try:
        with urllib.request.urlopen(
            urllib.request.Request(
                base + path,
                method=method,
                data=data,
                headers={
                    **headers,
                    "Content-Type": "application/x-www-form-urlencoded"
                    if form
                    else "application/json",
                },
            ),
            timeout=60,
        ) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        raise RuntimeError(str(error.code) + " " + error.read().decode())


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
ws = req("POST", "/api/workspaces", {"name": "Inspector QA " + str(int(time.time()))})[
    "id"
]
errors = []
scans = []
try:
    req("POST", "/api/workspaces/switch", {"workspace_id": ws})
    headers["X-Workspace-Id"] = ws
    company = req(
        "POST",
        "/api/records/company",
        {
            "company": {
                "name": "Atlas Research",
                "domain": "inspector-atlas.test",
                "city": "Pune",
                "industry": "Software",
                "description": "Original description",
                "seats": 10,
                "enabled": True,
                "metadata": {"tag": "original"},
                "large_id": 9007199254740993,
            }
        },
    )
    other = req(
        "POST",
        "/api/records/company",
        {"company": {"name": "Boreal Research", "domain": "inspector-boreal.test"}},
    )
    ana = req(
        "POST",
        "/api/records/person",
        {
            "person": {"full_name": "Ana Rivera", "email": "ana@inspector-atlas.test"},
            "company_id": company["id"],
            "employment": {"title": "Engineer"},
        },
    )
    unknown = req(
        "POST",
        "/api/records/person",
        {
            "person": {
                "full_name": "Role without employer",
                "email": "independent@inspector.test",
            }
        },
    )
    req(
        "POST",
        "/api/records/person",
        {
            "person": {
                "full_name": "Unrelated Person",
                "email": "unrelated@inspector.test",
            },
            "company_id": other["id"],
        },
    )
    from concurrent.futures import ThreadPoolExecutor

    def member(index):
        return req(
            "POST",
            "/api/records/person",
            {
                "person": {
                    "full_name": "QA Member " + str(index).zfill(3),
                    "email": "member" + str(index) + "@inspector.test",
                },
                "company_id": company["id"],
                "employment": {"title": "Analyst"},
            },
        )

    with ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(member, range(1, 56)))
    report.update(workspace_id=ws, company_id=company["id"], person_id=ana["id"])
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

        def open_record(kind, record):
            page.goto(
                base
                + ("/companies" if kind == "company" else "/people")
                + "?record="
                + record["id"]
            )
            dialog = page.get_by_role(
                "dialog",
                name=record["fields"].get("name", record["fields"].get("full_name")),
                exact=True,
            )
            expect(
                dialog.get_by_role("button", name="Edit record", exact=True)
            ).to_be_enabled()
            expect(
                dialog.get_by_role("button", name="Refresh record", exact=True)
            ).to_be_enabled()
            return dialog

        def add_field(dialog, name, kind, value=None):
            dialog.get_by_label("Custom field name", exact=True).fill(name)
            dialog.get_by_label("Custom field type", exact=True).select_option(kind)
            dialog.get_by_role("button", name="Add custom field", exact=True).click()
            if value is not None:
                dialog.get_by_label(
                    name.replace("_", " ").capitalize(), exact=True
                ).fill(value)

        dialog = open_record("company", company)
        dialog.get_by_role("button", name="Edit record", exact=True).click()
        dialog.get_by_label("City", exact=True).fill("Mine city")
        dialog.get_by_label("Description", exact=True).fill("Mine description")
        add_field(dialog, "capacity", "number", "not a number")
        dialog.get_by_role("button", name="Save shared record", exact=True).click()
        expect(dialog.get_by_label("Capacity", exact=True)).to_have_attribute(
            "aria-invalid", "true"
        )
        expect(dialog.get_by_label("City", exact=True)).to_have_value("Mine city")
        audit("invalid-number")
        dialog.get_by_label("Capacity", exact=True).fill("0")
        add_field(dialog, "allow_export", "boolean")
        add_field(dialog, "routing_rules", "json", '{"tag":"demo"}')
        add_field(dialog, "import_note", "text")
        dialog.get_by_label("Metadata", exact=True).fill('{"broken"')
        dialog.get_by_role("button", name="Save shared record", exact=True).click()
        expect(dialog.get_by_label("Metadata", exact=True)).to_have_attribute(
            "aria-invalid", "true"
        )
        audit("invalid-json")
        dialog.get_by_label("Metadata", exact=True).fill('{"tag":"original"}')
        latest = req(
            "PATCH",
            "/api/records/company/" + company["id"],
            {
                "version": company["version"],
                "company": {
                    "city": "Remote city",
                    "industry": "Infrastructure",
                    "remote_note": "Keep this update",
                },
            },
        )
        dialog.get_by_role("button", name="Refresh record", exact=True).click()
        expect(
            dialog.get_by_text("A newer shared version is available.", exact=False)
        ).to_be_visible()
        expect(dialog.get_by_label("City", exact=True)).to_have_value("Mine city")
        expect(dialog.get_by_label("Capacity", exact=True)).to_have_value("0")
        log("refetch_keeps_the_typed_draft")
        dialog.get_by_role("button", name="Save shared record", exact=True).click()
        expect(
            dialog.get_by_role(
                "heading", name="Review changes before saving", exact=True
            )
        ).to_be_visible()
        expect(
            dialog.get_by_role(
                "button", name="Continue with reviewed draft", exact=True
            )
        ).to_be_disabled()
        for theme in ["light", "dark"]:
            page.evaluate("value=>window.OpenGTMTheme.setPreference(value)", theme)
            audit("conflict-" + theme)
            page.screenshot(path=str(out / ("conflict-" + theme + ".png")))
        dialog.get_by_label("Choose value for City", exact=True).select_option("draft")
        dialog.get_by_role(
            "button", name="Continue with reviewed draft", exact=True
        ).click()
        expect(dialog.get_by_label("City", exact=True)).to_have_value("Mine city")
        expect(dialog.get_by_label("Industry", exact=True)).to_have_value(
            "Infrastructure"
        )
        latest = req(
            "PATCH",
            "/api/records/company/" + company["id"],
            {
                "version": latest["version"],
                "company": {"description": "Remote description"},
            },
        )
        pattern = "**/api/records/company/" + company["id"]

        def fail_get(route):
            if route.request.method == "GET":
                route.fulfill(
                    status=503,
                    content_type="application/json",
                    body='{"detail":"Unavailable"}',
                )
            else:
                route.continue_()

        page.route(pattern, fail_get)
        dialog.get_by_role("button", name="Save shared record", exact=True).click()
        expect(
            dialog.get_by_text(
                "The record conflicted, and the current version could not be loaded.",
                exact=False,
            )
        ).to_be_visible()
        expect(dialog.get_by_label("City", exact=True)).to_have_value("Mine city")
        page.unroute(pattern, fail_get)
        dialog.get_by_role("button", name="Review current version", exact=True).click()
        expect(
            dialog.get_by_role(
                "heading", name="Review changes before saving", exact=True
            )
        ).to_be_visible()
        dialog.get_by_label("Choose value for Description", exact=True).select_option(
            "current"
        )
        dialog.get_by_role(
            "button", name="Continue with reviewed draft", exact=True
        ).click()
        dialog.get_by_role("button", name="Save shared record", exact=True).click()
        expect(dialog).not_to_be_visible()
        saved = req("GET", "/api/records/company/" + company["id"])
        assert (
            saved["fields"]["city"] == "Mine city"
            and saved["fields"]["industry"] == "Infrastructure"
            and saved["fields"]["description"] == "Remote description"
            and saved["fields"]["remote_note"] == "Keep this update"
        )
        assert (
            saved["fields"]["capacity"] == 0
            and saved["fields"]["allow_export"] is False
            and saved["fields"]["routing_rules"] == {"tag": "demo"}
            and saved["fields"]["import_note"] == ""
            and saved["fields"]["large_id"] == 9007199254740993
        )
        log("real_conflicts_and_retry_preserve_remote_and_typed_values")
        page.goto(base + "/companies")
        page.get_by_role("button", name="Atlas Research", exact=True).click()
        dialog = page.get_by_role("dialog", name="Atlas Research", exact=True)
        expect(
            dialog.get_by_role("button", name="Refresh record", exact=True)
        ).to_be_enabled()
        dialog.get_by_role("button", name="Edit record", exact=True).click()
        dialog.get_by_label("City", exact=True).fill("In-flight reviewed edit")
        pending = []

        def hold_patch(route):
            if route.request.method == "PATCH":
                pending.append(route)
            else:
                route.continue_()

        page.route(pattern, hold_patch)
        dialog.get_by_role("button", name="Save shared record", exact=True).click()
        page.wait_for_timeout(100)
        assert len(pending) == 1
        page.evaluate("history.back()")
        held = page.get_by_role("dialog", name="Save in progress", exact=True)
        expect(held).to_be_visible()
        expect(
            held.get_by_role("button", name="Discard changes", exact=True)
        ).to_be_disabled()
        held.get_by_role("button", name="Keep editing", exact=True).click()
        expect(
            dialog.get_by_role("button", name="Save shared record", exact=True)
        ).to_be_disabled()
        with page.expect_response(
            lambda response: response.request.method == "PATCH"
            and response.url.endswith("/records/company/" + company["id"])
        ) as resumed:
            pending[0].continue_()
            page.unroute(pattern, hold_patch)
        assert resumed.value.status == 200
        expect(dialog).not_to_be_visible()
        assert (
            req("GET", "/api/records/company/" + company["id"])["fields"]["city"]
            == "In-flight reviewed edit"
        )
        log("pending_save_cannot_be_discarded_by_back")

        # Enter through a collection action so history Back has an inspector entry.
        page.goto(base + "/companies")
        page.get_by_role("button", name="Atlas Research", exact=True).click()
        dialog = page.get_by_role("dialog", name="Atlas Research", exact=True)
        dialog.get_by_role("button", name="Edit record", exact=True).click()
        dialog.get_by_label("City", exact=True).fill("Unsaved Back draft")
        page.evaluate("history.back()")
        confirm = page.get_by_role("dialog", name="Discard your changes?", exact=True)
        expect(confirm).to_be_visible()
        audit("blocked-back")
        confirm.get_by_role("button", name="Keep editing", exact=True).click()
        expect(dialog.get_by_label("City", exact=True)).to_have_value(
            "Unsaved Back draft"
        )
        assert "record=" in page.url
        native = []

        def dismiss_unload(prompt):
            native.append(prompt.type)
            prompt.dismiss()

        page.once("dialog", dismiss_unload)
        try:
            page.reload(timeout=5000)
        except Exception:
            if not native:
                raise
        assert native == ["beforeunload"], native
        expect(dialog.get_by_label("City", exact=True)).to_have_value(
            "Unsaved Back draft"
        )
        log("back_and_native_reload_keep_drafts")
        dialog.get_by_role("tab", name="People", exact=True).click()
        expect(dialog.get_by_text("56 matching people", exact=True)).to_be_visible()
        expect(dialog.locator(".record-company-people li")).to_have_count(50)
        expect(
            dialog.get_by_role("link", name="Unrelated Person", exact=False)
        ).not_to_be_visible()
        dialog.get_by_role("button", name="Next people", exact=True).click()
        expect(dialog.locator(".record-company-people li")).to_have_count(6)
        dialog.get_by_label("Search people at this company", exact=True).fill(
            "QA Member 055"
        )
        expect(dialog.locator(".record-company-people li")).to_have_count(1)
        audit("company-people")
        dialog.get_by_role("link", name="QA Member 055", exact=False).click()
        expect(confirm).to_be_visible()
        confirm.get_by_role("button", name="Discard changes", exact=True).click()
        expect(
            page.get_by_role("dialog", name="QA Member 055", exact=True)
        ).to_be_visible()
        assert "people?record=" in page.url
        assert (
            req("GET", "/api/records/company/" + company["id"])["fields"]["city"]
            == "In-flight reviewed edit"
        )
        log("company_people_pagination_and_guarded_navigation")
        # Current employment and role-only edits use the real canonical API.
        dialog = open_record("person", ana)
        dialog.get_by_role("button", name="Edit record", exact=True).click()
        dialog.get_by_label("End current employment when saving", exact=True).check()
        expect(dialog.get_by_label("Current role", exact=True)).to_be_disabled()
        dialog.get_by_role("button", name="Save shared record", exact=True).click()
        expect(dialog).not_to_be_visible()
        ended = req("GET", "/api/records/person/" + ana["id"])
        assert (
            ended["id"] == ana["id"]
            and ended["company_id"] is None
            and not any(item["current"] for item in ended["employments"])
        )
        dialog = open_record("person", unknown)
        dialog.get_by_role("button", name="Edit record", exact=True).click()
        dialog.get_by_label("Current role", exact=True).fill("Founder")
        dialog.get_by_role("button", name="Save shared record", exact=True).click()
        expect(dialog).not_to_be_visible()
        standalone = req("GET", "/api/records/person/" + unknown["id"])
        assert standalone["company_id"] is None and any(
            item["current"] and item["fields"]["title"] == "Founder"
            for item in standalone["employments"]
        )
        dialog = open_record("person", ana)
        dialog.get_by_role("button", name="Edit record", exact=True).click()
        dialog.get_by_label("Current company", exact=True).select_option(other["id"])
        dialog.get_by_label("Current role", exact=True).fill("VP")
        dialog.get_by_role("button", name="Save shared record", exact=True).click()
        expect(dialog).not_to_be_visible()
        moved = req("GET", "/api/records/person/" + ana["id"])
        assert (
            moved["id"] == ana["id"]
            and moved["company_id"] == other["id"]
            and len(moved["employments"]) == 2
        )
        log("explicit_employment_end_unknown_role_and_employer_change")
        dialog = open_record("company", company)
        dialog.get_by_role("tab", name="People", exact=True).click()
        expect(dialog.get_by_text("55 matching people", exact=True)).to_be_visible()
        people_pattern = "**/api/records/person?company_id=" + company["id"] + "*"

        def unavailable(route):
            route.fulfill(
                status=503,
                content_type="application/json",
                body='{"detail":"Unavailable"}',
            )

        page.route(people_pattern, unavailable)
        page.reload()
        dialog = page.get_by_role("dialog", name="Atlas Research", exact=True)
        dialog.get_by_role("tab", name="People", exact=True).click()
        expect(
            dialog.get_by_role(
                "heading", name="This view is temporarily unavailable", exact=True
            )
        ).to_be_visible()
        expect(
            dialog.get_by_text(
                "No people are currently linked to this company.", exact=True
            )
        ).not_to_be_visible()
        audit("people-error")
        page.unroute(people_pattern, unavailable)
        dialog.get_by_role("button", name="Try again", exact=True).click()
        expect(dialog.get_by_text("55 matching people", exact=True)).to_be_visible()
        history_pattern = "**/api/records/company/" + company["id"] + "/history"
        page.route(history_pattern, unavailable)
        dialog.get_by_role("tab", name="Sources", exact=True).click()
        expect(
            dialog.get_by_role(
                "heading", name="This view is temporarily unavailable", exact=True
            )
        ).to_be_visible()
        expect(
            dialog.get_by_text("No field observations are available yet.", exact=True)
        ).not_to_be_visible()
        audit("sources-error")
        page.unroute(history_pattern, unavailable)
        dialog.get_by_role("button", name="Try again", exact=True).click()
        expect(dialog.locator(".record-observation").first).to_be_visible()
        dialog.get_by_role("tab", name="People", exact=True).click()
        log("people_and_sources_failures_are_not_empty_success")
        for width in [320, 390]:
            page.set_viewport_size({"width": width, "height": 844})
            assert page.evaluate("document.documentElement.scrollWidth<=innerWidth")
            audit("people-" + str(width))
            page.screenshot(path=str(out / ("people-" + str(width) + ".png")))
        dialog.get_by_role("tab", name="Overview", exact=True).click()
        dialog.get_by_role("button", name="Edit record", exact=True).click()
        for width in [320, 390]:
            page.set_viewport_size({"width": width, "height": 844})
            audit("typed-form-" + str(width))
            page.screenshot(path=str(out / ("typed-form-" + str(width) + ".png")))
        expect(
            dialog.get_by_role("button", name="Save shared record", exact=True)
        ).to_be_disabled()
        dialog.get_by_label("Custom field name", exact=True).fill("Pending field")
        dialog.get_by_label("Custom field type", exact=True).select_option("json")
        dialog.get_by_role("tab", name="Sources", exact=True).click()
        dialog.get_by_role("tab", name="Overview", exact=True).click()
        expect(dialog.get_by_label("Custom field name", exact=True)).to_have_value(
            "Pending field"
        )
        expect(dialog.get_by_label("Custom field type", exact=True)).to_have_value(
            "json"
        )
        dialog.get_by_role("button", name="Save shared record", exact=True).click()
        expect(
            dialog.get_by_text(
                "Add or clear the custom field name before saving.", exact=False
            )
        ).to_be_visible()
        dialog.get_by_role("button", name="Cancel", exact=True).click()
        staged = page.get_by_role("dialog", name="Discard your changes?", exact=True)
        expect(staged).to_be_visible()
        staged.get_by_role("button", name="Keep editing", exact=True).click()
        dialog.get_by_label("Custom field name", exact=True).fill("")
        expect(
            dialog.get_by_role("button", name="Save shared record", exact=True)
        ).to_be_disabled()
        log("staged_custom_field_survives_tabs_and_close_guard")
        dialog.get_by_role("button", name="Cancel", exact=True).click()

        def view_only(route):
            route.fulfill(
                status=200, content_type="application/json", body='{"can_write":false}'
            )

        page.route("**/api/records/access", view_only)
        page.goto(base + "/companies?record=" + company["id"])
        dialog = page.get_by_role("dialog", name="Atlas Research", exact=True)
        expect(
            dialog.get_by_role("button", name="Edit record", exact=True)
        ).to_be_disabled()
        expect(dialog.get_by_text("View only.", exact=False)).to_be_visible()
        audit("view-only")
        page.unroute("**/api/records/access", view_only)
        log("view_only_inspector_explains_role")
        report["page_errors"] = errors
        assert not errors, errors
        browser.close()
finally:
    report["axe"] = scans
    report["page_errors"] = errors
    (out / "results.json").write_text(json.dumps(report, indent=2))
    headers.pop("X-Workspace-Id", None)
    if prior:
        req("POST", "/api/workspaces/switch", {"workspace_id": prior})
