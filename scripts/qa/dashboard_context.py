"""Verify workspace isolation, shared draft guards, and provider failures with synthetic fixtures.
Provider writes/tests are intercepted: no real credentials or provider calls are changed.
"""

from pathlib import Path
import argparse, json, os, time, urllib.request, urllib.parse
from playwright.sync_api import sync_playwright, expect

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--base-url", default="http://127.0.0.1:8003")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument("--output", default=".run/dashboard-context")
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
base = args.base_url.rstrip("/")
headers = {}
report = {"axe": [], "page_errors": []}


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
            method=method,
            headers={
                **headers,
                "Content-Type": "application/x-www-form-urlencoded"
                if form
                else "application/json",
            },
        ),
        timeout=30,
    ) as response:
        return json.load(response)


def save():
    (out / "results.json").write_text(json.dumps(report, indent=2) + "\n")


def log(name):
    report[name] = "pass"
    save()
    print(name, "pass", flush=True)


username = os.environ.get("OPENGTM_QA_USERNAME")
password = os.environ.get("OPENGTM_QA_PASSWORD")
if not username or not password:
    parser.error("Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD")
headers["Authorization"] = (
    "Bearer "
    + req("POST", "/auth/token", {"username": username, "password": password}, True)[
        "access_token"
    ]
)
prior = req("GET", "/api/workspaces")["active_id"]
tag = str(int(time.time()))
spaces = [
    req("POST", "/api/workspaces", {"name": "Context " + name + " " + tag})
    for name in ["Alpha", "Bravo"]
]
a, b = spaces
try:
    for space, name in zip(spaces, ["Alpha only", "Bravo only"]):
        headers["X-Workspace-Id"] = space["id"]
        req(
            "POST",
            "/api/records/company",
            {
                "company": {
                    "name": name,
                    "domain": name.split()[0].lower() + ".context.test",
                }
            },
        )
    req("POST", "/api/workspaces/switch", {"workspace_id": a["id"]})
    report["workspace_ids"] = [a["id"], b["id"]]
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True, executable_path=args.browser_path)
        context = browser.new_context(
            viewport={"width": 1440, "height": 1000}, reduced_motion="reduce"
        )
        context.add_init_script(
            'if(!localStorage.getItem("yupcha_token"))localStorage.setItem("yupcha_token",'
            + json.dumps(headers["Authorization"][7:])
            + ');if(!localStorage.getItem("yupcha_workspace_id"))localStorage.setItem("yupcha_workspace_id",'
            + json.dumps(a["id"])
            + ');localStorage.setItem("theme","light")'
        )
        page = context.new_page()
        page.on("pageerror", lambda e: report["page_errors"].append(str(e)))
        fixture = {
            "providers": [
                {
                    "id": "context-fixture",
                    "name": "Context fixture",
                    "model": "initial-model",
                    "configured": False,
                    "api_key_masked": "",
                    "default_model": "initial-model",
                    "docs_url": "https://example.test/docs",
                    "free_tier": "Synthetic QA fixture",
                    "is_default": True,
                    "openai_compatible": True,
                    "icon": "",
                    "base_url": "",
                }
            ],
            "default_provider": "context-fixture",
        }
        provider_mode = {"read": "ok", "write": "fail", "held": []}

        def providers(route):
            if route.request.method == "GET":
                if provider_mode["read"] == "fail":
                    route.fulfill(status=503, json={"detail": "Synthetic unavailable"})
                elif provider_mode["read"] == "malformed":
                    route.fulfill(json={"providers": None})
                else:
                    route.fulfill(json=fixture)
            elif provider_mode["write"] == "hold":
                provider_mode["held"].append(route)
            elif provider_mode["write"] == "ok":
                fixture["providers"][0]["model"] = route.request.post_data_json.get(
                    "model", fixture["providers"][0]["model"]
                )
                provider_mode["read"] = "fail"
                route.fulfill(json={"status": "ok", "provider": "context-fixture"})
            else:
                route.fulfill(status=503, json={"detail": "Synthetic save failure"})

        page.route("**/api/settings/providers", providers)
        page.route("**/api/settings/providers/context-fixture", providers)
        switches = []
        page.on(
            "request",
            lambda request: switches.append(request.post_data_json)
            if request.method == "POST"
            and request.url.endswith("/api/workspaces/switch")
            else None,
        )

        def audit(name):
            page.add_script_tag(
                path=str(root / "apps/web/node_modules/axe-core/axe.min.js")
            )
            result = page.evaluate("async()=>await axe.run(document)")
            report["axe"].append({"name": name, "violations": result["violations"]})
            save()
            assert not result["violations"], name

        def settings():
            page.goto(base + "/settings")
            expect(page.get_by_label("Model", exact=True)).to_be_visible()

        def switch(space):
            page.get_by_title("Switch workspace", exact=True).click()
            page.get_by_role("menuitem", name=space["name"]).click()

        def account_signout():
            page.get_by_title("Account menu", exact=True).click()
            page.get_by_role("menuitem", name="Sign out", exact=True).click()

        def keep(name):
            dialog = page.get_by_role("dialog", name=name, exact=True)
            expect(dialog).to_be_visible()
            dialog.get_by_role("button", name="Keep editing", exact=True).click()

        settings()
        model = page.get_by_label("Model", exact=True)
        model.fill("retained model draft")
        page.get_by_role("tab", name="Email", exact=True).click()
        audit("settings-section-guard")
        keep("Change settings section?")
        expect(model).to_have_value("retained model draft")
        page.get_by_role("link", name="Companies", exact=True).click()
        keep("Discard your changes?")
        expect(model).to_have_value("retained model draft")
        switch(b)
        audit("workspace-guard")
        keep("Switch workspaces?")
        assert not switches
        expect(model).to_have_value("retained model draft")
        log("section_route_and_workspace_cancel_keep_drafts_without_switch_requests")
        page.get_by_role("tab", name="Email", exact=True).click()
        page.get_by_role(
            "dialog", name="Change settings section?", exact=True
        ).get_by_role("button", name="Discard changes", exact=True).click()
        expect(page.get_by_label("Model", exact=True)).to_have_count(0)
        page.get_by_role("tab", name="AI Providers", exact=True).click()
        expect(model).to_have_value("initial-model")
        model.fill("retained model draft")
        log("explicit_section_discard_releases_the_old_form")

        page.get_by_role("tabpanel", name="AI Providers", exact=True).get_by_role(
            "button", name="Save", exact=True
        ).click()
        expect(page.get_by_role("alert")).to_contain_text("Save outcome unknown (503)")
        expect(model).to_have_value("retained model draft")
        audit("provider-save-error")
        log("provider_http_failure_keeps_draft_and_has_no_false_success")
        provider_mode["write"] = "hold"
        page.get_by_role("tabpanel", name="AI Providers", exact=True).get_by_role(
            "button", name="Save", exact=True
        ).click()
        page.wait_for_timeout(100)
        assert provider_mode["held"]
        switch(b)
        dialog = page.get_by_role("dialog", name="Save in progress", exact=True)
        expect(dialog).to_be_visible()
        expect(
            dialog.get_by_role("button", name="Discard changes", exact=True)
        ).to_be_disabled()
        audit("pending-provider-save")
        keep("Save in progress")
        assert not switches
        provider_mode["held"].pop().fulfill(
            status=503, json={"detail": "Synthetic save failure"}
        )
        expect(page.get_by_role("alert")).to_contain_text("Save outcome unknown (503)")
        log("pending_save_cannot_be_abandoned_by_workspace_change")
        provider_mode["write"] = "ok"
        page.get_by_role("tabpanel", name="AI Providers", exact=True).get_by_role(
            "button", name="Save", exact=True
        ).click()
        expect(
            page.get_by_role("tabpanel", name="AI Providers", exact=True).get_by_role(
                "button", name="Save", exact=True
            )
        ).to_be_disabled()
        expect(
            page.get_by_text(
                "The latest provider settings could not be loaded.", exact=False
            )
        ).to_be_visible()
        expect(model).to_have_value("retained model draft")
        page.get_by_role("link", name="Companies", exact=True).click()
        page.wait_for_url("**/companies")
        log("confirmed_provider_save_does_not_depend_on_followup_read")
        provider_mode["read"] = "ok"
        settings()
        model = page.get_by_label("Model", exact=True)
        model.fill("retained model draft again")

        # Another open tab starts in Alpha. Switching the first tab to Bravo must not
        # replace this tab's header scope or its displayed workspace.
        other = context.new_page()
        other.on("pageerror", lambda e: report["page_errors"].append(str(e)))
        other.goto(base + "/companies")
        expect(
            other.get_by_role("button", name="Alpha only", exact=True)
        ).to_be_visible()
        switch(b)
        dialog = page.get_by_role("dialog", name="Switch workspaces?", exact=True)
        dialog.get_by_role("button", name="Discard changes", exact=True).click()
        page.wait_for_url("**/chat")
        assert len(switches) == 1
        page.goto(base + "/companies")
        expect(
            page.get_by_role("button", name="Bravo only", exact=True)
        ).to_be_visible()
        expect(page.get_by_role("button", name="Alpha only", exact=True)).to_have_count(
            0
        )
        headers_seen = []
        other.on(
            "request",
            lambda request: headers_seen.append(request.headers.get("x-workspace-id"))
            if "/api/records/company?" in request.url
            else None,
        )
        payload = other.evaluate(
            'async()=>await (await fetch("/api/records/company?limit=20&offset=0")).json()'
        )
        assert [r["fields"]["name"] for r in payload["records"]] == ["Alpha only"]
        assert headers_seen[-1] == a["id"]
        expect(
            other.get_by_role("button", name="Alpha only", exact=True)
        ).to_be_visible()
        log("cross_tab_workspace_switch_does_not_redirect_existing_tab_requests")
        page.route(
            "**/api/workspaces/switch",
            lambda route: route.fulfill(
                status=403, json={"detail": "Synthetic workspace access denied"}
            ),
        )
        switch(a)
        expect(
            page.get_by_text("Synthetic workspace access denied", exact=True)
        ).to_be_visible()
        expect(
            page.get_by_role("button", name="Bravo only", exact=True)
        ).to_be_visible()
        assert page.evaluate('localStorage.getItem("yupcha_workspace_id")') == b["id"]
        log("denied_switch_keeps_current_workspace_and_data")
        page.unroute("**/api/workspaces/switch")
        page.goto(base + "/agency")
        row = page.locator(".rounded-lg.border").filter(
            has=page.get_by_text(a["name"], exact=True)
        )
        row.get_by_role("button", name="Switch", exact=True).click()
        expect(page.get_by_title("Switch workspace", exact=True)).to_contain_text(
            a["name"]
        )
        page.goto(base + "/companies")
        expect(
            page.get_by_role("button", name="Alpha only", exact=True)
        ).to_be_visible()
        log("workspace_manager_uses_auth_context_and_clears_previous_data")
        # Read errors/malformed successes remain recoverable rather than empty lists.
        provider_mode["read"] = "fail"
        page.goto(base + "/settings")
        expect(
            page.get_by_role("heading", name="This view is temporarily unavailable")
        ).to_be_visible()
        audit("providers-unavailable")
        provider_mode["read"] = "malformed"
        page.get_by_role("button", name="Try again", exact=True).click()
        expect(
            page.get_by_text(
                "Provider settings could not be verified. Try again.", exact=True
            )
        ).to_be_visible()
        provider_mode["read"] = "ok"
        page.get_by_role("button", name="Try again", exact=True).click()
        expect(page.get_by_label("Model", exact=True)).to_be_visible()
        log("provider_read_error_and_malformed_response_retry")
        page.get_by_role("tab", name="Agents", exact=True).click()
        expect(
            page.get_by_role("heading", name="Agent workflows", exact=True)
        ).to_be_visible()
        expect(
            page.get_by_role("link", name="Workbook enrichment", exact=False)
        ).to_have_attribute("href", "/workbooks")
        expect(page.locator('[id^="agent-"]')).to_have_count(0)
        audit("honest-agent-workflows")
        page.get_by_role("tab", name="AI Providers", exact=True).click()
        auth_user = req("GET", "/auth/me")
        auth_user["is_admin"] = False
        page.route("**/auth/me", lambda route: route.fulfill(json=auth_user))
        page.reload()
        expect(
            page.get_by_text(
                "View only. An instance admin manages provider settings.", exact=True
            )
        ).to_be_visible()
        expect(page.get_by_label("Model", exact=True)).to_be_disabled()
        expect(
            page.get_by_role("tabpanel", name="AI Providers", exact=True).get_by_role(
                "button", name="Save", exact=True
            )
        ).to_be_disabled()
        audit("instance-provider-view-only")
        log("instance_permissions_and_agent_workflows_are_honest")
        page.unroute("**/auth/me")
        page.reload()
        expect(page.get_by_label("Model", exact=True)).to_be_enabled()

        model = page.get_by_label("Model", exact=True)
        model.fill("signout draft")
        account_signout()
        keep("Sign out?")
        expect(model).to_have_value("signout draft")
        page.set_viewport_size({"width": 390, "height": 844})
        page.get_by_role("tab", name="Email", exact=True).click()
        audit("mobile-settings-guard")
        page.screenshot(path=str(out / "mobile-guard.png"))
        keep("Change settings section?")
        page.set_viewport_size({"width": 1440, "height": 1000})
        account_signout()
        page.get_by_role("dialog", name="Sign out?", exact=True).get_by_role(
            "button", name="Discard changes", exact=True
        ).click()
        page.wait_for_url("**/login")
        other.wait_for_url("**/login")
        assert not page.evaluate('localStorage.getItem("yupcha_token")')
        expect(
            other.get_by_role("button", name="Alpha only", exact=True)
        ).to_have_count(0)
        log("explicit_signout_is_guarded_and_other_tabs_clear_private_data")

        expired = context.browser.new_context(
            viewport={"width": 1440, "height": 1000}, reduced_motion="reduce"
        )
        expired.add_init_script(
            'localStorage.setItem("yupcha_token",'
            + json.dumps(headers["Authorization"][7:])
            + ');localStorage.setItem("yupcha_workspace_id",'
            + json.dumps(a["id"])
            + ")"
        )
        revoked = expired.new_page()
        revoked.on("pageerror", lambda error: report["page_errors"].append(str(error)))
        revoked.route(
            "**/api/settings/providers", lambda route: route.fulfill(json=fixture)
        )
        revoked.route(
            "**/api/qa-session-probe",
            lambda route: route.fulfill(
                status=401, json={"detail": "Synthetic expired session"}
            ),
        )
        revoked.goto(base + "/settings")
        revoked.get_by_label("Model", exact=True).fill("expired draft")
        revoked.get_by_title("Account menu", exact=True).click()
        revoked.get_by_role("menuitem", name="Sign out", exact=True).click()
        expect(
            revoked.get_by_role("dialog", name="Sign out?", exact=True)
        ).to_be_visible()
        revoked.evaluate('async()=>await fetch("/api/qa-session-probe")')
        revoked.wait_for_url("**/login")
        expect(revoked.get_by_role("dialog")).to_have_count(0)
        expect(revoked.get_by_label("Model", exact=True)).to_have_count(0)
        assert not revoked.evaluate('localStorage.getItem("yupcha_token")')
        log("revoked_session_cancels_pending_actions_and_clears_private_forms")
        expired.close()
        assert not report["page_errors"], report["page_errors"]
        browser.close()
finally:
    save()
    if prior:
        req("POST", "/api/workspaces/switch", {"workspace_id": prior})
