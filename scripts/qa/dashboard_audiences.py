from playwright.sync_api import sync_playwright, expect
from pathlib import Path
import argparse, json, os, urllib.request, urllib.parse, time

parser = argparse.ArgumentParser(
    description="Verify canonical audience identities, refresh receipts and recoverable errors using isolated synthetic records."
)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument("--output", default=".run/dashboard-audiences")
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
ws = req("POST", "/api/workspaces", {"name": "Activation UI " + str(int(time.time()))})[
    "id"
]
try:
    req("POST", "/api/workspaces/switch", {"workspace_id": ws})
    headers["X-Workspace-Id"] = ws
    c = req(
        "POST",
        "/api/records/company",
        {"company": {"name": "Atlas Research", "domain": "activation-atlas.test"}},
    )
    p1 = req(
        "POST",
        "/api/records/person",
        {
            "person": {"full_name": "Ana Rivera", "email": "ana@activation-atlas.test"},
            "company_id": c["id"],
            "employment": {"title": "Founder"},
        },
    )
    p2 = req(
        "POST",
        "/api/records/person",
        {
            "person": {"full_name": "Bo Chen", "email": "bo@activation-atlas.test"},
            "company_id": c["id"],
            "employment": {"title": "Engineer"},
        },
    )
    ca = req(
        "POST",
        "/api/records/company/audience",
        {"ids": [c["id"]], "name": "Company research"},
    )
    pa = req(
        "POST",
        "/api/records/person/audience",
        {"ids": [p1["id"], p2["id"]], "name": "People ready for review"},
    )
    for audience in [ca, pa]:
        req("POST", "/api/audiences/" + audience["audience_id"] + "/refresh", {})
    report["workspace_id"] = ws
    report["company_audience"] = ca["audience_id"]
    report["people_audience"] = pa["audience_id"]
    report["company_id"] = c["id"]
    report["person_id"] = p1["id"]
    errors = []
    scans = []
    with sync_playwright() as playwright:
        browser = playwright.chromium.launch(
            headless=True, executable_path=args.browser_path
        )
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
        page.on("pageerror", lambda e: errors.append(str(e)))
        for scope, audience, identity, name in [
            ("company", ca, c["id"], "Atlas Research"),
            ("person", pa, p1["id"], "Ana Rivera"),
        ]:
            page.goto(base + "/audiences?audience=" + audience["audience_id"])
            link = page.get_by_role("link", name=name, exact=False).first
            expect(link).to_be_visible()
            expected = (
                ("/companies" if scope == "company" else "/people")
                + "?record="
                + identity
            )
            expect(link).to_have_attribute("href", expected)
            assert page.locator('a[href="/leads/null"],a[href="/leads/0"]').count() == 0
            page.add_script_tag(
                path=str(root / "apps/web/node_modules/axe-core/axe.min.js")
            )
            a = page.evaluate("async()=>await axe.run(document)")
            scans.append({"state": scope, "violations": a["violations"]})
            page.screenshot(path=str(out / (scope + ".png")))
            link.click()
            expect(
                page.get_by_role("dialog").get_by_role("heading", name=name, exact=True)
            ).to_be_visible()
            assert "?record=" in page.url
            page.reload()
            expect(
                page.get_by_role("dialog").get_by_role("heading", name=name, exact=True)
            ).to_be_visible()
            page.get_by_role("dialog").get_by_role(
                "button", name="Close record", exact=True
            ).click()
            assert "record=" not in page.url
        report["canonical_identity_and_shareable_inspectors"] = "pass"
        page.goto(base + "/audiences?audience=" + pa["audience_id"])
        with page.expect_response(
            lambda response: response.request.method == "POST"
            and response.url.endswith("/refresh")
        ) as refresh:
            page.get_by_role("button", name="Refresh now", exact=True).click()
            page.get_by_role(
                "dialog", name="Review audience refresh", exact=True
            ).get_by_role("button", name="Confirm refresh", exact=True).click()
        counts = refresh.value.json()
        expect(page.get_by_text("Membership refreshed", exact=True)).to_be_visible()
        for key in ["entered", "exited", "changed", "unchanged"]:
            expect(page.locator(".audiences-receipt")).to_contain_text(
                str(counts[key]) + " " + key
            )
        report["refresh_receipt"] = {"status": "pass", "counts": counts}
        for width in [320, 390, 768]:
            page.set_viewport_size({"width": width, "height": 844})
            page.goto(base + "/audiences?audience=" + pa["audience_id"])
            expect(
                page.get_by_role("link", name="Ana Rivera", exact=False)
            ).to_be_visible()
            assert page.evaluate("document.documentElement.scrollWidth<=innerWidth"), (
                page.evaluate("document.documentElement.scrollWidth")
            )
            page.add_script_tag(
                path=str(root / "apps/web/node_modules/axe-core/axe.min.js")
            )
            a = page.evaluate("async()=>await axe.run(document)")
            scans.append(
                {"state": "people-" + str(width), "violations": a["violations"]}
            )
            page.screenshot(path=str(out / ("people-" + str(width) + ".png")))

        def failed(route):
            route.fulfill(
                status=503,
                content_type="application/json",
                body='{"detail":"Temporarily unavailable"}',
            )

        page.route("**/api/audiences/*/members?*", failed)
        page.reload()
        expect(
            page.get_by_role(
                "heading", name="This view is temporarily unavailable"
            ).first
        ).to_be_visible(timeout=10000)
        expect(
            page.get_by_text("No records on this page.", exact=False)
        ).not_to_be_visible()
        page.unroute("**/api/audiences/*/members?*", failed)
        page.get_by_role("button", name="Try again", exact=True).first.click()
        expect(page.get_by_role("link", name="Ana Rivera", exact=False)).to_be_visible()
        report["members_error_and_retry"] = "pass"
        report["scans"] = scans
        report["page_errors"] = errors
        (out / "results.json").write_text(json.dumps(report, indent=2))
        print(
            [
                (r["state"], [(v["id"], len(v["nodes"])) for v in r["violations"]])
                for r in scans
            ]
        )
        assert not errors
        assert all(not scan["violations"] for scan in scans)
        browser.close()
finally:
    headers.pop("X-Workspace-Id", None)
    if prior:
        req("POST", "/api/workspaces/switch", {"workspace_id": prior})
