"""Verify destination configuration/review/receipt, draft protection and recovery.

Creates isolated native records and paused audiences. Native creation checks use
disabled webhook destinations; no sync or external delivery is requested.
"""

import argparse
import json
import os
import re
import time
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument("--engine", choices=["chromium", "webkit"], default="chromium")
parser.add_argument("--output", default=".run/dashboard-destination-setup")
parser.add_argument("--reopen-cycles", type=int, default=8)
parser.add_argument(
    "--reopen-only",
    action="store_true",
    help="Run only the fast field-identity/focus regression",
)
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
base = args.base_url.rstrip("/")
headers = {}
report = {"engine": args.engine, "axe": [], "page_errors": []}


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
            "Content-Type": (
                "application/x-www-form-urlencoded" if form else "application/json"
            ),
        },
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def save():
    (out / "results.json").write_text(json.dumps(report, indent=2) + "\n")


def audit(page, name):
    page.add_script_tag(path=str(root / "apps/web/node_modules/axe-core/axe.min.js"))
    result = page.evaluate("async()=>await axe.run(document)")
    report["axe"].append({"name": name, "violations": result["violations"]})
    save()
    assert not result["violations"], name


username, password = os.environ.get("OPENGTM_QA_USERNAME"), os.environ.get(
    "OPENGTM_QA_PASSWORD"
)
if not username or not password:
    parser.error("Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD.")
token = req("POST", "/auth/token", {"username": username, "password": password}, True)[
    "access_token"
]
headers["Authorization"] = "Bearer " + token
prior = req("GET", "/api/workspaces")["active_id"]
ws = req(
    "POST", "/api/workspaces", {"name": "Destination setup QA " + str(int(time.time()))}
)["id"]
try:
    req("POST", "/api/workspaces/switch", {"workspace_id": ws})
    headers["X-Workspace-Id"] = ws
    company = req(
        "POST",
        "/api/records/company",
        {"company": {"name": "Setup company", "domain": "setup-qa.invalid"}},
    )
    audience = req(
        "POST",
        "/api/records/company/audience",
        {"ids": [company["id"]], "name": "Setup audience"},
    )["audience_id"]
    other = req(
        "POST",
        "/api/records/company/audience",
        {"ids": [company["id"]], "name": "Other setup audience"},
    )["audience_id"]
    for aid in [audience, other]:
        req("POST", f"/api/audiences/{aid}/refresh", {})
        req("PATCH", f"/api/audiences/{aid}", {"refresh_enabled": False})
    report.update(workspace_id=ws, audience_id=audience)
    with sync_playwright() as p:
        browser = getattr(p, args.engine).launch(
            headless=True,
            executable_path=args.browser_path if args.engine == "chromium" else None,
        )
        context = browser.new_context(
            viewport={"width": 1440, "height": 1000}, reduced_motion="reduce"
        )
        auth = (
            'localStorage.setItem("yupcha_token",'
            + json.dumps(token)
            + ');localStorage.setItem("yupcha_workspace_id",'
            + json.dumps(ws)
            + ');localStorage.setItem("theme","light");'
        )
        context.add_init_script(auth)
        page = context.new_page()
        page.on("pageerror", lambda error: report["page_errors"].append(str(error)))
        mode = {
            "post": "native",
            "list": "native",
            "permission": True,
            "catalog": "native",
            "held": None,
            "creates": [],
            "saved": [],
        }

        def intercept(route):
            if route.request.method != "POST":
                route.fallback()
                return
            body = route.request.post_data_json
            mode["creates"].append(body)
            # Every native fixture write is deliberately disabled, with the
            # audience schedule also paused independently.
            assert body["enabled"] is False and body["destination_type"] == "webhook"
            if mode["post"] == "hold":
                mode["held"] = route
                return
            if mode["post"] == "422":
                route.fulfill(status=422, json={"detail": "Synthetic config refusal"})
                return
            result = route.fetch()
            assert result.status == 201, result.status
            mode["saved"].append(result.json())
            if mode["post"] == "lost":
                mode["list"] = "fail"
                route.fulfill(
                    status=503, json={"detail": "Synthetic response lost after commit"}
                )
            else:
                if mode["post"] == "receipt-read-failure":
                    mode["list"] = "fail"
                route.fulfill(response=result)

        def list_read(route):
            if mode["list"] == "fail":
                route.fulfill(
                    status=503, json={"detail": "Synthetic destination read failure"}
                )
            else:
                route.fallback()

        def permission(route):
            if mode["permission"]:
                route.fallback()
            else:
                route.fulfill(json={"can_write": False})

        def catalog_read(route):
            if mode["catalog"] == "fail":
                route.fulfill(
                    status=503, json={"detail": "Synthetic catalog read failure"}
                )
            elif mode["catalog"] == "malformed":
                route.fulfill(json={"types": "not a catalog"})
            elif mode["catalog"] == "empty":
                route.fulfill(json={"types": []})
            else:
                route.fallback()

        page.route("**/api/audience-destinations/types", catalog_read)
        page.route("**/api/audience-destinations", intercept)
        page.route("**/api/audience-destinations?*", list_read)
        page.route("**/api/audience-destinations/access", permission)

        def open_setup(name="Setup draft", disabled=True):
            page.get_by_role("button", name="Add destination", exact=True).click()
            dialog = page.get_by_role(
                "dialog", name="Configure destination", exact=True
            )
            expect(dialog.get_by_label("Destination type", exact=True)).to_be_enabled()
            dialog.get_by_label("Destination name", exact=True).fill(name)
            dialog.get_by_label("Delivery availability", exact=True).select_option(
                "disabled" if disabled else "enabled"
            )
            return dialog

        def discard():
            current = page.get_by_role("dialog").first
            current.get_by_role("button", name="Cancel", exact=True).click()
            decision = page.get_by_role(
                "dialog", name="Discard destination setup?", exact=True
            )
            expect(decision).to_be_visible()
            decision.get_by_role("button", name="Discard changes", exact=True).click()
            expect(page.get_by_role("dialog")).to_have_count(0)

        def review(dialog):
            dialog.get_by_role("button", name="Review destination", exact=True).click()
            result = page.get_by_role(
                "dialog", name="Review new destination", exact=True
            )
            expect(result).to_be_visible()
            expect(
                result.get_by_role("button", name="Create destination", exact=True)
            ).to_be_enabled()
            return result

        page.goto(base + "/audiences?audience=" + audience)
        expect(
            page.get_by_role("button", name="Add destination", exact=True)
        ).to_be_enabled()
        for cycle in range(args.reopen_cycles):
            draft_name = "Rapid draft " + str(cycle)
            endpoint = "https://setup-qa.invalid/rapid-" + str(cycle)
            dialog = open_setup(draft_name)
            dialog.get_by_label("Webhook URL", exact=True).fill(endpoint)
            page.evaluate(
                "()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)))"
            )
            page.keyboard.type("-typed")
            expect(dialog.get_by_label("Webhook URL", exact=True)).to_have_value(
                endpoint + "-typed"
            )
            expect(dialog.get_by_label("Destination name", exact=True)).to_have_value(
                draft_name
            )
            review(dialog)
            discard()
        report["rapid_reopen_keeps_field_identity_and_keyboard_focus"] = (
            args.reopen_cycles
        )
        if args.reopen_only:
            browser.close()
            print(
                json.dumps(
                    {
                        "reopen_cycles": args.reopen_cycles,
                        "page_errors": report["page_errors"],
                    }
                )
            )
            raise SystemExit(0)
        dialog = open_setup("")
        dialog.get_by_role("button", name="Review destination", exact=True).click()
        expect(dialog.get_by_label("Destination name", exact=True)).to_be_focused()
        expect(
            dialog.get_by_text(
                "Use a destination name of 1–200 characters.", exact=True
            )
        ).to_be_visible()
        dialog.get_by_label("Destination name", exact=True).fill("Keep this draft")
        dialog.get_by_label("Webhook URL", exact=True).fill(
            "https://user:password@setup-qa.invalid"
        )
        dialog.get_by_role("button", name="Review destination", exact=True).click()
        expect(dialog.get_by_label("Webhook URL", exact=True)).to_have_attribute(
            "aria-invalid", "true"
        )
        assert not mode["creates"]
        dialog.get_by_role("button", name="Cancel", exact=True).click()
        decision = page.get_by_role(
            "dialog", name="Discard destination setup?", exact=True
        )
        decision.get_by_role("button", name="Keep editing", exact=True).click()
        expect(dialog.get_by_label("Destination name", exact=True)).to_have_value(
            "Keep this draft"
        )
        audit(page, "inline-validation-and-retained-draft")
        discard()
        expect(
            page.get_by_role("button", name="Add destination", exact=True)
        ).to_be_focused()
        report["validation_cancel_and_focus_return_send_nothing"] = True

        # Render all destination forms with persistent visible labels/help.
        names = [
            "webhook",
            "warehouse_http",
            "hubspot",
            "salesforce",
            "instantly",
            "smartlead",
            "google_sheets",
            "airtable",
            "slack",
            "meta_ads",
            "google_ads",
            "linkedin_ads",
        ]
        for kind in names:
            dialog = open_setup("Label review " + kind)
            dialog.get_by_label("Destination type", exact=True).select_option(kind)
            for control in dialog.locator(
                "input:not([type=checkbox]),textarea,select"
            ).all():
                control_id = control.get_attribute("id")
                assert control_id and dialog.locator("label").evaluate_all(
                    "(labels,id)=>labels.some(label=>label.htmlFor===id&&label.textContent.trim())",
                    control_id,
                )
            audit(page, "configuration-" + kind)
            if kind in ["webhook", "google_sheets", "hubspot"]:
                dialog.screenshot(path=str(out / ("configure-" + kind + ".png")))
            discard()
        report["all_twelve_destination_forms_have_visible_labels"] = True

        mode["catalog"] = "fail"
        page.reload()
        page.get_by_role("button", name="Add destination", exact=True).click()
        dialog = page.get_by_role("dialog", name="Configure destination", exact=True)
        dialog.get_by_label("Destination name", exact=True).fill("Catalog retry draft")
        expect(
            dialog.get_by_role("button", name="Try catalog again", exact=True)
        ).to_be_visible()
        expect(
            dialog.get_by_role("button", name="Review destination", exact=True)
        ).to_be_disabled()
        audit(page, "catalog-failure-keeps-draft")
        mode["catalog"] = "malformed"
        with page.expect_response(
            lambda response: response.url.endswith("/api/audience-destinations/types")
            and response.status == 200
        ):
            dialog.get_by_role("button", name="Try catalog again", exact=True).click()
        expect(
            dialog.get_by_role("button", name="Try catalog again", exact=True)
        ).to_be_enabled()
        expect(dialog.get_by_label("Destination name", exact=True)).to_have_value(
            "Catalog retry draft"
        )
        mode["catalog"] = "empty"
        dialog.get_by_role("button", name="Try catalog again", exact=True).click()
        expect(
            dialog.get_by_text(
                "No destination types are currently available. Your draft is kept.",
                exact=True,
            )
        ).to_be_visible()
        expect(
            dialog.get_by_role("button", name="Review destination", exact=True)
        ).to_be_disabled()
        audit(page, "empty-catalog-is-not-available-destinations")
        mode["catalog"] = "native"
        dialog.get_by_role("button", name="Refresh catalog", exact=True).click()
        expect(dialog.get_by_label("Destination type", exact=True)).to_be_enabled()
        expect(dialog.get_by_label("Destination name", exact=True)).to_have_value(
            "Catalog retry draft"
        )
        discard()
        if page.get_by_role(
            "button", name="Try integration status again", exact=True
        ).is_visible():
            page.get_by_role(
                "button", name="Try integration status again", exact=True
            ).click()
        report["failed_malformed_and_empty_catalogs_keep_drafts_and_recover"] = True

        # Back between audience URLs must not silently reassign a draft.
        page.locator(".audiences-list-item").filter(
            has_text="Other setup audience"
        ).click()
        page.locator(".audiences-list-item").filter(
            has_text=re.compile("^Setup audience")
        ).click()
        dialog = open_setup("Audience-owned draft")
        page.evaluate("window.history.back()")
        decision = page.get_by_role("dialog", name="Discard your changes?", exact=True)
        expect(decision).to_be_visible()
        decision.get_by_role("button", name="Keep editing", exact=True).click()
        expect(dialog.get_by_label("Destination name", exact=True)).to_have_value(
            "Audience-owned draft"
        )
        page.evaluate("window.history.back()")
        decision.get_by_role("button", name="Discard changes", exact=True).click()
        expect(page).to_have_url(base + "/audiences?audience=" + other)
        expect(page.get_by_role("dialog")).to_have_count(0)
        page.locator(".audiences-list-item").filter(
            has_text=re.compile("^Setup audience")
        ).click()
        report["audience_navigation_keeps_or_explicitly_discards_owned_draft"] = True

        dialog = open_setup("Schedule-reviewed target")
        dialog.get_by_label("Webhook URL", exact=True).fill(
            "https://setup-qa.invalid/target?private=value"
        )
        panel = review(dialog)
        expect(
            panel.get_by_text("https://setup-qa.invalid", exact=False)
        ).to_be_visible()
        assert "private=value" not in panel.inner_text()
        endpoint = panel.get_by_label("Full endpoint URL", exact=True)
        expect(endpoint).to_have_attribute("type", "password")
        panel.get_by_role("button", name="Show full endpoint", exact=True).click()
        expect(endpoint).to_have_attribute("type", "text")
        expect(endpoint).to_have_value("https://setup-qa.invalid/target?private=value")
        panel.get_by_role("button", name="Hide full endpoint", exact=True).click()
        expect(endpoint).to_have_attribute("type", "password")
        req("PATCH", "/api/audiences/" + audience, {"refresh_interval_minutes": 15})
        panel.get_by_role("button", name="Create destination", exact=True).click()
        expect(panel.get_by_role("alert")).to_contain_text("changed")
        assert not mode["creates"]
        panel.get_by_role("button", name="Refresh review", exact=True).click()
        expect(
            panel.get_by_role("button", name="Create destination", exact=True)
        ).to_be_enabled()
        expect(
            panel.get_by_role("heading", name="Review new destination", exact=True)
        ).to_be_focused()
        audit(page, "changed-scope-requires-new-review")
        discard()
        report["scope_and_schedule_changes_block_the_preflight_without_post"] = True

        dialog = open_setup("Permission-reviewed target")
        dialog.get_by_label("Webhook URL", exact=True).fill(
            "https://setup-qa.invalid/permission"
        )
        panel = review(dialog)
        mode["permission"] = False
        panel.get_by_role("button", name="Create destination", exact=True).click()
        expect(panel.get_by_role("alert")).to_contain_text("permission changed")
        assert not mode["creates"]
        discard()
        mode["permission"] = True
        page.reload()
        expect(
            page.get_by_role("button", name="Add destination", exact=True)
        ).to_be_enabled()
        report["permission_change_blocks_creation_before_post"] = True

        # Enabled creation and ad consent require separate, scoped decisions.
        dialog = open_setup("Advertising review", disabled=False)
        dialog.get_by_label("Destination type", exact=True).select_option("meta_ads")
        dialog.get_by_label("Custom audience ID", exact=True).fill("12345")
        dialog.get_by_label("Consent source / policy", exact=True).fill(
            "Synthetic consent ledger"
        )
        dialog.get_by_role("button", name="Review destination", exact=True).click()
        panel = page.get_by_role("dialog", name="Review new destination", exact=True)
        create = panel.get_by_role("button", name="Create destination", exact=True)
        expect(create).to_be_disabled()
        auto = panel.get_by_role(
            "checkbox",
            name="I understand future audience membership changes can automatically send records to this target.",
            exact=True,
        )
        consent = panel.get_by_role(
            "checkbox",
            name="I confirm this audience has permission for advertising use.",
            exact=True,
        )
        auto.check()
        expect(create).to_be_disabled()
        consent.check()
        expect(create).to_be_enabled()
        audit(page, "advertising-and-automatic-activation-review")
        panel.get_by_role("button", name="Edit configuration", exact=True).click()
        dialog = page.get_by_role("dialog", name="Configure destination", exact=True)
        dialog.get_by_label("Custom audience ID", exact=True).fill("67890")
        dialog.get_by_role("button", name="Review destination", exact=True).click()
        expect(auto).not_to_be_checked()
        expect(consent).not_to_be_checked()
        expect(create).to_be_disabled()
        discard()
        assert not mode["creates"]
        report["advertising_and_enablement_acknowledgements_reset_on_review_change"] = (
            True
        )

        mode["post"] = "hold"
        dialog = open_setup("Native paused target")
        dialog.get_by_label("Webhook URL", exact=True).fill(
            "https://setup-qa.invalid/native"
        )
        panel = review(dialog)
        panel.get_by_role("button", name="Create destination", exact=True).click()
        expect(
            panel.get_by_role("button", name="Creating…", exact=True)
        ).to_be_disabled()
        deadline = time.monotonic() + 5
        while mode["held"] is None and time.monotonic() < deadline:
            page.wait_for_timeout(20)
        assert mode["held"] is not None and len(mode["creates"]) == 1
        page.evaluate("window.history.back()")
        pending = page.get_by_role(
            "dialog", name="Destination creation in progress", exact=True
        )
        expect(
            pending.get_by_role("button", name="Discard changes", exact=True)
        ).to_be_disabled()
        audit(page, "pending-creation-protects-context")
        pending.get_by_role("button", name="Keep editing", exact=True).click()
        response = mode["held"].fetch()
        assert response.status == 201
        native = response.json()
        for field in ["created_at", "updated_at"]:
            written = datetime.fromisoformat(native[field])
            if written.tzinfo is None:
                written = written.replace(tzinfo=timezone.utc)
            assert abs(written.timestamp() - time.time()) < 5, (field, native[field])
        mode["saved"].append(native)
        mode["list"] = "fail"
        mode["held"].fulfill(response=response)
        receipt = page.get_by_role("dialog", name="Destination saved", exact=True)
        expect(receipt.get_by_text(native["id"], exact=True)).to_be_visible()
        expect(
            receipt.get_by_role("heading", name="Destination saved", exact=True)
        ).to_be_focused()
        audit(page, "confirmed-configuration-survives-list-failure")
        receipt.screenshot(path=str(out / "saved-receipt.png"))
        receipt.get_by_role("button", name="Close receipt", exact=True).click()
        expect(page.get_by_text("Native paused target", exact=True)).to_be_visible()
        assert (
            req("GET", "/api/audience-destinations/" + native["id"] + "/runs")["runs"]
            == []
        )
        report[
            "one_native_creation_has_no_run_and_retains_receipt_when_list_read_fails"
        ] = True

        mode.update(post="lost", list="native")
        dialog = open_setup("Recover committed target")
        dialog.get_by_label("Webhook URL", exact=True).fill(
            "https://setup-qa.invalid/recover"
        )
        panel = review(dialog)
        panel.get_by_role("button", name="Create destination", exact=True).click()
        expect(panel.get_by_role("alert")).to_contain_text("may already be saved")
        expect(
            panel.get_by_role("button", name="Create destination", exact=True)
        ).to_be_disabled()
        assert len(mode["creates"]) == 2
        panel.get_by_role("button", name="Check saved destinations", exact=True).click()
        expect(panel.get_by_role("alert")).to_contain_text("still unconfirmed")
        assert len(mode["creates"]) == 2
        mode["list"] = "native"
        panel.get_by_role("button", name="Check saved destinations", exact=True).click()
        receipt = page.get_by_role(
            "dialog", name="Matching saved destination found", exact=True
        )
        expect(receipt.get_by_text(mode["saved"][-1]["id"], exact=True)).to_be_visible()
        assert len(mode["creates"]) == 2
        audit(page, "lost-response-recovered-without-second-post")
        receipt.get_by_role("button", name="Close receipt", exact=True).click()
        report[
            "lost_response_and_failed_recovery_read_keep_draft_without_reposting"
        ] = True
        page.screenshot(path=str(out / "audiences-configured.png"))

        mode["post"] = "422"
        dialog = open_setup("Refused configuration")
        dialog.get_by_label("Webhook URL", exact=True).fill(
            "https://setup-qa.invalid/refused"
        )
        panel = review(dialog)
        panel.get_by_role("button", name="Create destination", exact=True).click()
        expect(panel.get_by_role("alert")).to_contain_text("server refused")
        expect(
            panel.get_by_role("button", name="Create destination", exact=True)
        ).to_be_disabled()
        assert len(mode["creates"]) == 3
        panel.get_by_role("button", name="Edit configuration", exact=True).click()
        dialog = page.get_by_role("dialog", name="Configure destination", exact=True)
        expect(dialog.get_by_label("Destination name", exact=True)).to_have_value(
            "Refused configuration"
        )
        expect(dialog.get_by_label("Webhook URL", exact=True)).to_have_value(
            "https://setup-qa.invalid/refused"
        )
        audit(page, "server-refusal-keeps-editable-configuration")
        discard()
        dialog = open_setup("Native paused target")
        dialog.get_by_label("Webhook URL", exact=True).fill(
            "https://setup-qa.invalid/native"
        )
        dialog.get_by_role("button", name="Review destination", exact=True).click()
        expect(dialog.get_by_role("alert")).to_contain_text("already saved")
        assert len(mode["creates"]) == 3
        discard()
        report["server_refusal_and_existing_name_never_silently_repost"] = True

        touch = browser.new_context(
            viewport={"width": 390, "height": 844},
            has_touch=True,
            is_mobile=True,
            reduced_motion="reduce",
        )
        touch.add_init_script(auth)
        phone = touch.new_page()
        phone.on("pageerror", lambda error: report["page_errors"].append(str(error)))
        for width in [320, 390, 768]:
            phone.set_viewport_size({"width": width, "height": 844})
            phone.goto(base + "/audiences?audience=" + audience)
            phone.evaluate('window.OpenGTMTheme.setPreference("dark")')
            phone.get_by_role("button", name="Add destination", exact=True).click()
            dialog = phone.get_by_role(
                "dialog", name="Configure destination", exact=True
            )
            expect(dialog.get_by_label("Destination type", exact=True)).to_be_enabled()
            dialog.get_by_label("Destination name", exact=True).fill("Touch draft")
            dialog.get_by_label("Webhook URL", exact=True).fill(
                "https://setup-qa.invalid/touch"
            )
            dialog.get_by_role("button", name="Review destination", exact=True).click()
            panel = phone.get_by_role(
                "dialog", name="Review new destination", exact=True
            )
            check = panel.locator(".destination-setup-check")
            expect(check).to_be_visible()
            assert check.bounding_box()["height"] >= 44
            assert phone.evaluate("document.documentElement.scrollWidth<=innerWidth")
            audit(phone, "touch-dark-review-" + str(width))
            phone.screenshot(path=str(out / ("review-" + str(width) + ".png")))
            panel.get_by_role("button", name="Cancel", exact=True).click()
            phone.get_by_role(
                "dialog", name="Discard destination setup?", exact=True
            ).get_by_role("button", name="Discard changes", exact=True).click()
            expect(phone.get_by_role("dialog")).to_have_count(0)
        report["touch_review_reflows_and_keeps_large_acknowledgement_targets"] = True
        mode["permission"] = False
        page.reload()
        expect(
            page.get_by_role("button", name="Add destination", exact=True)
        ).to_be_disabled()
        expect(page.get_by_text("Native paused target", exact=True)).to_be_visible()
        audit(page, "view-only-can-read-but-cannot-create")
        report["view_only_keeps_saved_configuration_readable"] = True
        mode["catalog"] = "malformed"
        page.reload()
        expect(page.get_by_text("Status unavailable", exact=True)).to_have_count(2)
        expect(page.get_by_text("beta", exact=True)).to_have_count(0)
        expect(
            page.get_by_role("button", name="Add destination", exact=True)
        ).to_be_disabled()
        audit(page, "saved-destinations-do-not-invent-catalog-status")
        report[
            "saved_destinations_handle_malformed_catalog_without_fake_beta_or_page_error"
        ] = True
        assert not report["page_errors"], report["page_errors"]
        browser.close()
finally:
    save()
    if prior:
        req("POST", "/api/workspaces/switch", {"workspace_id": prior})
print(
    json.dumps({"axe_states": len(report["axe"]), "page_errors": report["page_errors"]})
)
