from playwright.sync_api import sync_playwright, expect
from pathlib import Path
import argparse, json, os, urllib.request, urllib.parse, time

parser = argparse.ArgumentParser(
    description="Verify reviewed token rotation, independent history recovery, copy failures and explicit advertising consent. Uses native synthetic fixtures; token writes and external sends are intercepted."
)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument("--output", default=".run/dashboard-activation-trust")
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
ws = req(
    "POST", "/api/workspaces", {"name": "Activation trust " + str(int(time.time()))}
)["id"]
errors = []
scans = []
try:
    req("POST", "/api/workspaces/switch", {"workspace_id": ws})
    headers["X-Workspace-Id"] = ws
    company = req(
        "POST",
        "/api/records/company",
        {"company": {"name": "Activation fixture", "domain": "trust-activation.test"}},
    )
    audience = req(
        "POST",
        "/api/records/company/audience",
        {"ids": [company["id"]], "name": "Review audience"},
    )["audience_id"]
    crm = req(
        "POST",
        "/api/audience-destinations",
        {
            "audience_id": audience,
            "name": "QA CRM",
            "destination_type": "hubspot",
            "config": {"inbound_conflict_policy": "fill_missing"},
        },
    )
    report.update(workspace_id=ws, audience_id=audience)
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
        mode = {"history": "fail", "copy": "fail", "rotations": 0, "creates": []}

        def rotate(route):
            mode["rotations"] += 1
            route.fulfill(
                json={"token": "synthetic-review-token-never-valid-on-server"}
            )

        def history(route):
            if mode["history"] == "fail":
                route.fulfill(
                    status=503, json={"detail": "Synthetic history unavailable"}
                )
            elif mode["history"] == "malformed":
                route.fulfill(json={"receipts": None})
            else:
                route.fulfill(json={"receipts": [], "next_cursor": None})

        page.route("**/api/audience-destinations/*/inbound-token", rotate)
        page.route("**/api/audience-destinations/*/inbound-receipts?*", history)
        page.add_init_script(
            'Object.defineProperty(navigator,"clipboard",{value:{writeText:async(value)=>{if(!window.qaAllowCopy)throw new Error("Synthetic clipboard denial");window.qaCopiedToken=value}},configurable:true})'
        )

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
            assert not result["violations"], name

        page.goto(base + "/audiences?audience=" + audience)
        page.get_by_role("button", name="Rotate token", exact=True).click()
        dialog = page.get_by_role("dialog", name="Rotate inbound token?", exact=True)
        expect(dialog).to_be_visible()
        assert mode["rotations"] == 0
        audit("rotation-review")
        dialog.get_by_role("button", name="Cancel", exact=True).click()
        expect(dialog).not_to_be_visible()
        assert mode["rotations"] == 0
        log("rotation_review_cancel_sends_no_mutation")
        page.get_by_role("button", name="Rotate token", exact=True).click()
        page.get_by_role(
            "dialog", name="Rotate inbound token?", exact=True
        ).get_by_role("button", name="Rotate token", exact=True).click()
        panel = page.get_by_role("dialog", name="Inbound callbacks", exact=True)
        expect(
            panel.get_by_text(
                "synthetic-review-token-never-valid-on-server", exact=True
            )
        ).to_be_visible()
        expect(panel.get_by_role("alert")).to_contain_text(
            "Your new token is still available"
        )
        expect(
            panel.get_by_text("No inbound callbacks received yet.", exact=True)
        ).to_have_count(0)
        assert mode["rotations"] == 1
        audit("token-preserved-after-history-error")
        page.screenshot(path=str(out / "token-history-error.png"))
        log("rotated_token_is_visible_independently_of_history_failure")
        panel.get_by_role("button", name="Copy token", exact=True).click()
        expect(
            panel.get_by_text("The token could not be copied.", exact=False)
        ).to_be_visible()
        expect(panel.get_by_text("Token copied.", exact=True)).to_have_count(0)
        audit("copy-failure")
        panel.get_by_role("button", name="Close setup", exact=True).click()
        guard = page.get_by_role("dialog", name="Close token setup?", exact=True)
        expect(guard).to_contain_text("has not been copied")
        guard.get_by_role("button", name="Keep editing", exact=True).click()
        expect(
            panel.get_by_text(
                "synthetic-review-token-never-valid-on-server", exact=True
            )
        ).to_be_visible()
        log("failed_clipboard_copy_keeps_token_and_close_guard")
        mode["history"] = "malformed"
        with page.expect_response(
            lambda response: "/inbound-receipts?" in response.url
            and response.status == 200
        ):
            panel.get_by_role("button", name="Try history again", exact=True).click()
        expect(
            panel.get_by_role("button", name="Try history again", exact=True)
        ).to_be_enabled()
        expect(
            panel.get_by_text("No inbound callbacks received yet.", exact=True)
        ).to_have_count(0)
        mode["history"] = "ok"
        panel.get_by_role("button", name="Try history again", exact=True).click()
        expect(
            panel.get_by_text("No inbound callbacks received yet.", exact=True)
        ).to_be_visible()
        expect(
            panel.get_by_text(
                "synthetic-review-token-never-valid-on-server", exact=True
            )
        ).to_be_visible()
        log("history_retry_never_rotates_or_drops_the_token")
        page.evaluate("window.qaAllowCopy=true")
        panel.get_by_role("button", name="Copy token", exact=True).click()
        expect(panel.get_by_text("Token copied.", exact=True)).to_be_visible()
        assert (
            page.evaluate("window.qaCopiedToken")
            == "synthetic-review-token-never-valid-on-server"
        )
        panel.get_by_role("button", name="Close setup", exact=True).click()
        expect(panel).not_to_be_visible()
        log("successful_copy_acknowledges_token_and_allows_close")
        # A second reviewed token also works at a narrow viewport and in dark mode.
        page.set_viewport_size({"width": 390, "height": 844})
        page.evaluate('window.OpenGTMTheme.setPreference("dark")')
        page.get_by_role("button", name="Rotate token", exact=True).click()
        audit("mobile-dark-rotation-review")
        page.get_by_role(
            "dialog", name="Rotate inbound token?", exact=True
        ).get_by_role("button", name="Rotate token", exact=True).click()
        panel = page.get_by_role("dialog", name="Inbound callbacks", exact=True)
        expect(
            panel.get_by_text("New inbound token · shown once", exact=True)
        ).to_be_visible()
        audit("mobile-dark-token-setup")
        assert page.evaluate("document.documentElement.scrollWidth===innerWidth")
        panel.get_by_role("button", name="I stored this token", exact=True).click()
        expect(panel.get_by_text("Token marked as stored.", exact=True)).to_be_visible()
        panel.get_by_role("button", name="Close setup", exact=True).click()
        expect(panel).not_to_be_visible()
        log("manual_token_storage_acknowledgement_and_mobile_layout")
        page.set_viewport_size({"width": 1440, "height": 1000})
        page.evaluate('window.OpenGTMTheme.setPreference("light")')

        def create(route):
            mode["creates"].append(route.request.post_data_json)
            route.fulfill(status=503, json={"detail": "Synthetic create failure"})

        page.route(
            "**/api/audience-destinations",
            lambda route: create(route)
            if route.request.method == "POST"
            else route.fallback(),
        )
        page.get_by_role("button", name="Add destination", exact=True).click()
        setup = page.get_by_role("dialog", name="Configure destination", exact=True)
        setup.get_by_label("Destination type", exact=True).select_option("meta_ads")
        setup.get_by_label("Destination name", exact=True).fill("Reviewed advertising")
        setup.get_by_label("Custom audience ID", exact=True).fill("12345")
        setup.get_by_label("Consent source / policy", exact=True).fill("Synthetic consent ledger")
        setup.get_by_role("button", name="Review destination", exact=True).click()
        review = page.get_by_role("dialog", name="Review new destination", exact=True)
        add = review.get_by_role("button", name="Create destination", exact=True)
        expect(add).to_be_disabled()
        automatic = review.get_by_role("checkbox", name="I understand future audience membership changes can automatically send records to this target.", exact=True)
        consent = review.get_by_role("checkbox", name="I confirm this audience has permission for advertising use.", exact=True)
        expect(consent).not_to_be_checked()
        automatic.check()
        expect(add).to_be_disabled()
        consent.check()
        expect(add).to_be_enabled()
        audit("explicit-advertising-consent")
        review.get_by_role("button", name="Edit configuration", exact=True).click()
        setup.get_by_label("Destination type", exact=True).select_option("linkedin_ads")
        setup.get_by_label("Segment ID", exact=True).fill("67890")
        setup.get_by_label("Consent source / policy", exact=True).fill("Other synthetic ledger")
        setup.get_by_role("button", name="Review destination", exact=True).click()
        expect(consent).not_to_be_checked()
        expect(automatic).not_to_be_checked()
        expect(add).to_be_disabled()
        review.get_by_role("button", name="Edit configuration", exact=True).click()
        setup.get_by_label("Destination type", exact=True).select_option("meta_ads")
        expect(setup.get_by_label("Custom audience ID", exact=True)).to_have_value("12345")
        setup.get_by_role("button", name="Review destination", exact=True).click()
        expect(consent).not_to_be_checked()
        automatic.check()
        consent.check()
        add.click()
        expect(review.get_by_role("alert")).to_contain_text("may already be saved")
        assert len(mode["creates"]) == 1 and mode["creates"][0]["config"]["consent_attested"] is True
        log("advertising_consent_is_explicit_and_scoped_to_review_and_target")
        assert not errors, errors
        report["page_errors"] = errors
        browser.close()
finally:
    (out / "results.json").write_text(json.dumps(report, indent=2) + "\n")
    if prior:
        req("POST", "/api/workspaces/switch", {"workspace_id": prior})
